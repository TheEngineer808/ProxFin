package src

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	m3u "threadfin/src/internal/m3u-parser"
)

// Playlisten parsen
func parsePlaylist(filename, fileType string) (channels []interface{}, err error) {

	content, err := readByteFromFile(filename)
	var id = strings.TrimSuffix(getFilenameFromPath(filename), path.Ext(getFilenameFromPath(filename)))
	var playlistName = getProviderParameter(id, fileType, "name")

	if err == nil {

		switch fileType {
		case "m3u":
			channels, err = m3u.MakeInterfaceFromM3U(content)
		case "hdhr":
			channels, err = makeInteraceFromHDHR(content, playlistName, id)
		}

	}

	return
}

// Patterns for the conditions of a filter rule: "!{...}" (exclude) and
// "{...}" (include).
var (
	filterExcludePattern = regexp.MustCompile(`!+[{]+[^.]+[}]`)
	filterIncludePattern = regexp.MustCompile(`[{]+[^.]+[}]`)
)

// preparedFilter is a filter rule split into its parts once per build.
// The rule used to be parsed, and both patterns compiled, for every stream
// and every filter.
type preparedFilter struct {
	caseSensitive bool
	liveEvent     bool
	filterType    string
	rule          string
	exclude       string
	include       string
}

func prepareFilters(filters []Filter) (prepared []preparedFilter) {

	prepared = make([]preparedFilter, 0, len(filters))

	for _, filter := range filters {

		if filter.Rule == "" {
			continue
		}

		var exclude, include string

		// Unerwünschte Streams !{DEU}
		val := filterExcludePattern.FindStringSubmatch(filter.Rule)

		if len(val) == 1 {

			exclude = val[0][2 : len(val[0])-1]
			filter.Rule = strings.Replace(filter.Rule, " "+val[0], "", -1)
			filter.Rule = strings.Replace(filter.Rule, val[0], "", -1)

		}

		// Muss zusätzlich erfüllt sein {DEU}
		val = filterIncludePattern.FindStringSubmatch(filter.Rule)

		if len(val) == 1 {

			include = val[0][1 : len(val[0])-1]
			filter.Rule = strings.Replace(filter.Rule, " "+val[0], "", -1)
			filter.Rule = strings.Replace(filter.Rule, val[0], "", -1)

		}

		if !filter.CaseSensitive {
			filter.Rule = strings.ToLower(filter.Rule)
			exclude = strings.ToLower(exclude)
			include = strings.ToLower(include)
		}

		prepared = append(prepared, preparedFilter{
			caseSensitive: filter.CaseSensitive,
			liveEvent:     filter.LiveEvent,
			filterType:    filter.Type,
			rule:          filter.Rule,
			exclude:       exclude,
			include:       include,
		})

	}

	return
}

// Streams filtern
func filterThisStream(s interface{}) (status bool, liveEvent bool) {
	return matchStream(s.(map[string]string), prepareFilters(Data.Filter))
}

// matchStream reports whether the stream passes the filters, and the live
// event setting of the filter that decided (of the last filter if none
// matched).
func matchStream(stream map[string]string, filters []preparedFilter) (status bool, liveEvent bool) {

	if len(filters) == 0 {
		return false, false
	}

	var group = stream["group-title"]
	var name = stream["name"]
	var streamValues = strings.Replace(stream["_values"], "\r", "", -1)

	// lower-case variants, computed once when a filter needs them
	var lowered bool
	var lowerGroup, lowerName, lowerValues string

	for _, filter := range filters {

		liveEvent = filter.liveEvent

		var g, n, v = group, name, streamValues

		if !filter.caseSensitive {
			if !lowered {
				lowerGroup = strings.ToLower(group)
				lowerName = strings.ToLower(name)
				lowerValues = strings.ToLower(streamValues)
				lowered = true
			}
			g, n, v = lowerGroup, lowerName, lowerValues
		}

		var search string
		var match = false

		switch filter.filterType {

		case "group-title":
			search = n

			if g == filter.rule {
				match = true
			}

		case "custom-filter":
			search = v
			if strings.Contains(search, filter.rule) {
				match = true
			}
		}

		if match {

			if len(filter.exclude) > 0 {
				if !checkConditions(search, filter.exclude, "exclude") {
					return false, liveEvent
				}
			}

			if len(filter.include) > 0 {
				if !checkConditions(search, filter.include, "include") {
					return false, liveEvent
				}
			}

			return true, liveEvent

		}

	}

	return false, liveEvent
}

// Bedingungen für den Filter
func checkConditions(streamValues, conditions, coType string) (status bool) {

	switch coType {

	case "exclude":
		status = true

	case "include":
		status = false

	}

	conditions = strings.Replace(conditions, ", ", ",", -1)
	conditions = strings.Replace(conditions, " ,", ",", -1)

	var keys = strings.Split(conditions, ",")

	for _, key := range keys {

		if strings.Contains(streamValues, key) {

			switch coType {

			case "exclude":
				return false

			case "include":
				return true

			}

		}

	}

	return
}

// Threadfin M3U Datei erstellen
func buildM3U(groups []string) (m3u string, err error) {

	var imgc = Data.Cache.Images
	// Preserve every active channel as a distinct entry by iteration order, not keyed by number
	type channelEntry struct {
		idx int
		ch  XEPGChannelStruct
	}
	var entries []channelEntry

	// Build a map of group -> expectedCount from Data.Playlist.M3U.Groups.Text (format: "Group (N)")
	expectedGroupCount := make(map[string]int)
	for _, label := range Data.Playlist.M3U.Groups.Text {
		// label example: "nfl (42)"
		open := strings.LastIndex(label, " (")
		close := strings.LastIndex(label, ")")
		if open > 0 && close > open+2 {
			name := label[:open]
			countStr := label[open+2 : close]
			if n, e := strconv.Atoi(countStr); e == nil {
				expectedGroupCount[name] = n
			}
		}
	}

	// Count deactivated channels per group
	deactivatedPerGroup := make(map[string]int)
	for _, dxc := range Data.XEPG.Channels {
		var ch XEPGChannelStruct
		if err := json.Unmarshal([]byte(mapToJSON(dxc)), &ch); err == nil {
			group := ch.XGroupTitle
			if ch.XCategory != "" {
				group = ch.XCategory
			}
			if group == "" {
				group = ch.GroupTitle
			}
			if !ch.XActive || ch.XHideChannel {
				deactivatedPerGroup[group] = deactivatedPerGroup[group] + 1
			}
		}
	}

	for _, dxc := range Data.XEPG.Channels {
		var xepgChannel XEPGChannelStruct
		err := json.Unmarshal([]byte(mapToJSON(dxc)), &xepgChannel)
		if err == nil {
			if xepgChannel.TvgName == "" {
				xepgChannel.TvgName = xepgChannel.Name
			}
			if xepgChannel.XActive && !xepgChannel.XHideChannel {
				if len(groups) > 0 {

					if indexOfString(xepgChannel.XGroupTitle, groups) == -1 {
						goto Done
					}

				}
				entries = append(entries, channelEntry{idx: len(entries), ch: xepgChannel})
			}
		}

	Done:
	}

	// Prepare header
	var xmltvURL = fmt.Sprintf("%s://%s/xmltv/threadfin.xml", System.ServerProtocol.XML, System.Domain)
	if Settings.ForceHttps && Settings.HttpsThreadfinDomain != "" {
		xmltvURL = fmt.Sprintf("https://%s/xmltv/threadfin.xml", Settings.HttpsThreadfinDomain)
	}
	header := fmt.Sprintf(`#EXTM3U url-tvg="%s" x-tvg-url="%s"`+"\n", xmltvURL, xmltvURL)

	// If generating the full file, stream to disk to avoid huge in-memory strings
	var writer *bufio.Writer
	var file *os.File
	if len(groups) == 0 {
		filename := System.Folder.Data + "threadfin.m3u"
		file, err = os.Create(filename)
		if err != nil {
			return "", err
		}
		defer file.Close()
		writer = bufio.NewWriterSize(file, 1<<20) // 1MB buffer
		if _, err = writer.WriteString(header); err != nil {
			return "", err
		}
	} else {
		m3u = header
	}

	// Sort entries by tvg-chno numerically
	sort.Slice(entries, func(i, j int) bool {
		chI := entries[i].ch.TvgChno
		chJ := entries[j].ch.TvgChno

		// Try to parse as numbers for proper numeric sorting
		numI, errI := strconv.ParseFloat(chI, 64)
		numJ, errJ := strconv.ParseFloat(chJ, 64)

		// If both are numbers, sort numerically
		if errI == nil && errJ == nil {
			return numI < numJ
		}

		// If one is a number and other isn't, number comes first
		if errI == nil && errJ != nil {
			return true
		}
		if errI != nil && errJ == nil {
			return false
		}

		// If both are strings, sort alphabetically
		return chI < chJ
	})

	// Avoid duplicate exact stream URLs within the same group and cap per-group by expected minus deactivated
	seenURLInGroup := make(map[string]struct{})
	emittedGroupCount := make(map[string]int)
	for _, e := range entries {
		var channel = e.ch

		group := channel.XGroupTitle
		if channel.XCategory != "" {
			group = channel.XCategory
		}
		if group == "" {
			group = channel.GroupTitle
		}

		// Determine allowed active count = expected - deactivated
		if expected, ok := expectedGroupCount[group]; ok {
			allowed := expected - deactivatedPerGroup[group]
			if allowed < 0 {
				allowed = 0
			}
			if emittedGroupCount[group] >= allowed {
				continue
			}
		}

        // Disabling so not to rewrite stream to https domain when disable stream from https set
		if Settings.ForceHttps && Settings.HttpsThreadfinDomain != "" && Settings.ExcludeStreamHttps == false {
			u, err := url.Parse(channel.URL)
			if err == nil {
				u.Scheme = "https"
				host_split := strings.Split(u.Host, ":")
				if len(host_split) > 0 {
					u.Host = host_split[0]
				}
				if u.RawQuery != "" {
					channel.URL = fmt.Sprintf("https://%s:%d%s?%s", u.Host, Settings.HttpsPort, u.Path, u.RawQuery)
				} else {
					channel.URL = fmt.Sprintf("https://%s:%d%s", u.Host, Settings.HttpsPort, u.Path)
				}
			}
	    }

		logo := ""
		if channel.TvgLogo != "" {
			logo = imgc.Image.GetURL(channel.TvgLogo, Settings.HttpThreadfinDomain, Settings.Port, Settings.ForceHttps, Settings.HttpsPort, Settings.HttpsThreadfinDomain)
		}
		var parameter = fmt.Sprintf(`#EXTINF:0 channelID="%s" tvg-chno="%s" tvg-name="%s" tvg-id="%s" tvg-logo="%s" group-title="%s",%s`+"\n", channel.XEPG, channel.XChannelID, channel.XName, channel.XChannelID, logo, group, channel.XName)
		var stream, err = createStreamingURL("M3U", channel.FileM3UID, channel.XChannelID, channel.XName, channel.URL, channel.BackupChannel1, channel.BackupChannel2, channel.BackupChannel3)
		if err == nil {
			key := group + "|" + stream
			if _, ok := seenURLInGroup[key]; ok {
				continue
			}
			seenURLInGroup[key] = struct{}{}
			if writer != nil {
				if _, err = writer.WriteString(parameter); err != nil {
					return "", err
				}
				if _, err = writer.WriteString(stream + "\n"); err != nil {
					return "", err
				}
			} else {
				m3u = m3u + parameter + stream + "\n"
			}
			emittedGroupCount[group] = emittedGroupCount[group] + 1
		}

	}

	if writer != nil {
		if err = writer.Flush(); err != nil {
			return "", err
		}
	}

	return
}

func probeChannel(request RequestStruct) (string, string, string, error) {

	ffmpegPath := Settings.FFmpegPath
	ffprobePath := strings.Replace(ffmpegPath, "ffmpeg", "ffprobe", 1)

	cmd := exec.Command(ffprobePath, "-v", "error", "-show_streams", "-of", "json", request.ProbeURL)
	output, err := cmd.Output()
	if err != nil {
		return "", "", "", fmt.Errorf("failed to execute ffprobe: %v", err)
	}

	var ffprobeOutput FFProbeOutput
	err = json.Unmarshal(output, &ffprobeOutput)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to parse ffprobe output: %v", err)
	}

	var resolution, frameRate, audioChannels string

	for _, stream := range ffprobeOutput.Streams {
		if stream.CodecType == "video" {
			resolution = fmt.Sprintf("%dp", stream.Height)
			frameRateParts := strings.Split(stream.RFrameRate, "/")
			if len(frameRateParts) == 2 {
				frameRate = fmt.Sprintf("%d", parseFrameRate(frameRateParts))
			} else {
				frameRate = stream.RFrameRate
			}
		}
		if stream.CodecType == "audio" {
			audioChannels = stream.ChannelLayout
			if audioChannels == "" {
				switch stream.Channels {
				case 1:
					audioChannels = "Mono"
				case 2:
					audioChannels = "Stereo"
				case 6:
					audioChannels = "5.1"
				case 8:
					audioChannels = "7.1"
				default:
					audioChannels = fmt.Sprintf("%d channels", stream.Channels)
				}
			}
		}
	}

	return resolution, frameRate, audioChannels, nil
}

func parseFrameRate(parts []string) int {
	numerator, denom := 1, 1
	fmt.Sscanf(parts[0], "%d", &numerator)
	fmt.Sscanf(parts[1], "%d", &denom)
	if denom == 0 {
		return 0
	}
	return int(math.Round(float64(numerator) / float64(denom)))
}
