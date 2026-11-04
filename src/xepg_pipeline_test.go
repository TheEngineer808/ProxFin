package src

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"threadfin/src/internal/imgcache"
)

// usePipelineFolders points the files and caches the playlist/EPG pipeline
// uses at a temporary folder and restores them when the test ends.
//
// (The tests using it are in xepg_* files so that they run after the
// buffer tests, whose streams linger for a moment and read the globals.)
func usePipelineFolders(t testing.TB) string {
	t.Helper()

	// The log is silenced; System.Flag.Info is only written if needed, as
	// lingering goroutines of other tests read it.
	if !System.Flag.Info {
		System.Flag.Info = true
		t.Cleanup(func() { System.Flag.Info = false })
	}

	savedSystemFolder, savedSystemFile, savedGZ := System.Folder, System.File, System.Compressed.GZxml
	savedImages, savedXMLTVCache := Data.Cache.Images, Data.Cache.XMLTV
	savedXEPG, savedXMLTV, savedStreams, savedFilter := Data.XEPG, Data.XMLTV, Data.Streams, Data.Filter
	t.Cleanup(func() {
		System.Folder, System.File, System.Compressed.GZxml = savedSystemFolder, savedSystemFile, savedGZ
		Data.Cache.Images, Data.Cache.XMLTV = savedImages, savedXMLTVCache
		Data.XEPG, Data.XMLTV, Data.Streams, Data.Filter = savedXEPG, savedXMLTV, savedStreams, savedFilter
		programIndexMu.Lock()
		programIndex = make(map[string]*xmltvProgramIndex)
		programIndexMu.Unlock()
	})

	dir := t.TempDir()
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
	clearXMLTVCache()

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
func useSettings(t testing.TB, dir string) {
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
		clearXMLTVCache()
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

// testGuide returns an XMLTV guide with programmes for several channels, in
// interleaved order, with the fields getProgramData copies or rewrites.
func testGuide(channels, perChannel int) XMLTV {
	var guide XMLTV
	for c := 0; c < channels; c++ {
		guide.Channel = append(guide.Channel, &Channel{ID: fmt.Sprintf("ch%d.example", c), DisplayName: []DisplayName{{Value: fmt.Sprintf("Channel %d", c)}}})
	}
	for p := 0; p < perChannel; p++ {
		for c := 0; c < channels; c++ {
			prog := &Program{
				Channel:  fmt.Sprintf("ch%d.example", c),
				Start:    fmt.Sprintf("20261104%02d0000 +0000", p%24),
				Stop:     fmt.Sprintf("20261104%02d3000 +0000", p%24),
				Title:    []*Title{{Lang: "en", Value: fmt.Sprintf("Show %d – Ünïcode %d", p, c)}},
				Desc:     []*Desc{{Lang: "en", Value: "Description"}},
				Category: []*Category{{Lang: "en", Value: "Series"}},
				Video:    Video{Quality: map[bool]string{true: "HDTV", false: ""}[p%2 == 0]},
			}
			if p%3 == 0 {
				prog.Poster = []Poster{{Src: fmt.Sprintf("http://img.example/%d/%d.jpg", c, p)}}
			}
			if p%4 == 0 {
				prog.EpisodeNum = []*EpisodeNum{{System: "xmltv_ns", Value: fmt.Sprintf("1.%d.", p)}}
			}
			guide.Program = append(guide.Program, prog)
		}
	}
	return guide
}

// Indexing the programmes by channel must return what the scan over all
// programmes returned, and follow the cache when a file is reloaded.
func TestGetProgramDataMatchesLegacy(t *testing.T) {
	dir := usePipelineFolders(t)
	useSettings(t, dir)
	Settings.EnableNonAscii = false
	Settings.XepgReplaceMissingImages = true
	Settings.Filter = map[int64]interface{}{0: map[string]interface{}{"filter": "News", "type": "group-title"}}

	file := System.Folder.Data + "X1.xml"
	Data.Cache.XMLTV[file] = testGuide(12, 30)

	var channels []XEPGChannelStruct
	for c := 0; c < 14; c++ { // ch12 and ch13 have no programmes
		channels = append(channels, XEPGChannelStruct{
			XmltvFile: "X1.xml", XMapping: fmt.Sprintf("ch%d.example", c), XChannelID: fmt.Sprint(100 + c),
			XName: fmt.Sprintf("Channel %d HD", c), TvgLogo: "http://logo.example/l.png", XCategory: map[bool]string{true: "Sports", false: ""}[c%2 == 0],
		})
	}
	channels = append(channels,
		XEPGChannelStruct{XmltvFile: "Threadfin Dummy", XMapping: "60_Minutes", XChannelID: "500", XName: "Dummy"},
		XEPGChannelStruct{XmltvFile: "", XMapping: "ch1.example"},
		XEPGChannelStruct{XmltvFile: "missing.xml", XMapping: "ch1.example"},
	)

	compare := func(stage string) {
		t.Helper()
		for _, ch := range channels {
			got, gotErr := getProgramData(ch)
			want, wantErr := legacyGetProgramData(ch)
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("%s: %s/%s: error %v, legacy %v", stage, ch.XmltvFile, ch.XMapping, gotErr, wantErr)
			}
			if ch.XmltvFile == "Threadfin Dummy" { // start times depend on the clock
				if len(got.Program) != len(want.Program) {
					t.Fatalf("%s: dummy: %d programmes, legacy %d", stage, len(got.Program), len(want.Program))
				}
				continue
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: %s/%s differs from legacy:\n got  %d programmes\n want %d programmes", stage, ch.XmltvFile, ch.XMapping, len(got.Program), len(want.Program))
			}
		}
	}

	compare("first load")
	if n := len(programIndex[file].byChannel["ch3.example"]); n != 30 {
		t.Fatalf("index holds %d programmes for ch3, want 30", n)
	}

	// The file is updated and parsed again: the index must not be stale.
	Data.Cache.XMLTV[file] = testGuide(3, 5)
	compare("reloaded")
	if got, _ := getProgramData(channels[1]); len(got.Program) != 5 {
		t.Fatalf("after reload: %d programmes, want 5", len(got.Program))
	}

	clearXMLTVCache()
	if len(programIndex) != 0 || len(Data.Cache.XMLTV) != 0 {
		t.Fatal("clearXMLTVCache left data behind")
	}
}

func benchmarkProgramData(b *testing.B, get func(XEPGChannelStruct) (XMLTV, error)) {
	images, _ := imgcache.New(b.TempDir(), "http://localhost/images/", false)
	savedImages, savedCache, savedFilter, savedInfo := Data.Cache.Images, Data.Cache.XMLTV, Settings.Filter, System.Flag.Info
	b.Cleanup(func() {
		clearXMLTVCache()
		Data.Cache.Images, Data.Cache.XMLTV, Settings.Filter, System.Flag.Info = savedImages, savedCache, savedFilter, savedInfo
	})
	System.Flag.Info = true // benchmarks run alone
	Data.Cache.Images = images
	clearXMLTVCache()
	Settings.Filter = map[int64]interface{}{}
	for i := 0; i < 20; i++ {
		Settings.Filter[int64(i)] = map[string]interface{}{"filter": fmt.Sprintf("Group %d", i), "type": "group-title", "active": true}
	}
	file := System.Folder.Data + "bench.xml"
	Data.Cache.XMLTV[file] = testGuide(1000, 50) // 50k programmes
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for c := 0; c < 200; c++ { // 200 mapped channels
			get(XEPGChannelStruct{XmltvFile: "bench.xml", XMapping: fmt.Sprintf("ch%d.example", c), XChannelID: fmt.Sprint(c)})
		}
	}
}

func BenchmarkGetProgramData(b *testing.B)       { benchmarkProgramData(b, getProgramData) }
func BenchmarkGetProgramDataLegacy(b *testing.B) { benchmarkProgramData(b, legacyGetProgramData) }

// A dummy length of 0 looped forever, adding programmes until memory ran
// out. Other lengths are unchanged.
func TestDummyProgramLengths(t *testing.T) {
	usePipelineFolders(t)

	for mapping, want := range map[string]int{
		"30_Minutes": 4 * 48, "60_Minutes": 4 * 24, "360_Minutes": 4 * 4,
		"0_Minutes": 4 * 48, "00_Minutes": 4 * 48, "99999999999999999999_Minutes": 0,
		"abc_Minutes": 4 * 48, "2000_Minutes": 0,
	} {
		done := make(chan int, 1)
		go func() { done <- len(createDummyProgram(XEPGChannelStruct{XMapping: mapping, XName: "Dummy"}).Program) }()
		select {
		case got := <-done:
			if got != want {
				t.Errorf("%s: %d programmes, want %d", mapping, got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: no result after 10 s", mapping)
		}
	}
}
