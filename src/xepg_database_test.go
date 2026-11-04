package src

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
)

// legacyIsInInactiveList is isInInactiveList as it was (copied verbatim).
func legacyIsInInactiveList(channelURL string) bool {
	for _, channel := range Data.Streams.Inactive {
		// Type assert channel to map[string]interface{}
		chMap, ok := channel.(map[string]interface{})
		if !ok {
			continue
		}

		urlValue, exists := chMap["url"]
		if !exists {
			continue
		}

		urlStr, ok := urlValue.(string)
		if !ok {
			continue
		}

		if urlStr == channelURL {
			return true
		}
	}
	return false
}

// legacyFindByUUID is the full scan createXEPGDatabase did for a stream
// whose hash was not in the database (logic copied verbatim).
func legacyFindByUUID(xepgChannelsValuesMap map[string]XEPGChannelStruct, m3uChannel M3UChannelStructXEPG) (currentXEPGID string, channelExists bool) {
	for _, dxc := range xepgChannelsValuesMap {
		if m3uChannel.FileM3UID == dxc.FileM3UID && !legacyIsInInactiveList(dxc.URL) {

			dxc.FileM3UID = m3uChannel.FileM3UID
			dxc.FileM3UName = m3uChannel.FileM3UName

			if len(dxc.UUIDValue) > 0 && len(m3uChannel.UUIDValue) > 0 {
				if dxc.UUIDValue == m3uChannel.UUIDValue {

					channelExists = true
					currentXEPGID = dxc.XEPG
					break

				}
			}
		}

	}
	return
}

func TestUUIDMatcherMatchesLegacy(t *testing.T) {
	saved := Data.Streams.Inactive
	t.Cleanup(func() { Data.Streams.Inactive = saved })

	rnd := rand.New(rand.NewSource(5))
	files := []string{"M1", "M2", "M3"}
	values := map[string]XEPGChannelStruct{}
	for i := 0; i < 600; i++ {
		ch := XEPGChannelStruct{XEPG: fmt.Sprintf("x-ID.%d", i), FileM3UID: files[i%3], URL: fmt.Sprintf("http://p/%d", i)}
		if i%5 != 0 {
			ch.UUIDValue = fmt.Sprintf("Channel %d", i) // unique per playlist
		}
		values[fmt.Sprintf("hash%d", i)] = ch
	}
	// inactive streams as buildDatabaseDVR stores them; none is in the
	// database, so the old (never matching) check makes no difference
	Data.Streams.Inactive = nil
	for i := 0; i < 300; i++ {
		Data.Streams.Inactive = append(Data.Streams.Inactive, map[string]string{"url": fmt.Sprintf("http://inactive/%d", i)})
	}

	matcher := newUUIDMatcher(values, Data.Streams.Inactive)
	found := 0
	for i := 0; i < 2000; i++ {
		m3u := M3UChannelStructXEPG{FileM3UID: files[rnd.Intn(3)], UUIDValue: fmt.Sprintf("Channel %d", rnd.Intn(700))}
		if i%10 == 0 {
			m3u.UUIDValue = ""
		}
		wantID, wantOK := legacyFindByUUID(values, m3u)
		got, gotOK := matcher.find(m3u.FileM3UID, m3u.UUIDValue)
		if gotOK != wantOK || got.XEPG != wantID {
			t.Fatalf("%+v: got (%s, %v), want (%s, %v)", m3u, got.XEPG, gotOK, wantID, wantOK)
		}
		if gotOK {
			found++
		}
	}
	if found == 0 {
		t.Fatal("no stream matched")
	}

	// channels added while the database is updated are found too
	matcher.add(XEPGChannelStruct{XEPG: "x-ID.900", FileM3UID: "M1", UUIDValue: "New", URL: "http://p/new"})
	if got, ok := matcher.find("M1", "New"); !ok || got.XEPG != "x-ID.900" {
		t.Fatalf("added channel not found: %v %v", got, ok)
	}
}

// A database channel whose URL is now a filtered-out stream must not be
// matched. The old check never matched because the inactive streams are
// map[string]string.
func TestUUIDMatcherSkipsInactiveStreams(t *testing.T) {
	saved := Data.Streams.Inactive
	t.Cleanup(func() { Data.Streams.Inactive = saved })

	values := map[string]XEPGChannelStruct{
		"h1": {XEPG: "x-ID.1", FileM3UID: "M1", UUIDValue: "News", URL: "http://p/filtered"},
		"h2": {XEPG: "x-ID.2", FileM3UID: "M1", UUIDValue: "News", URL: "http://p/active"},
	}
	Data.Streams.Inactive = []interface{}{map[string]string{"url": "http://p/filtered", "name": "News"}}

	if !legacyIsInInactiveList("http://p/active") && legacyIsInInactiveList("http://p/filtered") {
		t.Fatal("expected the old check to never match")
	}
	got, ok := newUUIDMatcher(values, Data.Streams.Inactive).find("M1", "News")
	if !ok || got.XEPG != "x-ID.2" {
		t.Fatalf("got %v %v, want x-ID.2", got.XEPG, ok)
	}

	Data.Streams.Inactive = []interface{}{map[string]interface{}{"url": "http://p/active"}, map[string]string{"url": "http://p/filtered"}}
	if _, ok := newUUIDMatcher(values, Data.Streams.Inactive).find("M1", "News"); ok {
		t.Fatal("matched a channel whose URL is inactive")
	}
}

// testStreams returns streams as buildDatabaseDVR stores them. Without a
// tvg-id in the playlist the parser derives it from the URL.
func testStreams(n int, urlPrefix string) (streams []interface{}) {
	for i := 0; i < n; i++ {
		url := fmt.Sprintf("%s/%d.ts", urlPrefix, i)
		name := fmt.Sprintf("Channel %d", i)
		streams = append(streams, map[string]string{
			"_file.m3u.id": "M1", "_file.m3u.name": "Playlist", "_file.m3u.path": "/tmp/M1.m3u",
			"group-title": fmt.Sprintf("Group %d", i%7), "name": name, "tvg-name": name,
			"tvg-id": fmt.Sprintf("threadfin-%x", md5.Sum([]byte(url))), "url": url,
			"_uuid.key": "tvg-name", "_uuid.value": name, "_values": name, "liveEvent": "false",
		})
	}
	return
}

func setupXEPGDatabase(t testing.TB) {
	dir := usePipelineFolders(t)
	useSettings(t, dir)
	Settings.MappingFirstChannel = 1000
	Settings.Filter = map[int64]interface{}{}
	if err := saveSettings(Settings); err != nil {
		t.Fatal(err)
	}
	if err := saveMapToJSONFile(System.File.XEPG, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	Data.XMLTV.Mapping = map[string]interface{}{}
}

func xepgDatabase(t testing.TB) map[string]XEPGChannelStruct {
	t.Helper()
	db, err := loadJSONFileToMap(System.File.XEPG)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]XEPGChannelStruct{}
	for id, v := range db {
		var ch XEPGChannelStruct
		if err := jsonUnmarshalString(mapToJSON(v), &ch); err != nil {
			t.Fatal(err)
		}
		out[id] = ch
	}
	return out
}

// First import, unchanged update, and an update in which every URL
// changed (tokens): IDs and numbers are assigned in playlist order and kept.
func TestCreateXEPGDatabaseKeepsChannels(t *testing.T) {
	setupXEPGDatabase(t)

	Data.Streams.Active = testStreams(60, "http://provider/a")
	Data.Streams.Inactive = testStreams(40, "http://provider/inactive")
	for run := 0; run < 2; run++ {
		if err := createXEPGDatabase(); err != nil {
			t.Fatal(err)
		}
		db := xepgDatabase(t)
		if len(db) != 60 {
			t.Fatalf("run %d: %d channels, want 60", run, len(db))
		}
		for i := 0; i < 60; i++ {
			ch := db[fmt.Sprintf("x-ID.%d", i)]
			if ch.XName != fmt.Sprintf("Channel %d", i) || ch.XChannelID != fmt.Sprint(1000+i) {
				t.Fatalf("run %d: x-ID.%d is %q number %s, want Channel %d number %d", run, i, ch.XName, ch.XChannelID, i, 1000+i)
			}
		}
	}

	// activate the channels, then change every URL
	db, _ := loadJSONFileToMap(System.File.XEPG)
	for _, v := range db {
		v.(map[string]interface{})["x-active"] = true
	}
	saveMapToJSONFile(System.File.XEPG, db)

	Data.Streams.Active = testStreams(60, "http://provider/b")
	if err := createXEPGDatabase(); err != nil {
		t.Fatal(err)
	}
	after := xepgDatabase(t)
	if len(after) != 60 {
		t.Fatalf("after the URL change: %d channels, want 60", len(after))
	}
	for i := 0; i < 60; i++ {
		ch := after[fmt.Sprintf("x-ID.%d", i)]
		if ch.URL != fmt.Sprintf("http://provider/b/%d.ts", i) || ch.XChannelID != fmt.Sprint(1000+i) {
			t.Fatalf("x-ID.%d: URL %s number %s", i, ch.URL, ch.XChannelID)
		}
	}
}

// First import of 2,000 channels from a playlist with 20,000 filtered-out
// streams.
func BenchmarkCreateXEPGDatabaseFirstImport(b *testing.B) {
	setupXEPGDatabase(b)
	Data.Streams.Active = testStreams(2000, "http://provider/a")
	Data.Streams.Inactive = testStreams(20000, "http://provider/inactive")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		saveMapToJSONFile(System.File.XEPG, map[string]interface{}{})
		b.StartTimer()
		if err := createXEPGDatabase(); err != nil {
			b.Fatal(err)
		}
	}
}

func jsonUnmarshalString(s string, v interface{}) error {
	return json.Unmarshal([]byte(s), v)
}
