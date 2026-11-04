package src

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"threadfin/src/internal/streambuf"
)

// These tests drive the whole buffer (HTTP handler, stream manager, real
// processes) against a fake "ffmpeg" shell script. The script receives the
// stream URL as its only argument and treats it as a behaviour mode
// (anything after the mode, e.g. "stream?x", only makes the URL unique):
//
//	stream  - emit ~1.3 MB/s forever
//	short   - emit ~1.3 MB/s for ~3 s, then exit (upstream dropped)
//	hang    - emit ~1.3 MB/s for ~1 s, then stop sending without exiting
//	          (upstream stalled)
//	fail    - print an error and exit at once (e.g. HTTP 404)
const fakeFFmpegScript = `#!/bin/sh
burst() { head -c 65536 /dev/zero; sleep 0.05; }
case "$1" in
  stream*) while :; do burst; done ;;
  short*)  i=0; while [ $i -lt 60 ]; do burst; i=$((i+1)); done ;;
  hang*)   i=0; while [ $i -lt 20 ]; do burst; i=$((i+1)); done; exec sleep 3600 ;;
  fail*)   echo "Server returned 404 Not Found" >&2; exit 1 ;;
esac
`

const testPlaylistID = "Mtest"

var (
	bufferTestOnce   sync.Once
	bufferTestScript string
)

// setupBufferTest configures the globals once for all buffer tests. Every
// test uses its own stream URLs.
func setupBufferTest(t testing.TB) {
	t.Helper()

	if testing.Short() {
		t.Skip("buffer integration tests run real processes; skipped with -short")
	}

	bufferTestOnce.Do(func() {
		dir, err := os.MkdirTemp("", "threadfin-buffer-test")
		if err != nil {
			t.Fatal(err)
		}
		bufferTestScript = filepath.Join(dir, "ffmpeg")
		if err := os.WriteFile(bufferTestScript, []byte(fakeFFmpegScript), 0755); err != nil {
			t.Fatal(err)
		}

		System.Flag.Info = true // keep the test output readable
		loadHTMLMap()           // embedded files, for the tuner-limit video

		bufferTuning.StartTimeout = 3 * time.Second
		bufferTuning.StallTimeout = 2 * time.Second
		bufferTuning.Linger = time.Second

		Settings.BufferSize = 256 // KB
		Settings.BufferTimeout = 0
		Settings.UserAgent = ""
		Settings.FFmpegPath = bufferTestScript
		Settings.FFmpegOptions = "[URL]"
		Settings.Files.M3U = map[string]interface{}{
			testPlaylistID: map[string]interface{}{
				"buffer": "ffmpeg",
				"tuner":  "4",
				"name":   "test",
			},
		}
	})

	// Once every viewer has left, the stream must stop with its process.
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for countProcesses(bufferTestScript) > 0 || streamManager.Stats().Streams > 0 {
			if time.Now().After(deadline) {
				t.Errorf("stream still running 5s after all viewers left: %+v, %d processes",
					streamManager.Stats(), countProcesses(bufferTestScript))
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

// countProcesses returns how many running processes have arg in their
// command line (Linux /proc).
func countProcesses(arg string) (n int) {
	dirs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, d := range dirs {
		if b, err := os.ReadFile(d); err == nil && strings.Contains(string(b), arg) {
			n++
		}
	}
	return
}

// streamResult records what a client received.
type streamResult struct {
	status  int
	data    []byte
	bytes   int64
	first   time.Duration // time to the first byte
	maxGap  time.Duration // longest pause between two reads
	ended   bool          // server closed the response
	endedAt time.Duration // time from request start to end of response
}

// watchStream requests the stream and reads it until the server ends the
// response or `limit` elapses. With keep set it records the data.
func watchStream(t *testing.T, srv *httptest.Server, path string, limit time.Duration, keep bool) streamResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	res := streamResult{status: resp.StatusCode}
	buf := make([]byte, 64*1024)
	last := time.Now()
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if res.bytes == 0 {
				res.first = time.Since(start)
			} else if gap := time.Since(last); gap > res.maxGap {
				res.maxGap = gap
			}
			last = time.Now()
			res.bytes += int64(n)
			if keep {
				res.data = append(res.data, buf[:n]...)
			}
		}
		if err != nil {
			if err == io.EOF || ctx.Err() == nil {
				res.ended = true
				res.endedAt = time.Since(start)
			}
			if res.bytes > 0 {
				if gap := time.Since(last); gap > res.maxGap {
					res.maxGap = gap
				}
			}
			return res
		}
	}
}

func newBufferServer(t *testing.T, primary string, backup *BackupStream) *httptest.Server {
	primary += "?" + t.Name()
	if backup != nil {
		backup.URL += "?" + t.Name()
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		url := primary
		if r.URL.Path != "/" {
			url = "stream?" + r.URL.Path + "-" + t.Name()
		}
		bufferingStream(testPlaylistID, url, backup, nil, nil, "chan"+r.URL.Path, w, r)
	}))
}

// A source that drops is replaced by its backup without the viewer
// noticing (the previous buffer froze the viewer here).
func TestBufferFailoverKeepsStreaming(t *testing.T) {
	setupBufferTest(t)

	srv := newBufferServer(t, "short", &BackupStream{PlaylistID: testPlaylistID, URL: "stream"})
	defer srv.Close()

	res := watchStream(t, srv, "/", 9*time.Second, false)
	t.Logf("received %d bytes, first byte after %v, longest gap %v, ended=%v", res.bytes, res.first, res.maxGap, res.ended)

	if res.ended {
		t.Fatalf("stream ended after %v instead of failing over to the backup", res.endedAt)
	}
	if res.maxGap > 500*time.Millisecond {
		t.Fatalf("viewer froze for %v during failover", res.maxGap)
	}
}

// When every source has failed, the viewer is disconnected (and the tuner
// freed) instead of being left on a dead stream.
func TestBufferSourceExitEndsResponse(t *testing.T) {
	setupBufferTest(t)

	srv := newBufferServer(t, "short", nil)
	defer srv.Close()

	// The source is reconnected once (it may have been a hiccup), then the
	// stream gives up.
	res := watchStream(t, srv, "/", 12*time.Second, false)
	t.Logf("received %d bytes, response ended after %v", res.bytes, res.endedAt)

	if !res.ended {
		t.Fatalf("response still open 12s after the source failed")
	}
	if res.bytes == 0 {
		t.Fatalf("no data received before the source exited")
	}
}

// A source that stops sending without disconnecting is detected.
func TestBufferStalledSourceEndsResponse(t *testing.T) {
	setupBufferTest(t)

	srv := newBufferServer(t, "hang", nil)
	defer srv.Close()

	res := watchStream(t, srv, "/", 15*time.Second, false)
	t.Logf("received %d bytes, response ended after %v", res.bytes, res.endedAt)

	if !res.ended {
		t.Fatalf("response still open 15s after the source stalled")
	}
}

// A channel that cannot start gets a proper error, like from a real
// HDHomeRun, instead of an empty video.
func TestBufferFailedChannelGets503(t *testing.T) {
	setupBufferTest(t)

	srv := newBufferServer(t, "fail", nil)
	defer srv.Close()

	res := watchStream(t, srv, "/", 5*time.Second, false)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", res.status)
	}
}

// Two channels of one playlist tuned at the same instant, several times:
// each gets its own stream and data (the previous buffer could mix them up
// and leave a process running).
func TestBufferSimultaneousTunesOnOnePlaylist(t *testing.T) {
	setupBufferTest(t)

	srv := newBufferServer(t, "stream", nil)
	defer srv.Close()

	for round := 0; round < 3; round++ {
		var wg sync.WaitGroup
		results := make([]streamResult, 4)
		for i, path := range []string{"/a", "/b", "/a", "/b"} {
			wg.Add(1)
			go func(i int, path string) {
				defer wg.Done()
				results[i] = watchStream(t, srv, path, 2*time.Second, false)
			}(i, path)
		}
		wg.Wait()

		for i, res := range results {
			if res.bytes == 0 {
				t.Fatalf("round %d: client %d received no data", round, i)
			}
		}
	}
}

func TestBufferTunerLimitPlaysLimitVideo(t *testing.T) {
	setupBufferTest(t)

	systemMutex.Lock()
	Settings.Files.M3U["Mlimit"] = map[string]interface{}{"buffer": "ffmpeg", "tuner": "1", "name": "limited"}
	systemMutex.Unlock()
	defer func() {
		systemMutex.Lock()
		delete(Settings.Files.M3U, "Mlimit")
		systemMutex.Unlock()
	}()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bufferingStream("Mlimit", "stream?"+r.URL.Path+t.Name(), nil, nil, nil, "chan", w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/one", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	eventually(t, 3*time.Second, func() bool { return streamManager.Stats().Viewers == 1 })

	res := watchStream(t, srv, "/two", 3*time.Second+bufferTuning.TunerWait, true)
	cancel()

	want := []byte(GetHTMLString(webUI["html/video/stream-limit.ts"].(string)))
	if res.status != http.StatusOK || !bytes.HasPrefix(res.data, want[:1024]) {
		t.Fatalf("second channel did not get the tuner-limit video (status %d, %d bytes)", res.status, res.bytes)
	}
}

func eventually(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---- real ffmpeg ------------------------------------------------------

// useRealFFmpeg makes the buffer run real ffmpeg, generating a live 25 fps
// H.264/AAC test channel with a keyframe every 2 s.
func useRealFFmpeg(t *testing.T) {
	setupBufferTest(t)

	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}

	oldPath, oldOptions := Settings.FFmpegPath, Settings.FFmpegOptions
	Settings.FFmpegPath = path
	Settings.FFmpegOptions = "-hide_banner -loglevel error -re -f lavfi -i testsrc2=size=640x360:rate=25 -f lavfi -i sine=frequency=1000 -c:v libx264 -preset ultrafast -g 50 -b:v 2M -c:a aac -f mpegts pipe:1"
	t.Cleanup(func() {
		eventually(t, 5*time.Second, func() bool { return streamManager.Stats().Streams == 0 })
		Settings.FFmpegPath, Settings.FFmpegOptions = oldPath, oldOptions
	})
}

// completeFrames cuts a capture before its last video frame, which the
// client may have stopped reading in the middle of (ffmpeg puts video on
// PID 0x100).
func completeFrames(data []byte) []byte {
	n := len(data) / 188 * 188
	for off := n - 188; off >= 0; off -= 188 {
		p := data[off:]
		if p[0] == 0x47 && p[1]&0x40 != 0 && uint16(p[1]&0x1f)<<8|uint16(p[2]) == 0x100 {
			return data[:off]
		}
	}
	return data[:n]
}

// decodeErrors decodes a captured stream with ffmpeg and returns its error
// output (empty means it decoded cleanly from the first byte).
func decodeErrors(t *testing.T, data []byte) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "capture.ts")
	if err := os.WriteFile(file, completeFrames(data), 0644); err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command("ffmpeg", "-hide_banner", "-v", "error", "-i", file, "-f", "null", "-").CombinedOutput()
	return strings.TrimSpace(string(out))
}

func firstVideoFrameType(t *testing.T, data []byte) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "probe.ts")
	if err := os.WriteFile(file, data, 0644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "frame=pict_type",
		"-read_intervals", "%+#1", "-of", "csv=p=0", file).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	return strings.Trim(string(out), ", \n")
}

// A viewer joining a running channel starts at once, on a keyframe with
// the program tables first, and decodes without a single error.
func TestBufferRealFFmpegJoinerStartsCleanly(t *testing.T) {
	useRealFFmpeg(t)

	srv := newBufferServer(t, "live", nil)
	defer srv.Close()

	firstViewer := make(chan streamResult)
	go func() { firstViewer <- watchStream(t, srv, "/", 7*time.Second, true) }()
	time.Sleep(3500 * time.Millisecond) // join mid-GOP

	joiner := watchStream(t, srv, "/", 3*time.Second, true)
	first := <-firstViewer

	t.Logf("first viewer: first byte after %v, %d bytes", first.first, first.bytes)
	t.Logf("joiner: first byte after %v, %d bytes", joiner.first, joiner.bytes)

	for name, res := range map[string]streamResult{"first viewer": first, "joiner": joiner} {
		if pid := uint16(res.data[1]&0x1f)<<8 | uint16(res.data[2]); res.data[0] != 0x47 || pid != 0 {
			t.Errorf("%s: stream does not start with the PAT", name)
		}
		if ft := firstVideoFrameType(t, res.data); ft != "I" {
			t.Errorf("%s: first video frame is %q, want a keyframe (I)", name, ft)
		}
		if errs := decodeErrors(t, res.data); errs != "" {
			t.Errorf("%s: decode errors:\n%s", name, errs)
		}
	}

	if joiner.first > 200*time.Millisecond {
		t.Errorf("joiner waited %v for its first byte", joiner.first)
	}
	if joiner.bytes < int64(Settings.BufferSize*1024) {
		t.Errorf("joiner received %d bytes in 3 s, want at least the %d KB cushion", joiner.bytes, Settings.BufferSize)
	}
}

// ---- HEAD ---------------------------------------------------------------

// A provider that never answers a HEAD request (common: many do not
// support it) must not hold the client's request, a goroutine and a socket
// forever; nor may a client that left.
func TestStreamHEADIsBounded(t *testing.T) {
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ok") {
			w.Header().Set("X-Provider", "yes")
			return
		}
		select { // never answers
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer provider.Close()
	defer close(release)

	old := streamHeadTimeout
	defer func() { streamHeadTimeout = old }()

	head := func(ctx context.Context, path string) (*httptest.ResponseRecorder, time.Duration) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodHead, "/stream/x", nil).WithContext(ctx)
		start := time.Now()
		streamHEAD(rec, req, provider.URL+path)
		return rec, time.Since(start)
	}

	streamHeadTimeout = 300 * time.Millisecond
	if rec, d := head(context.Background(), "/silent"); d > 2*time.Second || rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("silent provider: answered %d after %v, want 405 after the timeout", rec.Code, d)
	}

	streamHeadTimeout = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond) // the client leaves
	defer cancel()
	if _, d := head(ctx, "/silent"); d > 2*time.Second {
		t.Errorf("request went on for %v after the client left", d)
	}

	if rec, _ := head(context.Background(), "/ok"); rec.Code != http.StatusOK || rec.Header().Get("X-Provider") != "yes" {
		t.Errorf("provider headers not passed on: %d %v", rec.Code, rec.Header())
	}
}

// stuckWriter is a client that accepts no data: a write blocks until the
// write deadline (if one is set) passes.
type stuckWriter struct {
	header   http.Header
	mu       sync.Mutex
	deadline time.Time
	writes   int
}

func (w *stuckWriter) Header() http.Header { return w.header }
func (w *stuckWriter) WriteHeader(int)     {}

func (w *stuckWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = t
	return nil
}

func (w *stuckWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	w.writes++
	deadline := w.deadline
	w.mu.Unlock()
	if w.writes == 1 {
		return len(b), nil // the first one fits in the socket buffers
	}
	if deadline.IsZero() {
		select {} // blocks forever, as on a dead connection without a deadline
	}
	time.Sleep(time.Until(deadline))
	return 0, errors.New("i/o timeout")
}

// The tuner-limit clip is sent with a write deadline: a client that
// accepts no data does not hold the request until TCP gives up.
func TestTunerLimitVideoDropsAStuckClient(t *testing.T) {
	setupBufferTest(t)
	old := tunerLimitWriteTimeout
	tunerLimitWriteTimeout = 200 * time.Millisecond
	defer func() { tunerLimitWriteTimeout = old }()

	w := &stuckWriter{header: http.Header{}}
	done := make(chan struct{})
	go func() {
		sendTunerLimitVideo(w, httptest.NewRequest(http.MethodGet, "/stream/x", nil))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("still writing to a client that accepts no data")
	}
}

// Cost of a client connecting to a running channel and leaving again
// (Plex probes each channel and reconnects at once). Run with -benchmem.
func BenchmarkBufferViewerConnect(b *testing.B) {
	setupBufferTest(b)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bufferingStream(testPlaylistID, "stream?"+b.Name(), nil, nil, nil, "bench", w, r)
	}))
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()

	get := func(ctx context.Context, n int64) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		io.CopyN(io.Discard, resp.Body, n)
		resp.Body.Close()
	}

	// One viewer keeps the channel running.
	ctx, cancel := context.WithCancel(context.Background())
	running := make(chan struct{})
	go func() {
		defer close(running)
		get(ctx, 1<<40)
	}()
	defer func() { cancel(); <-running }()
	for streamManager.Stats().Viewers == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		get(context.Background(), 64<<10)
	}
}

// Shutdown kills every source process (the processes run in their own
// process groups, so they would otherwise outlive Threadfin), and no new
// stream starts after it.
func TestBufferShutdownKillsEveryProcess(t *testing.T) {
	setupBufferTest(t)

	m := streambuf.NewManager(streambuf.Logger{})
	marker := "shutdown-" + t.Name()
	request := func(i int) streambuf.Request {
		return streambuf.Request{
			Channel: fmt.Sprint("chan", i),
			Program: "FFMPEG",
			Sources: []streambuf.Source{streamSource(testPlaylistID, fmt.Sprintf("stream?%s-%d", marker, i), "primary")},
			Launch:  bufferLauncher("ffmpeg"),
			Config:  bufferConfig(),
		}
	}

	var viewers []*streambuf.Viewer
	for i := 0; i < 3; i++ {
		v, err := m.Join(context.Background(), request(i))
		if err != nil {
			t.Fatal(err)
		}
		defer v.Close()
		viewers = append(viewers, v)
	}
	for _, v := range viewers {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := v.WaitReady(ctx)
		cancel()
		if err != nil {
			t.Fatalf("stream did not start: %v", err)
		}
	}
	if n := countProcesses(marker); n != 3 {
		t.Fatalf("%d source processes running, want 3", n)
	}

	if err := m.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if n := countProcesses(marker); n != 0 {
		t.Errorf("%d source processes still running after Shutdown", n)
	}
	if st := m.Stats(); st.Streams != 0 {
		t.Errorf("streams after Shutdown: %+v", st)
	}
	if _, err := m.Join(context.Background(), request(9)); !errors.Is(err, streambuf.ErrShutdown) {
		t.Errorf("Join after Shutdown = %v, want ErrShutdown", err)
	}
}
