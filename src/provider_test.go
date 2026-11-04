package src

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func shortDownloadTimeouts(t *testing.T) {
	c, r, i := downloadConnectTimeout, downloadResponseTimeout, downloadIdleTimeout
	downloadConnectTimeout, downloadResponseTimeout, downloadIdleTimeout = time.Second, 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { downloadConnectTimeout, downloadResponseTimeout, downloadIdleTimeout = c, r, i })
}

// A large file on a slow connection takes longer than any timeout, but
// data keeps arriving: it must download completely.
func TestDownloadSlowButSteadyFileSucceeds(t *testing.T) {
	shortDownloadTimeouts(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 15; i++ {
			w.Write([]byte(strings.Repeat("x", 1000)))
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()

	start := time.Now()
	_, body, err := downloadFileFromServer(srv.URL+"/list.m3u", "")
	if err != nil {
		t.Fatalf("download failed after %v: %v", time.Since(start), err)
	}
	if len(body) != 15000 {
		t.Fatalf("got %d bytes, want 15000", len(body))
	}
}

func TestDownloadStalledServerFails(t *testing.T) {
	shortDownloadTimeouts(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()

	start := time.Now()
	_, _, err := downloadFileFromServer(srv.URL+"/list.m3u", "")
	if err == nil || !strings.Contains(err.Error(), "no data received") {
		t.Fatalf("err = %v, want a stall error", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("stall detected after %v", d)
	}
}

func TestDownloadUnresponsiveServerFails(t *testing.T) {
	shortDownloadTimeouts(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()

	start := time.Now()
	if _, _, err := downloadFileFromServer(srv.URL+"/list.m3u", ""); err == nil {
		t.Fatalf("download of an unresponsive server succeeded")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("failed only after %v", d)
	}
}
