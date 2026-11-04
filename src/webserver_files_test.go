package src

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withDataFolder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir() + string(os.PathSeparator)
	saved, savedDev, savedInfo := System.Folder.Data, System.Dev, System.Flag.Info
	System.Folder.Data, System.Dev, System.Flag.Info = dir, false, true
	t.Cleanup(func() { System.Folder.Data, System.Dev, System.Flag.Info = saved, savedDev, savedInfo })
	return dir
}

func get(t *testing.T, path string, header http.Header) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	Threadfin(rec, req)
	return rec.Result()
}

// The guide is streamed from disk (it used to be held in memory four times
// per request) with the same content types as before, and conditional and
// range requests now work.
func TestXMLTVIsServedFromDisk(t *testing.T) {
	dir := withDataFolder(t)

	xml := `<?xml version="1.0" encoding="UTF-8"?><tv><channel id="a"/></tv>`
	os.WriteFile(filepath.Join(dir, "threadfin.xml"), []byte(xml), 0644)
	f, _ := os.Create(filepath.Join(dir, "threadfin.xml.gz"))
	zw := gzip.NewWriter(f)
	zw.Write([]byte(xml))
	zw.Close()
	f.Close()

	resp := get(t, "/xmltv/threadfin.xml", nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != xml {
		t.Fatalf("xml: %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Errorf("xml content type %q", ct)
	}

	resp = get(t, "/xmltv/threadfin.xml.gz", nil)
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if body, _ = io.ReadAll(zr); string(body) != xml {
		t.Errorf("gz content %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-gzip" {
		t.Errorf("gz content type %q", ct)
	}

	if resp = get(t, "/xmltv/threadfin.xml", http.Header{"Range": {"bytes=0-4"}}); resp.StatusCode != http.StatusPartialContent {
		t.Errorf("range request: %d", resp.StatusCode)
	}

	if resp = get(t, "/xmltv/missing.xml", nil); resp.StatusCode != 404 {
		t.Errorf("missing file: %d", resp.StatusCode)
	}
}

// Any query string without group-title (e.g. username and password with
// M3U authentication on) used to return an empty playlist.
func TestM3UWithQueryParametersServesThePlaylist(t *testing.T) {
	dir := withDataFolder(t)

	m3u := "#EXTM3U\n#EXTINF:-1,Channel\nhttp://example/stream\n"
	os.WriteFile(filepath.Join(dir, "threadfin.m3u"), []byte(m3u), 0644)

	for _, path := range []string{"/m3u/threadfin.m3u", "/m3u/threadfin.m3u?username=a&password=b"} {
		resp := get(t, path, nil)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(body) != m3u {
			t.Errorf("%s: %d %q", path, resp.StatusCode, body)
		}
	}
}

// If xepg.json can't be read, the PPV endpoints used to save "null" over
// it, deleting every channel mapping.
func TestPPVDoesNotOverwriteAnUnreadableXEPGFile(t *testing.T) {
	saved := System.File.XEPG
	System.File.XEPG = filepath.Join(t.TempDir(), "xepg.json")
	t.Cleanup(func() { System.File.XEPG = saved })

	const corrupt = `{"x-ID.1": {"x-mapping": "PPV"` // truncated
	os.WriteFile(System.File.XEPG, []byte(corrupt), 0644)

	for name, handler := range map[string]http.HandlerFunc{"enable": enablePPV, "disable": disablePPV} {
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "/ppv/"+name, nil))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: expected 500, got %d", name, rec.Code)
		}
		if b, _ := os.ReadFile(System.File.XEPG); string(b) != corrupt {
			t.Fatalf("%s overwrote xepg.json with %q", name, strings.TrimSpace(string(b)))
		}
	}
}
