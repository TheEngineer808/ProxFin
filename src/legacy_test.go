package src

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func useAppName(t *testing.T, name string) {
	t.Helper()
	saved := System.AppName
	System.AppName = name
	t.Cleanup(func() { System.AppName = saved })
}

// Switching from Threadfin: with no ProxFin folder yet, its settings
// folder is used, so playlists, mappings and users are all kept.
func TestDefaultConfigFolderPicksUpThreadfin(t *testing.T) {
	useAppName(t, "proxfin")
	sep := string(os.PathSeparator)

	home := t.TempDir()
	if folder, legacy := defaultConfigFolderIn(home); folder != filepath.Join(home, ".proxfin")+sep || legacy {
		t.Errorf("new install: %s, %v", folder, legacy)
	}

	os.MkdirAll(filepath.Join(home, ".threadfin"), 0755)
	if folder, legacy := defaultConfigFolderIn(home); folder != filepath.Join(home, ".threadfin")+sep || !legacy {
		t.Errorf("Threadfin install: %s, %v", folder, legacy)
	}

	os.MkdirAll(filepath.Join(home, ".proxfin"), 0755)
	if folder, legacy := defaultConfigFolderIn(home); folder != filepath.Join(home, ".proxfin")+sep || legacy {
		t.Errorf("both folders: %s, %v", folder, legacy)
	}
}

// DVR clients set up with Threadfin fetch /m3u/threadfin.m3u and
// /xmltv/threadfin.xml(.gz). Those URLs keep working, and the new names
// work even before ProxFin has written its files.
func TestThreadfinURLsStillServed(t *testing.T) {
	useAppName(t, "proxfin")
	dir := withDataFolder(t)
	write := func(name, content string) { os.WriteFile(filepath.Join(dir, name), []byte(content), 0644) }
	read := func(path string) (int, string) {
		resp := get(t, path, nil)
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Only Threadfin's files exist (the first start after switching).
	write("threadfin.xml", "<tv>old</tv>")
	write("threadfin.m3u", "#EXTM3U\n#EXTINF:-1,Old\nhttp://old\n")
	for _, path := range []string{"/xmltv/threadfin.xml", "/xmltv/proxfin.xml"} {
		if code, body := read(path); code != 200 || body != "<tv>old</tv>" {
			t.Errorf("%s before the first build: %d %q", path, code, body)
		}
	}
	if code, body := read("/m3u/threadfin.m3u"); code != 200 || body != "#EXTM3U\n#EXTINF:-1,Old\nhttp://old\n" {
		t.Errorf("/m3u/threadfin.m3u before the first build: %d %q", code, body)
	}

	// ProxFin has written its files: both names serve them.
	write("proxfin.xml", "<tv>new</tv>")
	write("proxfin.xml.gz", "gz")
	write("proxfin.m3u", "#EXTM3U\n#EXTINF:-1,New\nhttp://new\n")
	for _, path := range []string{"/xmltv/threadfin.xml", "/xmltv/proxfin.xml"} {
		if code, body := read(path); code != 200 || body != "<tv>new</tv>" {
			t.Errorf("%s: %d %q", path, code, body)
		}
	}
	if code, body := read("/xmltv/threadfin.xml.gz"); code != 200 || body != "gz" {
		t.Errorf("/xmltv/threadfin.xml.gz: %d %q", code, body)
	}
	for _, path := range []string{"/m3u/threadfin.m3u", "/m3u/proxfin.m3u", "/m3u/proxfin.m3u?username=a&password=b"} {
		if code, body := read(path); code != 200 || body != "#EXTM3U\n#EXTINF:-1,New\nhttp://new\n" {
			t.Errorf("%s: %d %q", path, code, body)
		}
	}

	if code, _ := read("/xmltv/other.xml"); code != http.StatusNotFound {
		t.Errorf("unknown file: %d", code)
	}
}

// Channels Threadfin mapped to its dummy guide keep their mapping.
func TestLegacyDummyGuideIsMigrated(t *testing.T) {
	channels := map[string]interface{}{
		"x-ID.1": map[string]interface{}{"x-xmltv-file": "Threadfin Dummy", "x-mapping": "30_Minutes"},
		"x-ID.2": map[string]interface{}{"x-xmltv-file": "guide.xml"},
		"x-ID.3": map[string]interface{}{"x-xmltv-file": dummyXMLTVFile},
	}
	if !migrateLegacyDummy(channels) {
		t.Fatal("no channel was migrated")
	}
	want := map[string]string{"x-ID.1": dummyXMLTVFile, "x-ID.2": "guide.xml", "x-ID.3": dummyXMLTVFile}
	for id, file := range want {
		if got := channels[id].(map[string]interface{})["x-xmltv-file"]; got != file {
			t.Errorf("%s: %v, want %s", id, got, file)
		}
	}
	if migrateLegacyDummy(channels) {
		t.Error("a second migration changed something")
	}
	if !isDummyXMLTVFile("Threadfin Dummy") || !isDummyXMLTVFile(dummyXMLTVFile) || isDummyXMLTVFile("guide.xml") {
		t.Error("isDummyXMLTVFile")
	}
}

// Playlist and guide entries in settings.json name their local copy under
// Threadfin's key; a refresh must keep using it.
func TestProviderFileKeyIsKept(t *testing.T) {
	if providerFileKey != "file.threadfin" {
		t.Fatalf("providerFileKey changed to %q: existing settings.json files use \"file.threadfin\"", providerFileKey)
	}
}
