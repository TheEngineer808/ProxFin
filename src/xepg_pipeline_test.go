package src

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"threadfin/src/internal/imgcache"
)

// usePipelineFolders points the files and caches the playlist/EPG pipeline
// uses at a temporary folder and restores them when the test ends.
func usePipelineFolders(t *testing.T) string {
	t.Helper()

	savedSystemFolder, savedSystemFile, savedGZ, savedInfo := System.Folder, System.File, System.Compressed.GZxml, System.Flag.Info
	savedImages, savedXMLTVCache := Data.Cache.Images, Data.Cache.XMLTV
	savedXEPG, savedXMLTV, savedStreams, savedFilter := Data.XEPG, Data.XMLTV, Data.Streams, Data.Filter
	t.Cleanup(func() {
		System.Folder, System.File, System.Compressed.GZxml, System.Flag.Info = savedSystemFolder, savedSystemFile, savedGZ, savedInfo
		Data.Cache.Images, Data.Cache.XMLTV = savedImages, savedXMLTVCache
		Data.XEPG, Data.XMLTV, Data.Streams, Data.Filter = savedXEPG, savedXMLTV, savedStreams, savedFilter
	})

	dir := t.TempDir()
	System.Flag.Info = true
	System.Folder.Data = dir + string(os.PathSeparator)
	System.Folder.ImagesCache = filepath.Join(dir, "images") + string(os.PathSeparator)
	if err := os.MkdirAll(System.Folder.ImagesCache, 0755); err != nil {
		t.Fatal(err)
	}
	System.File.XML = filepath.Join(dir, "threadfin.xml")
	System.Compressed.GZxml = filepath.Join(dir, "threadfin.xml.gz")
	System.File.XEPG = filepath.Join(dir, "xepg.json")
	System.File.URLS = filepath.Join(dir, "urls.json")

	images, err := imgcache.New(System.Folder.ImagesCache, "http://localhost/images/", false)
	if err != nil {
		t.Fatal(err)
	}
	Data.Cache.Images = images
	Data.Cache.XMLTV = make(map[string]XMLTV)

	return dir
}

// The .gz file was compressed before the last buffered part of the XML was
// written, so it lacked up to the last 1 MB, including </tv>.
func TestXMLTVGzipIsComplete(t *testing.T) {
	usePipelineFolders(t)

	Data.XMLTV.Files = []string{}
	Data.Streams.Active = []interface{}{map[string]string{"name": "x"}}
	Data.XEPG.Channels = make(map[string]interface{})
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("x-ID.%d", i)
		Data.XEPG.Channels[id] = XEPGChannelStruct{
			XEPG: id, XActive: true, XChannelID: fmt.Sprint(1000 + i), TvgChno: fmt.Sprint(1000 + i),
			XName: fmt.Sprintf("Channel %d", i), XmltvFile: "Threadfin Dummy", XMapping: "30_Minutes",
		}
	}

	if err := createXMLTVFile(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(System.File.XML)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 1<<20 {
		t.Fatalf("XML is only %d bytes; the test needs more than the 1 MB buffer", len(raw))
	}
	if !strings.HasSuffix(string(raw), "</tv>\n") {
		t.Fatal("XML file is not complete")
	}

	f, err := os.Open(System.Compressed.GZxml)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	unpacked, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unpacked, raw) {
		t.Fatalf("gzip holds %d bytes, XML file %d", len(unpacked), len(raw))
	}
}

// useSettings restores the settings when the test ends and sends saved
// settings to a temporary file.
func useSettings(t *testing.T, dir string) {
	t.Helper()
	savedSettings, savedSettingsFile, savedTemp := Settings, System.File.Settings, System.Folder.Temp
	t.Cleanup(func() {
		Settings, System.File.Settings, System.Folder.Temp = savedSettings, savedSettingsFile, savedTemp
	})
	System.File.Settings = filepath.Join(dir, "settings.json")
}

func writePlaylist(t *testing.T, id string, channels int) {
	t.Helper()
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for i := 0; i < channels; i++ {
		fmt.Fprintf(&b, "#EXTINF:-1 tvg-id=\"%s.%d\" tvg-name=\"%s %d\" group-title=\"%s\",%s %d\nhttp://provider.example/%s/%d.ts\n", id, i, id, i, id, id, i, id, i)
	}
	if err := os.WriteFile(System.Folder.Data+id+".m3u", []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
}

// A playlist that failed to load was removed from the list being ranged
// over: the next playlist was skipped and the last one loaded twice.
func TestBuildDatabaseDVRWithMissingPlaylist(t *testing.T) {
	dir := usePipelineFolders(t)
	useSettings(t, dir)

	Settings.Files.M3U = map[string]interface{}{}
	Settings.Files.HDHR = map[string]interface{}{}
	Settings.Filter = map[int64]interface{}{}
	Settings.IgnoreFilters = true
	for _, id := range []string{"M1", "M2", "M3", "M4", "M5", "M6"} {
		Settings.Files.M3U[id] = map[string]interface{}{"name": "Playlist " + id}
		if id != "M1" { // M1 has no local copy
			writePlaylist(t, id, 5)
		}
	}

	// The playlists are loaded in map order: repeat so that the missing
	// one is not always near the end (where the bug did no harm).
	for run := 0; run < 30; run++ {
		buildDatabaseDVR()

		perPlaylist := map[string]int{}
		for _, s := range Data.Streams.All {
			perPlaylist[s.(map[string]string)["_file.m3u.id"]]++
		}
		want := map[string]int{"M2": 5, "M3": 5, "M4": 5, "M5": 5, "M6": 5}
		if fmt.Sprint(perPlaylist) != fmt.Sprint(want) {
			t.Fatalf("run %d: streams per playlist %v, want %v", run, perPlaylist, want)
		}
		if len(Data.Streams.Active) != 25 {
			t.Fatalf("run %d: %d active streams, want 25", run, len(Data.Streams.Active))
		}
	}
}

// An XMLTV file that failed to load was not removed from Data.XMLTV.Files;
// the files after it were appended again instead.
func TestCreateXEPGMappingDropsMissingXMLTV(t *testing.T) {
	dir := usePipelineFolders(t)
	useSettings(t, dir)

	Settings.Files.XMLTV = map[string]interface{}{}
	for _, id := range []string{"X1", "X2", "X3"} {
		Settings.Files.XMLTV[id] = map[string]interface{}{"name": "Guide " + id}
	}
	for _, id := range []string{"X1", "X3"} { // X2 has no local copy
		content := fmt.Sprintf(`<?xml version="1.0"?><tv><channel id="%s.ch"><display-name>%s</display-name></channel></tv>`, id, id)
		if err := os.WriteFile(System.Folder.Data+id+".xml", []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	for run := 0; run < 8; run++ {
		Data.Cache.XMLTV = make(map[string]XMLTV)
		createXEPGMapping()

		var names []string
		for _, f := range Data.XMLTV.Files {
			names = append(names, filepath.Base(f))
		}
		if len(names) != 2 || strings.Contains(strings.Join(names, ","), "X2") {
			t.Fatalf("run %d: XMLTV files %v, want X1.xml and X3.xml", run, names)
		}
		for _, f := range []string{"X1.xml", "X3.xml", "Threadfin Dummy"} {
			if _, ok := Data.XMLTV.Mapping[f]; !ok {
				t.Fatalf("run %d: mapping lacks %s", run, f)
			}
		}
	}
}
