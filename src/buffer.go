package src

// Stream buffer: connects Threadfin's /stream/ requests to the stream
// buffer in src/internal/streambuf, which runs one ffmpeg/VLC process per
// channel and shares it between all viewers of that channel.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"threadfin/src/internal/streambuf"
)

// BackupStream is a backup URL for a channel, possibly on another playlist.
type BackupStream struct {
	PlaylistID string
	URL        string
}

var streamManager = streambuf.NewManager(streambuf.Logger{
	Info:      showInfo,
	Debug:     func(s string) { showDebug(s, 1) },
	Highlight: showHighlight,
	Error:     func(err error) { ShowError(err, 0) },
})

// bufferTuning holds the stream buffer settings that are not exposed in
// the web interface (tests shorten the timeouts).
var bufferTuning = streambuf.DefaultConfig()

const (
	// How long a request waits for its stream before answering. A channel
	// that fails to start within this time gets a 503 (as from a real
	// HDHomeRun) instead of an empty video; clients that need an early
	// answer get the headers after it and keep waiting for data.
	streamHeaderWait = 3 * time.Second

	// A client that accepts no data for this long is disconnected, freeing
	// its tuner (for example a TV that was switched off without closing the
	// connection).
	streamWriteTimeout = 60 * time.Second

	// Largest single write to a client.
	streamWriteSize = 256 << 10
)

// bufferConfig combines the settings from the web interface with the
// fixed tuning values.
func bufferConfig() streambuf.Config {
	cfg := bufferTuning

	// "Buffer Size": the cushion of data each viewer receives at once
	// when it starts.
	cfg.Prebuffer = Settings.BufferSize * 1024
	if cfg.Prebuffer < 0 {
		cfg.Prebuffer = 0
	}

	// "Timeout for new client connections": how long a new channel waits
	// for a tuner held by the channel the client is switching away from.
	if w := time.Duration(Settings.BufferTimeout) * time.Millisecond; w > cfg.TunerWait {
		cfg.TunerWait = w
	}

	return cfg
}

func playlistType(playlistID string) string {
	if strings.HasPrefix(playlistID, "H") {
		return "hdhr"
	}
	return "m3u"
}

// playlistBuffer returns the buffer setting of a playlist ("-", "ffmpeg",
// "vlc", or the old "threadfin").
func playlistBuffer(playlistID string) string {
	systemMutex.Lock()
	defer systemMutex.Unlock()

	p := Settings.Files.M3U[playlistID]
	if p == nil {
		p = Settings.Files.HDHR[playlistID]
	}
	if m, ok := p.(map[string]interface{}); ok {
		if b, ok := m["buffer"].(string); ok {
			return b
		}
	}
	return "-"
}

func streamSource(playlistID, url, label string) streambuf.Source {
	t := playlistType(playlistID)
	limit := getTuner(playlistID, t)
	if limit < 1 {
		limit = 1
	}
	return streambuf.Source{
		PlaylistID:   playlistID,
		PlaylistName: getProviderParameter(playlistID, t, "name"),
		TunerLimit:   limit,
		URL:          url,
		Label:        label,
	}
}

// joinStream connects to the channel, starting it if needed. backups[i] is
// labelled "backup <firstBackup+i>".
func joinStream(ctx context.Context, playlistID, url, label string, backups []*BackupStream, firstBackup int, channelName string) (*streambuf.Viewer, streambuf.Source, error) {
	primary := streamSource(playlistID, url, label)
	sources := []streambuf.Source{primary}
	for i, b := range backups {
		if b != nil && b.URL != "" {
			sources = append(sources, streamSource(b.PlaylistID, b.URL, fmt.Sprintf("backup %d", firstBackup+i)))
		}
	}

	program := strings.ToLower(playlistBuffer(playlistID))
	if program != "vlc" {
		program = "ffmpeg" // also the old "threadfin" value, which has no buffer of its own any more
	}

	viewer, err := streamManager.Join(ctx, streambuf.Request{
		Channel: channelName,
		Program: strings.ToUpper(program),
		Sources: sources,
		Launch:  bufferLauncher(program),
		Config:  bufferConfig(),
	})
	return viewer, primary, err
}

// bufferingStream serves a /stream/ request through the buffer.
func bufferingStream(playlistID string, streamingURL string, backupStream1 *BackupStream, backupStream2 *BackupStream, backupStream3 *BackupStream, channelName string, w http.ResponseWriter, r *http.Request) {

	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	backups := []*BackupStream{backupStream1, backupStream2, backupStream3}

	viewer, src, err := joinStream(r.Context(), playlistID, streamingURL, "primary", backups, 1, channelName)

	// No tuner left on the channel's playlist: try the backup channels,
	// which may be on playlists with tuners to spare.
	for i := 0; errors.Is(err, streambuf.ErrNoTuner) && i < len(backups); i++ {
		if b := backups[i]; b != nil && b.URL != "" {
			viewer, _, err = joinStream(r.Context(), b.PlaylistID, b.URL, fmt.Sprintf("backup %d", i+1), backups[i+1:], i+2, channelName)
		}
	}

	switch {
	case errors.Is(err, streambuf.ErrNoTuner):
		showInfo(fmt.Sprintf("Streaming Status:Playlist: %s - No new connections available. Tuner = %d", src.PlaylistName, src.TunerLimit))
		sendTunerLimitVideo(w, r)
		return
	case err != nil:
		return // the client left while waiting for a tuner
	}
	defer viewer.Close()

	streamToClient(viewer, w, r)
}

func streamToClient(v *streambuf.Viewer, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rc := http.NewResponseController(w)

	readyCtx, cancel := context.WithTimeout(ctx, streamHeaderWait)
	err := v.WaitReady(readyCtx)
	cancel()

	switch {
	case ctx.Err() != nil:
		return
	case err != nil && !errors.Is(err, context.DeadlineExceeded):
		showInfo(fmt.Sprintf("Streaming Status:Channel: %s - could not be started", v.Channel()))
		http.Error(w, "The channel is not available", http.StatusServiceUnavailable)
		return
	}

	// video/mpeg is what a real HDHomeRun sends.
	w.Header().Set("Content-Type", "video/mpeg")
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	// The pieces of each batch are combined into few large writes: each
	// write through the HTTP connection costs several system calls.
	wbuf := make([]byte, 0, streamWriteSize)
	write := func(b []byte) bool {
		_, err := w.Write(b)
		return err == nil
	}

	for {
		bufs, err := v.Next(ctx)

		if len(bufs) > 0 {
			rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
			for _, b := range bufs {
				if len(wbuf)+len(b) > cap(wbuf) && len(wbuf) > 0 {
					if !write(wbuf) {
						return
					}
					wbuf = wbuf[:0]
				}
				if len(b) >= cap(wbuf) {
					if !write(b) {
						return
					}
					continue
				}
				wbuf = append(wbuf, b...)
			}
			if len(wbuf) > 0 {
				if !write(wbuf) {
					return
				}
				wbuf = wbuf[:0]
			}
			if ferr := rc.Flush(); ferr != nil {
				return
			}
		}

		if err != nil {
			if !errors.Is(err, context.Canceled) {
				showDebug(fmt.Sprintf("Streaming Status:Channel: %s - stream ended: %v", v.Channel(), err), 1)
			}
			return
		}
	}
}

// sendTunerLimitVideo plays the "tuner limit reached" clip for 30 seconds.
func sendTunerLimitVideo(w http.ResponseWriter, r *http.Request) {
	value, ok := webUI["html/video/stream-limit.ts"]
	if !ok {
		http.Error(w, "All tuners are in use", http.StatusServiceUnavailable)
		return
	}

	content := []byte(GetHTMLString(value.(string)))
	w.Header().Set("Content-Type", "video/mpeg")
	w.WriteHeader(http.StatusOK)

	for i := 1; i < 60; i++ {
		if _, err := w.Write(content); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// getActiveClientCount returns the number of connected buffer viewers.
func getActiveClientCount() int {
	return streamManager.Stats().Viewers
}

// getActivePlaylistCount returns the number of playlists with running
// buffered channels.
func getActivePlaylistCount() int {
	return streamManager.Stats().Playlists
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
