package m3u

import (
	"crypto/md5"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Patterns used for every channel. They used to be compiled once per
// channel, which took most of the parse time of a large playlist.
var (
	exceptForParameter   = regexp.MustCompile(`[a-z-A-Z&=]*(".*?")`)
	exceptForChannelName = regexp.MustCompile(`,([^\n]*|,[^\r]*)`)

	// Applied once to the whole playlist before it is split into channels.
	// One pass gives the same result as removing ":-1" and then replacing
	// ' with " (neither replacement can create a match for the other).
	playlistReplacer = strings.NewReplacer(":-1", "", "'", `"`)
)

// MakeInterfaceFromM3U parses an extended M3U playlist. Every channel is
// returned as a map[string]string.
//
// Playlists over 10 MB used to go to a second parser that dropped every
// channel (it expected two URL lines per #EXTINF entry), so large playlists
// were saved and loaded as empty. All sizes now use this parser.
func MakeInterfaceFromM3U(byteStream []byte) (allChannels []interface{}, err error) {

	var content = string(byteStream)

	// Check if the content is a valid M3U file
	if strings.Contains(content, "#EXT-X-TARGETDURATION") || strings.Contains(content, "#EXT-X-MEDIA-SEQUENCE") {
		err = errors.New("Invalid M3U file, an extended M3U file is required.")
		return
	}

	if !strings.Contains(content, "#EXTM3U") {
		err = errors.New("Invalid M3U file, an extended M3U file is required.")
		return
	}

	content = playlistReplacer.Replace(content)

	var channels = strings.Split(content, "#EXTINF")[1:]

	for _, channel := range channels {
		var stream = parseMetaData(channel)
		if len(stream) > 0 {
			allChannels = append(allChannels, stream)
		}
	}

	return
}

// parseMetaData parses one channel: the rest of its #EXTINF line and the
// lines up to the next #EXTINF.
func parseMetaData(channel string) (stream map[string]string) {

	var channelName string

	stream = make(map[string]string)

	var lines = strings.Split(strings.Replace(channel, "\r\n", "\n", -1), "\n")

	// Remove lines starting with # and empty lines
	var kept = lines[:0]
	for _, line := range lines {
		if len(line) > 0 && line[0] != '#' {
			kept = append(kept, line)
		}
	}
	lines = kept

	// URL is always on the second line after #EXTINF
	if len(lines) < 2 {
		return
	}

	// Kept strings are copied: a substring would keep the whole playlist in
	// memory for as long as the channel is.
	stream["url"] = strings.Clone(strings.Trim(lines[1], "\r\n"))

	// Parse the first line (#EXTINF line) for metadata
	var value string
	var extinf = lines[0]
	var streamParameter = exceptForParameter.FindAllString(extinf, -1)

	for _, p := range streamParameter {
		extinf = strings.Replace(extinf, p, "", 1)
		p = strings.Replace(p, `"`, "", -1)
		var parameter = strings.SplitN(p, "=", 2)
		if len(parameter) == 2 {
			// Save TVG Key in lowercase
			if strings.Contains(parameter[0], "tvg") {
				stream[strings.ToLower(parameter[0])] = parameter[1]
			} else {
				stream[parameter[0]] = parameter[1]
			}

			// Do not pass URLs to the filter function
			if !strings.Contains(parameter[1], "://") && len(parameter[1]) > 0 {
				value = value + parameter[1] + " "
			}
		}
	}

	// Parse channel name
	var name = exceptForChannelName.FindAllString(extinf, 1)

	if len(name) > 0 {
		channelName = name[0]
		channelName = strings.Replace(channelName, `,`, "", 1)
		channelName = strings.TrimRight(channelName, "\r\n")
		channelName = strings.TrimRight(channelName, " ")
	}

	if len(channelName) == 0 {
		if v, ok := stream["tvg-name"]; ok {
			channelName = v
		}
	}

	// Only generate a new tvg-id if it's missing, blank, or (no tvg-id)
	if stream["tvg-id"] == "" || stream["tvg-id"] == "(no tvg-id)" {
		hash := md5.Sum([]byte(stream["url"]))
		// The "threadfin-" prefix is kept from Threadfin (which ProxFin is
		// based on): the id identifies the channel in existing databases
		// and in the guides of DVR clients.
		stream["tvg-id"] = fmt.Sprintf("threadfin-%x", hash)
	}

	channelName = strings.TrimRight(channelName, " ")

	// A channel without a name is kept, without name, values or UUID
	if len(channelName) == 0 {
		return
	}

	channelName = strings.Clone(channelName)
	stream["name"] = channelName
	value = value + channelName
	stream["_values"] = value

	// The tvg-name is the unique ID of the stream
	if key, ok := uuidKey(stream); ok {
		stream["_uuid.key"] = key
		stream["_uuid.value"] = stream[key]
	}

	return
}

// uuidKey returns the key holding the stream's unique ID: any key that
// contains "tvg-name" (in any case). "tvg-name" itself is preferred; if only
// other such keys exist, the first in sorted order is used (the choice used
// to depend on map iteration order).
func uuidKey(stream map[string]string) (string, bool) {

	if _, ok := stream["tvg-name"]; ok {
		return "tvg-name", true
	}

	var keys []string
	for key := range stream {
		if strings.Contains(strings.ToLower(key), "tvg-name") {
			keys = append(keys, key)
		}
	}

	if len(keys) == 0 {
		return "", false
	}

	sort.Strings(keys)
	return keys[0], true
}
