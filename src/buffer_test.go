package src

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests drive the real ffmpeg buffer path (bufferingStream ->
// thirdPartyBuffer) against a fake "ffmpeg" shell script. The script receives
// the stream URL as its only argument and treats it as a behaviour mode:
//
// (anything after the mode, e.g. "stream?x", only makes the URL unique)
//
//	stream  - emit ~1.3 MB/s forever
//	short   - emit ~1.3 MB/s for ~3 s, then exit (upstream dropped)
//	hang    - emit ~1.3 MB/s for ~1 s, then stop sending without exiting
//	          (upstream stalled)
const fakeFFmpegScript = `#!/bin/sh
burst() { head -c 65536 /dev/zero; sleep 0.05; }
case "$1" in
  stream*) while :; do burst; done ;;
  short*)  i=0; while [ $i -lt 60 ]; do burst; i=$((i+1)); done ;;
  hang*)   i=0; while [ $i -lt 20 ]; do burst; i=$((i+1)); done; exec sleep 3600 ;;
esac
`

const testPlaylistID = "Mtest"

var (
	bufferTestOnce   sync.Once
	bufferTestScript string
)

// setupBufferTest configures the globals once for all buffer tests. They are
// not reset per test: a previous test's buffer goroutine may still be doing
// its final cleanup, and every test uses its own stream URLs anyway.
func setupBufferTest(t *testing.T) {
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

		initBufferVFS()
		System.Folder.Temp = "/tmp/threadfin-test/"
		System.Flag.Info = true // keep the test output readable

		Settings.BufferSize = 256 // KB -> 128 KB segments
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

	// Once every viewer has disconnected, the buffer must stop its process.
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for countProcesses(bufferTestScript) > 0 {
			if time.Now().After(deadline) {
				t.Errorf("buffer process still running 5s after all viewers left")
				return
			}
			time.Sleep(100 * time.Millisecond)
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
	bytes   int64
	maxGap  time.Duration // longest pause between two reads
	ended   bool          // server closed the response
	endedAt time.Duration // time from request start to end of response
}

// watchStream requests the stream and reads it until the server ends the
// response or `limit` elapses.
func watchStream(t *testing.T, srv *httptest.Server, path string, limit time.Duration) streamResult {
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

	var res streamResult
	buf := make([]byte, 64*1024)
	last := time.Now()
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if gap := time.Since(last); gap > res.maxGap {
				res.maxGap = gap
			}
			last = time.Now()
			res.bytes += int64(n)
		}
		if err != nil {
			if err == io.EOF || ctx.Err() == nil {
				res.ended = true
				res.endedAt = time.Since(start)
			}
			// Count the silence before the client gave up as a gap too.
			if gap := time.Since(last); gap > res.maxGap {
				res.maxGap = gap
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
		if r.URL.Path == "/second" {
			url = "stream?second-" + t.Name()
		}
		bufferingStream(testPlaylistID, url, backup, nil, nil, "chan"+r.URL.Path, w, r)
	}))
}

// Issue: on failover the buffer restarted its segment numbering at 1.ts while
// each viewer remembered the names it had already sent, so the viewer skipped
// the backup's segments (freeze) or was dropped mid-restart.
func TestBufferFailoverKeepsStreaming(t *testing.T) {
	setupBufferTest(t)

	srv := newBufferServer(t, "short", &BackupStream{PlaylistID: testPlaylistID, URL: "stream"})
	defer srv.Close()

	res := watchStream(t, srv, "/", 9*time.Second)
	t.Logf("received %d bytes, longest gap %v, ended=%v", res.bytes, res.maxGap, res.ended)

	if res.ended {
		t.Fatalf("stream ended after %v instead of failing over to the backup", res.endedAt)
	}
	if res.maxGap > 2*time.Second {
		t.Fatalf("viewer froze for %v during failover (want < 2s)", res.maxGap)
	}
}

// Issue: with no backup configured, the error was never passed to viewers, so
// they sat on a dead stream (holding a tuner) until the player gave up.
func TestBufferSourceExitEndsResponse(t *testing.T) {
	setupBufferTest(t)

	srv := newBufferServer(t, "short", nil)
	defer srv.Close()

	res := watchStream(t, srv, "/", 12*time.Second)
	t.Logf("received %d bytes, response ended after %v", res.bytes, res.endedAt)

	if !res.ended {
		t.Fatalf("response still open 12s after the source exited at ~3s")
	}
	if res.bytes == 0 {
		t.Fatalf("no data received before the source exited")
	}
}

// Issue: a source that stops sending without disconnecting blocked the
// buffer's read forever; the startup timeout could never fire.
func TestBufferStalledSourceEndsResponse(t *testing.T) {
	setupBufferTest(t)

	srv := newBufferServer(t, "hang", nil)
	defer srv.Close()

	res := watchStream(t, srv, "/", 30*time.Second)
	t.Logf("received %d bytes, response ended after %v", res.bytes, res.endedAt)

	if !res.ended {
		t.Fatalf("response still open 30s after the source stalled at ~1s")
	}
}

// Two channels from the same playlist share the Playlist maps. This passes on
// its own; run it with -race to see the unsynchronised map access that can
// crash the process ("concurrent map read and map write").
//
// The second tune is deliberately staggered. Tuning both at the same instant
// is a known, separate bug: both requests can see the playlist as inactive
// and create it, the second overwrites the first, and about 1 run in 6 leaves
// an ffmpeg process running after its viewer has gone.
func TestBufferTwoChannelsSamePlaylist(t *testing.T) {
	setupBufferTest(t)

	srv := newBufferServer(t, "stream", nil)
	defer srv.Close()

	var wg sync.WaitGroup
	results := make([]streamResult, 2)
	for i, path := range []string{"/", "/second"} {
		wg.Add(1)
		go func(i int, path string) {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 1500 * time.Millisecond)
			results[i] = watchStream(t, srv, path, 5*time.Second)
		}(i, path)
	}
	wg.Wait()

	for i, res := range results {
		if res.bytes == 0 {
			t.Fatalf("client %d received no data", i)
		}
	}
}
