package m3u

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Entries covering the formats seen in provider playlists. None has more
// than one key containing "tvg-name": the legacy parser picked between those
// at random.
var parityEntries = []string{
	`#EXTINF:-1 tvg-id="ch%[1]d.example" tvg-name="Channel %[1]d HD" tvg-logo="http://logo.example/%[1]d.png" group-title="Group %[2]d",Channel %[1]d HD` + "\n" +
		"http://provider.example:8080/live/user/pass/%[1]d.ts\n",
	// single-quoted attributes
	`#EXTINF:-1 tvg-id='q%[1]d.example' tvg-name='Quoted %[1]d' group-title='Quoted Group',Quoted %[1]d` + "\n" +
		"http://provider.example/q/%[1]d\n",
	// CRLF line endings, extra # lines between EXTINF and URL
	`#EXTINF:0 channelID="%[1]d" tvg-chno="%[1]d" tvg-name="CRLF.%[1]d" tvg-id="crlf.%[1]d" group-title="CRLF", CRLF %[1]d  ` + "\r\n" +
		"#EXTGRP:Something\r\n#EXTVLCOPT:http-user-agent=Foo\r\n" +
		"http://provider.example/crlf/%[1]d\r\n",
	// no tvg-id, upper-case TVG keys, a URL attribute
	`#EXTINF:-1 TVG-NAME="Upper %[1]d" TVG-LOGO="https://logo.example/u.png" group-title="Upper",Upper %[1]d` + "\n" +
		"http://provider.example/upper/%[1]d\n",
	// "(no tvg-id)", empty attribute values
	`#EXTINF:-1 tvg-id="(no tvg-id)" tvg-name="" group-title="",Empty attrs %[1]d` + "\n" +
		"http://provider.example/empty/%[1]d\n",
	// no name: falls back to tvg-name
	`#EXTINF:-1 tvg-name="Fallback %[1]d" group-title="Fallback",` + "\n" +
		"http://provider.example/fallback/%[1]d\n",
	// no name at all: kept without name
	`#EXTINF:-1 group-title="Nameless",` + "\n" +
		"http://provider.example/nameless/%[1]d\n",
	// no URL: dropped
	`#EXTINF:-1 tvg-name="No URL %[1]d",No URL %[1]d` + "\n\n",
	// commas and quotes in the name, ":-1" inside a value, catchup attributes
	`#EXTINF:-1 tvg-id="c%[1]d" tvg-name="Comma, %[1]d" catchup="default" catchup-days="7" group-title="News::-1-1",Comma, Name %[1]d` + "\n" +
		"http://provider.example/c/%[1]d?token=a:-1b'c\n",
	// tvg-shift and other custom attributes, blank line before the URL
	`#EXTINF:-1 tvg-shift="-2" x-custom="v%[1]d" tvg-name="Shift %[1]d",Shift %[1]d` + "\n\n" +
		"http://provider.example/shift/%[1]d\n",
}

func buildPlaylist(entries int) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U url-tvg=\"http://example.com/epg.xml\"\n")
	for i := 0; i < entries; i++ {
		fmt.Fprintf(&b, parityEntries[i%len(parityEntries)], i, i%300)
	}
	return []byte(b.String())
}

func comparePlaylists(t *testing.T, content []byte) []interface{} {
	t.Helper()

	got, err := MakeInterfaceFromM3U(content)
	if err != nil {
		t.Fatal(err)
	}
	want, wantErr := legacyMakeInterfaceFromM3U(content)
	if wantErr != nil {
		t.Fatal(wantErr)
	}

	if len(got) != len(want) {
		t.Fatalf("got %d channels, legacy parser %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("channel %d differs:\n got  %v\n want %v", i, got[i], want[i])
		}
	}
	return got
}

func TestParserMatchesLegacySmall(t *testing.T) {
	channels := comparePlaylists(t, buildPlaylist(len(parityEntries)*3))

	// spot checks of the formats above (names are only trimmed on the right)
	quoted := channels[1].(map[string]string)
	if quoted["tvg-id"] != "q1.example" || quoted["group-title"] != "Quoted Group" || quoted["name"] != "Quoted 1" {
		t.Errorf("single-quoted attributes not parsed: %v", quoted)
	}
	crlf := channels[2].(map[string]string)
	if crlf["url"] != "http://provider.example/crlf/2" || crlf["name"] != " CRLF 2" {
		t.Errorf("CRLF entry: %v", crlf)
	}
	if _, ok := channels[6].(map[string]string)["name"]; ok {
		t.Errorf("nameless entry got a name: %v", channels[6])
	}
}

func TestParserMatchesLegacyTestFile(t *testing.T) {
	content := []byte(strings.Join([]string{
		"#EXTM3U",
		`#EXTINF:0 channelID="1" tvg-chno="1" tvg-name="Channel.1" tvg-id="tvg.id.1" group-title="Group 1", Channel 1`,
		"http://example.com/stream/1",
		"#EXTINF:123, Sample artist - Sample title",
		"http://example.com/stream/3",
		"",
	}, "\n"))
	comparePlaylists(t, content)
}

// Playlists over 10 MB went to a parser that returned no channels. The
// result must not depend on the size of the playlist.
func TestParserLargePlaylist(t *testing.T) {
	if testing.Short() {
		t.Skip("large playlist")
	}

	const entries = 90000
	content := buildPlaylist(entries)
	if len(content) <= 10*1024*1024 {
		t.Fatalf("playlist is only %d bytes, want > 10 MB", len(content))
	}

	start := time.Now()
	channels := comparePlaylists(t, content)
	t.Logf("%d channels from %.1f MB (both parsers: %v)", len(channels), float64(len(content))/1e6, time.Since(start))

	// every entry except the ones without a URL
	perRound := len(parityEntries) - 1
	if want := entries / len(parityEntries) * perRound; len(channels) != want {
		t.Fatalf("got %d channels, want %d", len(channels), want)
	}

	// the first channels are the same as when parsed from a small playlist
	small, err := MakeInterfaceFromM3U(buildPlaylist(len(parityEntries) * 2))
	if err != nil {
		t.Fatal(err)
	}
	for i := range small {
		if !reflect.DeepEqual(small[i], channels[i]) {
			t.Fatalf("channel %d differs between small and large playlist", i)
		}
	}
}

func TestParserUUIDKeyIsDeterministic(t *testing.T) {
	content := []byte("#EXTM3U\n" +
		`#EXTINF:-1 x-tvg-name="b" alt-tvg-name="a",Name` + "\nhttp://x/1\n" +
		`#EXTINF:-1 x-tvg-name="b" tvg-name="c",Name` + "\nhttp://x/2\n")
	for i := 0; i < 20; i++ {
		channels, err := MakeInterfaceFromM3U(content)
		if err != nil {
			t.Fatal(err)
		}
		if k := channels[0].(map[string]string)["_uuid.key"]; k != "alt-tvg-name" {
			t.Fatalf("uuid key %q, want alt-tvg-name", k)
		}
		if k := channels[1].(map[string]string)["_uuid.key"]; k != "tvg-name" {
			t.Fatalf("uuid key %q, want tvg-name", k)
		}
	}
}

func TestParserRejectsInvalidPlaylists(t *testing.T) {
	for _, content := range []string{
		"#EXTINF:-1,No header\nhttp://x/1\n",
		"#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\nseg1.ts\n",
		"#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:1\n",
	} {
		if _, err := MakeInterfaceFromM3U([]byte(content)); err == nil {
			t.Errorf("no error for %q", content)
		}
	}
	channels, err := MakeInterfaceFromM3U([]byte("#EXTM3U\n"))
	if err != nil || len(channels) != 0 {
		t.Errorf("empty playlist: %v, %v", channels, err)
	}
}

func BenchmarkParse200k(b *testing.B) {
	content := buildPlaylist(200000)
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := MakeInterfaceFromM3U(content); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParse200kLegacy(b *testing.B) {
	content := buildPlaylist(200000)
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := legacyMakeInterfaceFromM3U(content); err != nil {
			b.Fatal(err)
		}
	}
}
