package imgcache

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestCache(t *testing.T) (*Cache, string) {
	t.Helper()
	dir := t.TempDir() + string(os.PathSeparator)
	c, err := New(dir, "http://threadfin:34400/images/", true)
	if err != nil {
		t.Fatal(err)
	}
	return c, dir
}

func getURL(c *Cache, src string) string {
	return c.Image.GetURL(src, "", "34400", false, 443, "")
}

// Images are downloaded into the cache and then served from it; failed
// downloads stay queued and leave nothing behind in the cache folder.
func TestCachingDownloadsQueuedImages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("PNG" + r.URL.Path))
	}))
	defer srv.Close()

	c, dir := newTestCache(t)

	good, missing := srv.URL+"/logo.png?token=1", srv.URL+"/missing.png"
	if got := getURL(c, good); got != good {
		t.Fatalf("an uncached image must be served from its source, got %q", got)
	}
	getURL(c, good) // queued only once
	getURL(c, missing)
	if len(c.Queue) != 2 {
		t.Fatalf("expected 2 queued images, got %v", c.Queue)
	}

	c.Image.Caching()

	name := strToMD5(srv.URL+"/logo.png") + ".png"
	if got, want := getURL(c, good), "http://threadfin:34400/images/"+name; got != want {
		t.Errorf("cached image URL: got %q, want %q", got, want)
	}
	if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != "PNG/logo.png" {
		t.Errorf("cached file: %q, %v", b, err)
	}
	if len(c.Queue) != 1 || c.Queue[0] != missing {
		t.Errorf("only the failed image should stay queued, got %v", c.Queue)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected only the cached image in the folder, found %d entries", len(entries))
	}
}

// One image host that accepts the connection and never answers used to
// stall the whole cache with its lock held, blocking every GetURL (and so
// /m3u/ requests and the XMLTV build) until a restart.
func TestStalledImageHostDoesNotBlockLookups(t *testing.T) {
	saved := downloadTimeout
	downloadTimeout = 2 * time.Second
	t.Cleanup(func() { downloadTimeout = saved })

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	c, _ := newTestCache(t)
	getURL(c, srv.URL+"/stalled.png")

	done := make(chan struct{})
	go func() {
		c.Image.Caching()
		close(done)
	}()
	time.Sleep(200 * time.Millisecond) // the download is now in progress

	start := time.Now()
	for i := 0; i < 100; i++ {
		getURL(c, srv.URL+"/other.png")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("GetURL was blocked for %v by a download in progress", d)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Caching did not give up on a stalled image host")
	}
}
