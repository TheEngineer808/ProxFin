package src

/*
  Render tuner-limit image as video [ffmpeg]
  -loop 1 -i stream-limit.jpg -c:v libx264 -t 1 -pix_fmt yuv420p -vf scale=1920:1080  stream-limit.ts
*/

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/avfs/avfs/vfs/memfs"
)

type BackupStream struct {
	PlaylistID string
	URL        string
}

func getActiveClientCount() (count int) {
	count = 0
	cleanUpStaleClients() // Ensure stale clients are removed first

	BufferInformation.Range(func(key, value interface{}) bool {
		playlist, ok := value.(Playlist)
		if !ok {
			fmt.Printf("Invalid type assertion for playlist: %v\n", value)
			return true
		}

		for clientID, client := range playlist.Clients {
			if client.Connection < 0 {
				fmt.Printf("Client ID %d has negative connections: %d. Resetting to 0.\n", clientID, client.Connection)
				client.Connection = 0
				playlist.Clients[clientID] = client
				BufferInformation.Store(key, playlist)
			}
			if client.Connection > 1 {
				fmt.Printf("Client ID %d has suspiciously high connections: %d. Resetting to 1.\n", clientID, client.Connection)
				client.Connection = 1
				playlist.Clients[clientID] = client
				BufferInformation.Store(key, playlist)
			}
			count += client.Connection
		}

		fmt.Printf("Playlist %s has %d active clients\n", playlist.PlaylistID, len(playlist.Clients))
		return true
	})

	return count
}

func getActivePlaylistCount() (count int) {
	count = 0
	BufferInformation.Range(func(key, value interface{}) bool {
		count++
		return true
	})
	return count
}

func cleanUpStaleClients() {
	BufferInformation.Range(func(key, value interface{}) bool {
		playlist, ok := value.(Playlist)
		if !ok {
			fmt.Printf("Invalid type assertion for playlist: %v\n", value)
			return true
		}

		for clientID, client := range playlist.Clients {
			if client.Connection <= 0 {
				fmt.Printf("Removing stale client ID %d from playlist %s\n", clientID, playlist.PlaylistID)
				delete(playlist.Clients, clientID)
			}
		}
		BufferInformation.Store(key, playlist)
		return true
	})
}

func getClientIP(r *http.Request) string {
	// Check the X-Forwarded-For header first
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded != "" {
		// X-Forwarded-For may contain multiple IP addresses; return the first one
		ips := strings.Split(forwarded, ",")
		return strings.TrimSpace(ips[0])
	}

	// Check the X-Real-IP header next
	realIP := r.Header.Get("X-Real-IP")
	if realIP != "" {
		return realIP
	}

	// Fallback to RemoteAddr
	ip := r.RemoteAddr
	if strings.Contains(ip, ":") {
		// Remove port if present
		ip = strings.Split(ip, ":")[0]
	}

	return ip
}

func createStreamID(stream map[int]ThisStream, ip, userAgent string) (streamID int) {
	streamID = 0
	uniqueIdentifier := fmt.Sprintf("%s-%s", ip, userAgent)

	for i := 0; i <= len(stream); i++ {
		if _, ok := stream[i]; !ok {
			streamID = i
			break
		}
	}

	if _, ok := stream[streamID]; ok && stream[streamID].ClientID == uniqueIdentifier {
		// Return the same ID if the combination already exists
		return streamID
	}

	return
}

func bufferingStream(playlistID string, streamingURL string, backupStream1 *BackupStream, backupStream2 *BackupStream, backupStream3 *BackupStream, channelName string, w http.ResponseWriter, r *http.Request) {

	time.Sleep(time.Duration(Settings.BufferTimeout) * time.Millisecond)

	var playlist Playlist
	var client ThisClient
	var stream ThisStream
	var streamID int
	var debug string
	var timeOut = 0
	var newStream = true

	//w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Check whether the playlist is already in use
	Lock.Lock()
	if p, ok := BufferInformation.Load(playlistID); !ok {
		Lock.Unlock() // Unlock early if not found
		var playlistType string

		// Playlist is not yet in use, create default values for the playlist
		playlist.Folder = System.Folder.Temp + playlistID + string(os.PathSeparator)
		playlist.PlaylistID = playlistID
		playlist.Streams = make(map[int]ThisStream)
		playlist.Clients = make(map[int]ThisClient)

		err := checkVFSFolder(playlist.Folder, bufferVFS)
		if err != nil {
			ShowError(err, 000)
			httpStatusError(w, r, 404)
			return
		}

		switch playlist.PlaylistID[0:1] {

		case "M":
			playlistType = "m3u"

		case "H":
			playlistType = "hdhr"

		}

		var playListBuffer string
		systemMutex.Lock()
		playListInterface := Settings.Files.M3U[playlistID]
		if playListInterface == nil {
			playListInterface = Settings.Files.HDHR[playlistID]
		}
		if playListMap, ok := playListInterface.(map[string]interface{}); ok {
			if buffer, ok := playListMap["buffer"].(string); ok {
				playListBuffer = buffer
			} else {
				playListBuffer = "-"
			}
		}
		systemMutex.Unlock()

		playlist.Buffer = playListBuffer

		playlist.Tuner = getTuner(playlistID, playlistType)

		playlist.PlaylistName = getProviderParameter(playlist.PlaylistID, playlistType, "name")

		playlist.HttpProxyIP = getProviderParameter(playlist.PlaylistID, playlistType, "http_proxy.ip")
		playlist.HttpProxyPort = getProviderParameter(playlist.PlaylistID, playlistType, "http_proxy.port")

		playlist.HttpUserOrigin = getProviderParameter(playlist.PlaylistID, playlistType, "http_headers.origin")
		playlist.HttpUserReferer = getProviderParameter(playlist.PlaylistID, playlistType, "http_headers.referer")

		// Create default values for the stream
		streamID = createStreamID(playlist.Streams, getClientIP(r), r.UserAgent())

		client.Connection += 1

		stream.URL = streamingURL
		stream.BackupChannel1 = backupStream1
		stream.BackupChannel2 = backupStream2
		stream.BackupChannel3 = backupStream3
		stream.ChannelName = channelName
		stream.Status = false

		playlist.Streams[streamID] = stream
		playlist.Clients[streamID] = client

		Lock.Lock()
		BufferInformation.Store(playlistID, playlist)
		Lock.Unlock()

	} else {
		playlist = p.(Playlist)
		Lock.Unlock()

		// Playlist is already used for streaming
		// Check if the URL is already streaming from another client.
		for id := range playlist.Streams {

			stream = playlist.Streams[id]
			client = playlist.Clients[id]

			stream.BackupChannel1 = backupStream1
			stream.BackupChannel2 = backupStream2
			stream.BackupChannel3 = backupStream3
			stream.ChannelName = channelName
			stream.Status = false

			if streamingURL == stream.URL {

				streamID = id
				newStream = false
				client.Connection += 1

				playlist.Clients[streamID] = client

				Lock.Lock()
				BufferInformation.Store(playlistID, playlist)
				Lock.Unlock()

				debug = fmt.Sprintf("Restream Status:Playlist: %s - Channel: %s - Connections: %d", playlist.PlaylistName, stream.ChannelName, client.Connection)

				showDebug(debug, 1)

				if c, ok := BufferClients.Load(playlistID + stream.MD5); ok {

					var clients = c.(ClientConnection)
					clients.Connection = client.Connection

					showInfo(fmt.Sprintf("Streaming Status:Channel: %s (Clients: %d)", stream.ChannelName, clients.Connection))

					BufferClients.Store(playlistID+stream.MD5, clients)

				}

				break
			}

		}

		// New stream for an already active playlist
		if newStream {

			// Check if the playlist allows another stream (Tuner)
			if len(playlist.Streams) >= playlist.Tuner {
				// If there are backup URLs, use them
				if backupStream1 != nil {
					bufferingStream(backupStream1.PlaylistID, backupStream1.URL, nil, backupStream2, backupStream3, channelName, w, r)
				} else if backupStream2 != nil && backupStream1 == nil {
					bufferingStream(backupStream2.PlaylistID, backupStream2.URL, nil, nil, backupStream3, channelName, w, r)
				} else if backupStream3 != nil && backupStream1 == nil && backupStream2 == nil {
					bufferingStream(backupStream3.PlaylistID, backupStream3.URL, nil, nil, nil, channelName, w, r)
				}

				showInfo(fmt.Sprintf("Streaming Status:Playlist: %s - No new connections available. Tuner = %d", playlist.PlaylistName, playlist.Tuner))

				if value, ok := webUI["html/video/stream-limit.ts"]; ok {

					content := GetHTMLString(value.(string))

					w.WriteHeader(200)
					w.Header().Set("Content-type", "video/mpeg")
					w.Header().Set("Content-Length:", "0")

					for i := 1; i < 60; i++ {
						_ = i
						w.Write([]byte(content))
						time.Sleep(time.Duration(500) * time.Millisecond)
					}

					return
				}

				return
			}

			// Playlist allows another stream (Tuner limit not yet reached)
			// Create default values for the stream
			stream = ThisStream{}
			client = ThisClient{}

			streamID = createStreamID(playlist.Streams, getClientIP(r), r.UserAgent())

			client.Connection = 1
			stream.URL = streamingURL
			stream.ChannelName = channelName
			stream.Status = false
			stream.BackupChannel1 = backupStream1
			stream.BackupChannel2 = backupStream2
			stream.BackupChannel3 = backupStream3

			playlist.Streams[streamID] = stream
			playlist.Clients[streamID] = client

			Lock.Lock()
			BufferInformation.Store(playlistID, playlist)
			Lock.Unlock()

		}

	}

	// Check whether the stream is already being played by another client
	if !playlist.Streams[streamID].Status && newStream {

		// New buffer is needed
		stream = playlist.Streams[streamID]
		stream.MD5 = getMD5(streamingURL)
		stream.Folder = playlist.Folder + stream.MD5 + string(os.PathSeparator)
		stream.PlaylistID = playlistID
		stream.PlaylistName = playlist.PlaylistName
		stream.BackupChannel1 = backupStream1
		stream.BackupChannel2 = backupStream2
		stream.BackupChannel3 = backupStream3

		playlist.Streams[streamID] = stream

		Lock.Lock()
		BufferInformation.Store(playlistID, playlist)
		Lock.Unlock()

		// Register the viewer before the buffer process starts, so the
		// buffer's "any viewers left?" check never runs before this exists.
		var clients ClientConnection
		clients.Connection = 1
		BufferClients.Store(playlistID+stream.MD5, clients)

		switch playlist.Buffer {

		case "ffmpeg", "vlc":
			go thirdPartyBuffer(streamID, playlistID, false, 0)

		default:
			break

		}

		showInfo(fmt.Sprintf("Streaming Status 1:Playlist: %s - Tuner: %d / %d", playlist.PlaylistName, len(playlist.Streams), playlist.Tuner))

	}

	// Headers must be set before WriteHeader; changes made afterwards are ignored.
	// video/mpeg matches what a real HDHomeRun tuner sends.
	w.Header().Set("Content-Type", "video/mpeg")
	w.WriteHeader(200)

	for { //Loop 1: Wait until the first segment has been downloaded through the buffer

		if p, ok := BufferInformation.Load(playlistID); ok {

			var playlist = p.(Playlist)

			if stream, ok := playlist.Streams[streamID]; ok {

				if !stream.Status {

					timeOut++

					time.Sleep(time.Duration(100) * time.Millisecond)

					if c, ok := BufferClients.Load(playlistID + stream.MD5); ok {

						var clients = c.(ClientConnection)

						if clients.Error != nil || (timeOut > 200 && (playlist.Streams[streamID].BackupChannel1 == nil && playlist.Streams[streamID].BackupChannel2 == nil && playlist.Streams[streamID].BackupChannel3 == nil)) {
							killClientConnection(streamID, stream.PlaylistID, false)
							return
						}

					}

					continue
				}

				var oldSegments []string
				var lastSent int64 // number of the newest segment sent to this viewer

				for { // Loop 2: Temporary files are present, data can be sent to the client

					// Monitor HTTP client connection

					ctx := r.Context()
					if ok {

						select {

						case <-ctx.Done():
							killClientConnection(streamID, playlistID, false)
							return

						default:
							if c, ok := BufferClients.Load(playlistID + stream.MD5); ok {

								var clients = c.(ClientConnection)
								if clients.Error != nil {
									ShowError(clients.Error, 0)
									killClientConnection(streamID, playlistID, false)
									return
								}

							} else {

								return

							}

						}

					}

					if _, err := bufferVFS.Stat(stream.Folder); fsIsNotExistErr(err) {
						killClientConnection(streamID, playlistID, false)
						return
					}

					var tmpFiles = getBufTmpFiles(stream.Folder, &lastSent)

					for _, f := range tmpFiles {

						if _, err := bufferVFS.Stat(stream.Folder); fsIsNotExistErr(err) {
							killClientConnection(streamID, playlistID, false)
							return
						}

						oldSegments = append(oldSegments, f)

						var fileName = stream.Folder + f

						file, err := bufferVFS.Open(fileName)
						if err != nil {
							debug = fmt.Sprintf("Buffer Open (%s)", fileName)
							showDebug(debug, 2)
							killClientConnection(streamID, playlistID, false)
							return
						}

						debug = fmt.Sprintf("Buffer Status:Send to client (%s)", fileName)
						showDebug(debug, 2)

						// Stream the segment through a pooled buffer instead of
						// allocating a segment-sized slice per viewer per segment.
						_, err = io.Copy(w, file)
						file.Close()

						if err != nil {
							killClientConnection(streamID, playlistID, false)
							return
						}

						if len(oldSegments) > 21 {

							var fileToRemove = stream.Folder + oldSegments[0]
							if err = bufferVFS.RemoveAll(getPlatformFile(fileToRemove)); err != nil {
								ShowError(err, 4007)
							}
							oldSegments = append(oldSegments[:0], oldSegments[0+1:]...)

						}

					}

					if len(tmpFiles) == 0 {
						time.Sleep(time.Duration(100) * time.Millisecond)
					}

				} // End Loop 2

			} else {

				// Stream not found
				showDebug("Streaming Status:Stream not found. Killing Connection", 3)
				killClientConnection(streamID, stream.PlaylistID, false)
				showInfo(fmt.Sprintf("Streaming Status:Playlist: %s - Tuner: %d / %d", playlist.PlaylistName, len(playlist.Streams), playlist.Tuner))
				return

			}

		} // End BufferInformation

	} // End Loop 1

}

// getBufTmpFiles returns, in order, the completed segments in tmpFolder that
// are newer than *lastSent, and advances *lastSent. The newest file is still
// being written by the buffer process and is never returned.
//
// Tracking only the last segment number (instead of every name sent) keeps
// this O(files in folder) however long the stream runs.
func getBufTmpFiles(tmpFolder string, lastSent *int64) (tmpFiles []string) {

	var fileIDs []int64

	if _, err := bufferVFS.Stat(tmpFolder); !fsIsNotExistErr(err) {

		files, err := bufferVFS.ReadDir(getPlatformPath(tmpFolder))
		if err != nil {
			ShowError(err, 000)
			return
		}

		if len(files) > 2 {

			for _, file := range files {

				var fileID = strings.TrimSuffix(file.Name(), ".ts")
				var f, err = strconv.ParseInt(fileID, 10, 64)

				if err == nil {
					fileIDs = append(fileIDs, f)
				}

			}

			if len(fileIDs) == 0 {
				return
			}

			sort.Slice(fileIDs, func(i, j int) bool { return fileIDs[i] < fileIDs[j] })
			fileIDs = fileIDs[:len(fileIDs)-1]

			for _, id := range fileIDs {

				if id > *lastSent {
					tmpFiles = append(tmpFiles, fmt.Sprintf("%d.ts", id))
					*lastSent = id
				}

			}

		}

	}

	return
}

func killClientConnection(streamID int, playlistID string, force bool) {
	Lock.Lock()
	defer Lock.Unlock()

	if p, ok := BufferInformation.Load(playlistID); ok {
		var playlist = p.(Playlist)

		if force {
			delete(playlist.Streams, streamID)
			if len(playlist.Streams) == 0 {
				BufferInformation.Delete(playlistID)
			} else {
				BufferInformation.Store(playlistID, playlist)
			}
			showInfo(fmt.Sprintf("Streaming Status: Playlist: %s - Tuner: %d / %d", playlist.PlaylistName, len(playlist.Streams), playlist.Tuner))
			return
		}

		if stream, ok := playlist.Streams[streamID]; ok {
			client := playlist.Clients[streamID]

			if c, ok := BufferClients.Load(playlistID + stream.MD5); ok {
				var clients = c.(ClientConnection)
				clients.Connection--
				client.Connection--

				// Ensure client connections cannot go below zero
				if client.Connection < 0 {
					client.Connection = 0
				}
				if clients.Connection < 0 {
					clients.Connection = 0
				}

				playlist.Clients[streamID] = client
				BufferClients.Store(playlistID+stream.MD5, clients)

				showInfo(fmt.Sprintf("Streaming Status: Channel: %s (Clients: %d)", stream.ChannelName, clients.Connection))

				if clients.Connection <= 0 {
					BufferClients.Delete(playlistID + stream.MD5)
					delete(playlist.Streams, streamID)
					delete(playlist.Clients, streamID)

					if len(playlist.Streams) == 0 {
						BufferInformation.Delete(playlistID)
					} else {
						BufferInformation.Store(playlistID, playlist)
					}
				} else {
					BufferInformation.Store(playlistID, playlist)
				}

				if len(playlist.Streams) > 0 {
					showInfo(fmt.Sprintf("Streaming Status: Playlist: %s - Tuner: %d / %d", playlist.PlaylistName, len(playlist.Streams), playlist.Tuner))
				}
			}
		}
	}
}

func clientConnection(stream ThisStream) (status bool) {

	status = true
	Lock.Lock()
	defer Lock.Unlock()

	if _, ok := BufferClients.Load(stream.PlaylistID + stream.MD5); !ok {

		var debug = fmt.Sprintf("Streaming Status:Remove temporary files (%s)", stream.Folder)
		showDebug(debug, 1)

		status = false

		debug = fmt.Sprintf("Remove tmp folder:%s", stream.Folder)
		showDebug(debug, 1)

		if err := bufferVFS.RemoveAll(stream.Folder); err != nil {
			ShowError(err, 4005)
		}

		if p, ok := BufferInformation.Load(stream.PlaylistID); !ok {

			showInfo(fmt.Sprintf("Streaming Status:Channel: %s - No client is using this channel anymore. Streaming Server connection has ended", stream.ChannelName))

			if p != nil {
				var playlist = p.(Playlist)

				showInfo(fmt.Sprintf("Streaming Status:Playlist: %s - Tuner: %d / %d", playlist.PlaylistName, len(playlist.Streams), playlist.Tuner))

				if len(playlist.Streams) <= 0 {
					BufferInformation.Delete(stream.PlaylistID)
				}
			}

		}

		status = false

	}

	return
}

func parseM3U8(stream *ThisStream) (err error) {

	var debug string
	var noNewSegment = false
	var lastSegmentDuration float64
	var segment Segment
	var m3u8Segments []Segment
	var sequence int64

	stream.DynamicBandwidth = false

	debug = fmt.Sprintf(`M3U8 Playlist:`+"\n"+`%s`, stream.Body)
	showDebug(debug, 3)

	var getBandwidth = func(line string) int {

		var infos = strings.Split(line, ",")

		for _, info := range infos {

			if strings.Contains(info, "BANDWIDTH=") {

				var bandwidth = strings.Replace(info, "BANDWIDTH=", "", -1)
				n, err := strconv.Atoi(bandwidth)
				if err == nil {
					return n
				}

			}

		}

		return 0
	}

	var parseParameter = func(line string, segment *Segment) (err error) {

		line = strings.Trim(line, "\r\n")

		var parameters = []string{"#EXT-X-VERSION:", "#EXT-X-PLAYLIST-TYPE:", "#EXT-X-MEDIA-SEQUENCE:", "#EXT-X-STREAM-INF:", "#EXTINF:"}

		for _, parameter := range parameters {

			if strings.Contains(line, parameter) {

				var value = strings.Replace(line, parameter, "", -1)

				switch parameter {

				case "#EXT-X-VERSION:":
					version, err := strconv.Atoi(value)
					if err == nil {
						segment.Version = version
					}

				case "#EXT-X-PLAYLIST-TYPE:":
					segment.PlaylistType = value

				case "#EXT-X-MEDIA-SEQUENCE:":
					n, err := strconv.ParseInt(value, 10, 64)
					if err == nil {
						stream.Sequence = n
						sequence = n
					}

				case "#EXT-X-STREAM-INF:":
					segment.Info = true
					segment.StreamInf.Bandwidth = getBandwidth(value)

				case "#EXTINF:":
					var d = strings.Split(value, ",")
					if len(d) > 0 {

						value = strings.Replace(d[0], ",", "", -1)
						duration, err := strconv.ParseFloat(value, 64)
						if err == nil {
							segment.Duration = duration
						} else {
							ShowError(err, 1050)
							return err
						}

					}

				}

			}

		}

		return
	}

	var parseURL = func(line string, segment *Segment) {

		// Check if the address is a valid URL (http://... or /path/to/stream)
		_, err := url.ParseRequestURI(line)
		if err == nil {

			// Check if the domain is included in the address
			u, _ := url.Parse(line)

			if len(u.Host) == 0 {
				// Address does not contain the domain, redirect is added to the address
				segment.URL = stream.URLStreamingServer + line
			} else {
				// Domain included in the address
				segment.URL = line
			}

		} else {

			// Not a URL, but a file path (media/file-01.ts)
			var serverURLPath = strings.Replace(stream.M3U8URL, path.Base(stream.M3U8URL), line, -1)
			segment.URL = serverURLPath

		}
	}

	if strings.Contains(stream.Body, "#EXTM3U") {

		var lines = strings.Split(strings.Replace(stream.Body, "\r\n", "\n", -1), "\n")

		if !stream.DynamicBandwidth {
			stream.DynamicStream = make(map[int]DynamicStream)
		}

		// Parse parameters
		for i, line := range lines {

			_ = i

			if len(line) > 0 {

				if line[0:1] == "#" {

					err := parseParameter(line, &segment)
					if err != nil {
						return err
					}

					lastSegmentDuration = segment.Duration

				}

				// M3U8 contains multiple links to other M3U8 playlists (bandwidth options)
				if segment.Info && len(line) > 0 && line[0:1] != "#" {

					var dynamicStream DynamicStream

					segment.Duration = 0
					noNewSegment = false

					stream.DynamicBandwidth = true
					parseURL(line, &segment)

					dynamicStream.Bandwidth = segment.StreamInf.Bandwidth
					dynamicStream.URL = segment.URL

					stream.DynamicStream[dynamicStream.Bandwidth] = dynamicStream

				}

				// Segment with TS stream
				if segment.Duration > 0 && line[0:1] != "#" {

					parseURL(line, &segment)

					if len(segment.URL) > 0 {
						segment.Sequence = sequence
						m3u8Segments = append(m3u8Segments, segment)
						sequence++
					}

				}

			}

		}

	} else {

		err = errors.New(getErrMsg(4051))
		return
	}

	if len(m3u8Segments) > 0 {

		noNewSegment = true

		if !stream.Status {

			if len(m3u8Segments) >= 2 {
				m3u8Segments = m3u8Segments[0 : len(m3u8Segments)-1]
			}

		}

		for _, s := range m3u8Segments {

			segment = s

			if !stream.Status {

				noNewSegment = false
				stream.LastSequence = segment.Sequence

				// Stream is of type VOD. The first segment of the M3U8 playlist must be used.
				if strings.ToUpper(segment.PlaylistType) == "VOD" {
					break
				}

			} else {

				if segment.Sequence > stream.LastSequence {

					stream.LastSequence = segment.Sequence
					noNewSegment = false
					break

				}

			}

		}

	}

	if !noNewSegment {

		if stream.DynamicBandwidth {
			switchBandwidth(stream)
		} else {
			stream.Segment = append(stream.Segment, segment)
		}

	}

	if noNewSegment {

		var sleep = lastSegmentDuration * 0.5

		for i := 0.0; i < sleep*1000; i = i + 100 {

			_ = i
			time.Sleep(time.Duration(100) * time.Millisecond)

			if _, err := bufferVFS.Stat(stream.Folder); fsIsNotExistErr(err) {
				break
			}

		}

	}

	return
}

func switchBandwidth(stream *ThisStream) (err error) {

	var bandwidth []int
	var dynamicStream DynamicStream
	var segment Segment

	for key := range stream.DynamicStream {
		bandwidth = append(bandwidth, key)
	}

	sort.Ints(bandwidth)

	if len(bandwidth) > 0 {

		for i := range bandwidth {

			segment.StreamInf.Bandwidth = stream.DynamicStream[bandwidth[i]].Bandwidth

			dynamicStream = stream.DynamicStream[bandwidth[0]]

			if stream.NetworkBandwidth == 0 {

				dynamicStream = stream.DynamicStream[bandwidth[0]]
				break

			} else {

				if bandwidth[i] > stream.NetworkBandwidth {
					break
				}

				dynamicStream = stream.DynamicStream[bandwidth[i]]

			}

		}

	} else {

		err = errors.New("M3U8 does not contain streaming URLs")
		return

	}

	segment.URL = dynamicStream.URL
	segment.Duration = 0
	stream.Segment = append(stream.Segment, segment)

	return
}

// Timeouts for the ffmpeg/VLC buffer process.
var (
	// How long the process may take to fill its first segment.
	bufferStartTimeout = 20 * time.Second
	// How long a running stream may go without any data before it is
	// treated as dead (failover to a backup, or disconnect the viewers).
	bufferStallTimeout = 15 * time.Second
)

// Size of each read from the buffer process. Larger reads mean fewer
// syscalls per second; a Linux pipe holds 64 KB by default.
const bufferReadSize = 64 * 1024

// nextBackup returns the number (1-3) of the first configured backup channel
// after backup number `current` (0 = primary), or 0 if none is left.
func nextBackup(stream ThisStream, current int) int {
	backups := []*BackupStream{stream.BackupChannel1, stream.BackupChannel2, stream.BackupChannel3}
	for n := current + 1; n <= len(backups); n++ {
		if backups[n-1] != nil {
			return n
		}
	}
	return 0
}

// lastSegmentNumber returns the highest N among the N.ts files in folder,
// or 0 if there are none.
func lastSegmentNumber(folder string) (last int64) {
	files, err := bufferVFS.ReadDir(getPlatformPath(folder))
	if err != nil {
		return 0
	}
	for _, file := range files {
		if n, err := strconv.ParseInt(strings.TrimSuffix(file.Name(), ".ts"), 10, 64); err == nil && n > last {
			last = n
		}
	}
	return
}

// Buffer with FFMPEG
func thirdPartyBuffer(streamID int, playlistID string, useBackup bool, backupNumber int) {

	p, ok := BufferInformation.Load(playlistID)
	if !ok {
		return
	}

	var playlist = p.(Playlist)
	var debug, path, options, bufferType string
	var bufferSize = Settings.BufferSize * 1024
	var stream = playlist.Streams[streamID]
	var clientKey = playlistID + stream.MD5
	var fileSize = 0

	if bufferSize <= 0 {
		bufferSize = 1024 * 1024
	}

	var tmpFolder = stream.Folder
	var url = stream.URL
	if useBackup {
		switch backupNumber {
		case 1:
			if stream.BackupChannel1 != nil {
				url = stream.BackupChannel1.URL
				showHighlight("START OF BACKUP 1 STREAM")
				showInfo("Backup Channel 1 URL: " + url)
			}
		case 2:
			if stream.BackupChannel2 != nil {
				url = stream.BackupChannel2.URL
				showHighlight("START OF BACKUP 2 STREAM")
				showInfo("Backup Channel 2 URL: " + url)
			}
		case 3:
			if stream.BackupChannel3 != nil {
				url = stream.BackupChannel3.URL
				showHighlight("START OF BACKUP 3 STREAM")
				showInfo("Backup Channel 3 URL: " + url)
			}
		}
	}

	stream.Status = false

	bufferType = strings.ToUpper(playlist.Buffer)

	switch playlist.Buffer {

	case "ffmpeg":

		if Settings.FFmpegForceHttp {
			url = strings.Replace(url, "https://", "http://", -1)
			showInfo("Forcing URL to HTTP for FFMPEG: " + url)
		}

		path = Settings.FFmpegPath
		options = Settings.FFmpegOptions

	case "vlc":
		path = Settings.VLCPath
		options = Settings.VLCOptions

	default:
		return
	}

	// Fail over to the next configured backup channel. When none is left,
	// pass the error to the viewers so they disconnect and free the tuner.
	//
	// Viewers remove themselves (killClientConnection) once they see the
	// error, so the buffer must not decrement their connection count itself.
	var addErrorToStream = func(err error) {
		if next := nextBackup(stream, backupNumber); next > 0 {
			thirdPartyBuffer(streamID, playlistID, true, next)
			return
		}

		Lock.Lock()
		if c, ok := BufferClients.Load(clientKey); ok {
			var clients = c.(ClientConnection)
			clients.Error = err
			BufferClients.Store(clientKey, clients)
		}
		Lock.Unlock()
	}

	// Only a fresh start clears the folder. On failover the viewers are still
	// reading from it, and the new process continues the segment numbering.
	if !useBackup {
		if err := bufferVFS.RemoveAll(getPlatformPath(tmpFolder)); err != nil {
			ShowError(err, 4005)
		}
	}

	err := checkVFSFolder(tmpFolder, bufferVFS)
	if err != nil {
		ShowError(err, 0)
		addErrorToStream(err)
		return
	}

	err = checkFile(path)
	if err != nil {
		ShowError(err, 0)
		addErrorToStream(err)
		return
	}

	showInfo(fmt.Sprintf("%s path:%s", bufferType, path))
	showInfo("Streaming URL:" + url)

	// Viewers only send segments numbered higher than the last one they sent,
	// so after a failover the numbering must continue, not restart at 1.ts.
	var tmpSegment = lastSegmentNumber(tmpFolder) + 1
	var firstSegment = tmpSegment
	var tmpFile = fmt.Sprintf("%s%d.ts", tmpFolder, tmpSegment)

	// Set User-Agent
	var args []string

	for i, a := range strings.Split(options, " ") {

		switch bufferType {
		case "FFMPEG":
			a = strings.Replace(a, "[URL]", url, -1)
			if i == 0 {
				if len(Settings.UserAgent) != 0 {
					args = []string{"-user_agent", Settings.UserAgent}
				}

				if playlist.HttpProxyIP != "" && playlist.HttpProxyPort != "" {
					args = append(args, "-http_proxy", fmt.Sprintf("http://%s:%s", playlist.HttpProxyIP, playlist.HttpProxyPort))
				}

				var headers string
				if len(playlist.HttpUserReferer) != 0 {
					headers += fmt.Sprintf("Referer: %s\r\n", playlist.HttpUserReferer)
				}
				if len(playlist.HttpUserOrigin) != 0 {
					headers += fmt.Sprintf("Origin: %s\r\n", playlist.HttpUserOrigin)
				}
				if headers != "" {
					args = append(args, "-headers", headers)
				}
			}

			args = append(args, a)

		case "VLC":
			if a == "[URL]" {
				a = strings.Replace(a, "[URL]", url, -1)
				args = append(args, a)

				if len(Settings.UserAgent) != 0 {
					args = append(args, fmt.Sprintf(":http-user-agent=%s", Settings.UserAgent))
				}

				if len(playlist.HttpUserReferer) != 0 {
					args = append(args, fmt.Sprintf(":http-referrer=%s", playlist.HttpUserReferer))
				}

				if playlist.HttpProxyIP != "" && playlist.HttpProxyPort != "" {
					args = append(args, fmt.Sprintf(":http-proxy=%s:%s", playlist.HttpProxyIP, playlist.HttpProxyPort))
				}

			} else {
				args = append(args, a)
			}

		}

	}

	var cmd = exec.Command(path, args...)
	// Set this explicitly to avoid issues with VLC
	cmd.Env = append(os.Environ(), "DISPLAY=:0")

	debug = fmt.Sprintf("BUFFER DEBUG: %s:%s %s", bufferType, path, args)
	showDebug(debug, 1)

	// Byte data from the process
	stdOut, err := cmd.StdoutPipe()
	if err != nil {
		ShowError(err, 0)
		addErrorToStream(err)
		return
	}

	// Log data from the process
	logOut, err := cmd.StderrPipe()
	if err != nil {
		ShowError(err, 0)
		addErrorToStream(err)
		return
	}

	f, err := bufferVFS.Create(tmpFile)
	if err != nil {
		ShowError(err, 0)
		addErrorToStream(err)
		return
	}
	defer func() { f.Close() }() // f is replaced on every new segment

	showInfo(bufferType + ":Processing data")

	if err = cmd.Start(); err != nil {
		ShowError(err, 0)
		addErrorToStream(err)
		return
	}
	var startTime = time.Now()

	// Closed once the first segment is complete.
	var started = make(chan struct{})

	go func() {

		// Show the process log: as info until the stream is running, then
		// only in debug mode 1. Never sleep here: if stderr is not drained
		// the pipe fills up, the process blocks, and the video stalls.
		scanner := bufio.NewScanner(logOut)
		scanner.Split(bufio.ScanLines)

		for scanner.Scan() {

			var msg = fmt.Sprintf("%s log:%s", bufferType, strings.TrimSpace(scanner.Text()))

			select {
			case <-started:
				showDebug(msg, 1)
			default:
				showInfo(msg)
			}

		}

		// Keep draining if the scanner stopped early (e.g. an over-long line).
		io.Copy(io.Discard, logOut)

	}()

	// Watchdog: the read below blocks while the process sends nothing, so
	// stall detection and "every viewer has left" have to run alongside it.
	// Stopping the process unblocks the read.
	var lastData atomic.Int64
	lastData.Store(startTime.UnixNano())
	var stalled atomic.Bool
	var watchdogDone = make(chan struct{})
	var stopWatchdog = sync.OnceFunc(func() { close(watchdogDone) })
	defer stopWatchdog()

	go func() {

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-watchdogDone:
				return
			case <-ticker.C:
			}

			if _, ok := BufferClients.Load(clientKey); !ok {
				cmd.Process.Kill()
				return
			}

			select {
			case <-started:
				if time.Since(time.Unix(0, lastData.Load())) > bufferStallTimeout {
					stalled.Store(true)
					cmd.Process.Kill()
					return
				}
			default:
				if time.Since(startTime) > bufferStartTimeout {
					stalled.Store(true)
					cmd.Process.Kill()
					return
				}
			}
		}

	}()

	// Stops the process and the watchdog after a failure, then fails over or
	// passes the error to the viewers.
	var fail = func(err error) {
		stopWatchdog()
		cmd.Process.Kill()
		cmd.Wait()
		ShowError(err, 0)
		addErrorToStream(err)
	}

	showInfo("Streaming Status:Receive data from " + bufferType)

	buffer := make([]byte, bufferReadSize)

	for {

		n, readErr := stdOut.Read(buffer)

		if n > 0 {

			lastData.Store(time.Now().UnixNano())
			fileSize += n

			if _, err := f.Write(buffer[:n]); err != nil {
				fail(err)
				return
			}

		}

		if readErr != nil {
			// EOF: the process exited or was stopped by the watchdog.
			break
		}

		if fileSize >= bufferSize/2 {

			if tmpSegment == firstSegment {
				close(started)
				showInfo(fmt.Sprintf("Streaming Status:Buffering data from %s", bufferType))
			}

			f.Close()
			tmpSegment++

			if !stream.Status {
				Lock.Lock()
				stream.Status = true
				playlist.Streams[streamID] = stream
				BufferInformation.Store(playlistID, playlist)
				Lock.Unlock()
			}

			tmpFile = fmt.Sprintf("%s%d.ts", tmpFolder, tmpSegment)

			fileSize = 0

			if f, err = bufferVFS.Create(tmpFile); err != nil {
				fail(err)
				return
			}

		}

	}

	stopWatchdog()
	cmd.Process.Kill()
	cmd.Wait()

	// Every viewer has left: just clean up.
	if _, ok := BufferClients.Load(clientKey); !ok {
		clientConnection(stream)
		return
	}

	if stalled.Load() {
		select {
		case <-started:
			err = fmt.Errorf("%s: no data from the source for %v", bufferType, bufferStallTimeout)
		default:
			err = errors.New("Timeout")
		}
		ShowError(err, 4006)
	} else {
		err = errors.New(bufferType + " error")
		ShowError(err, 1204)
	}

	addErrorToStream(err)

	time.Sleep(time.Duration(500) * time.Millisecond)
	clientConnection(stream)

}

func getTuner(id, playlistType string) (tuner int) {

	var playListBuffer string
	systemMutex.Lock()
	playListInterface := Settings.Files.M3U[id]
	if playListInterface == nil {
		playListInterface = Settings.Files.HDHR[id]
	}
	if playListMap, ok := playListInterface.(map[string]interface{}); ok {
		if buffer, ok := playListMap["buffer"].(string); ok {
			playListBuffer = buffer
		} else {
			playListBuffer = "-"
		}
	}
	systemMutex.Unlock()

	switch playListBuffer {

	case "-":
		tuner = Settings.Tuner

	case "threadfin", "ffmpeg", "vlc":

		i, err := strconv.Atoi(getProviderParameter(id, playlistType, "tuner"))
		if err == nil {
			tuner = i
		} else {
			ShowError(err, 0)
			tuner = 1
		}

	}

	return
}

func initBufferVFS() {
	bufferVFS = memfs.New()
}

func debugRequest(req *http.Request) {

	var debugLevel = 3

	if System.Flag.Debug < debugLevel {
		return
	}

	var debug string

	fmt.Println()
	debug = "Request:* * * * * * BEGIN HTTP(S) REQUEST * * * * * * "
	showDebug(debug, debugLevel)

	debug = fmt.Sprintf("Method:%s", req.Method)
	showDebug(debug, debugLevel)

	debug = fmt.Sprintf("Proto:%s", req.Proto)
	showDebug(debug, debugLevel)

	debug = fmt.Sprintf("URL:%s", req.URL)
	showDebug(debug, debugLevel)

	for name, headers := range req.Header {

		name = strings.ToLower(name)

		for _, h := range headers {
			debug = fmt.Sprintf("Header:%v: %v", name, h)
			showDebug(debug, debugLevel)
		}

	}

	debug = "Request:* * * * * * END HTTP(S) REQUEST * * * * * *"
	showDebug(debug, debugLevel)

	return
}

func debugResponse(resp *http.Response) {

	var debugLevel = 3

	if System.Flag.Debug < debugLevel {
		return
	}

	var debug string

	fmt.Println()

	debug = "Response:* * * * * * BEGIN RESPONSE * * * * * * "
	showDebug(debug, debugLevel)

	debug = fmt.Sprintf("Proto:%s", resp.Proto)
	showDebug(debug, debugLevel)

	debug = fmt.Sprintf("Status Code:%d", resp.StatusCode)
	showDebug(debug, debugLevel)

	debug = fmt.Sprintf("Status Text:%s", http.StatusText(resp.StatusCode))
	showDebug(debug, debugLevel)

	for key, value := range resp.Header {

		switch fmt.Sprintf("%T", value) {

		case "[]string":
			debug = fmt.Sprintf("Header:%v: %s", key, strings.Join(value, " "))

		default:
			debug = fmt.Sprintf("Header:%v: %v", key, value)
		}

		showDebug(debug, debugLevel)

	}

	debug = "Pesponse:* * * * * * END RESPONSE * * * * * * "
	showDebug(debug, debugLevel)

	return
}

func terminateProcessGracefully(cmd *exec.Cmd) {
	if cmd.Process != nil {
		// Send a SIGTERM to the process
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			// If an error occurred while trying to send the SIGTERM, you might resort to a SIGKILL.
			ShowError(err, 0)
			cmd.Process.Kill()
		}

		// Optionally, you can wait for the process to finish too
		cmd.Wait()
	}
}
