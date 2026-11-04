package src

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func setupM3UChannels(t testing.TB, n int) {
	dir := usePipelineFolders(t)
	useSettings(t, dir)
	savedProtocol, savedDomain, savedGroups, savedName := System.ServerProtocol, System.Domain, Data.Playlist.M3U.Groups.Text, System.AppName
	t.Cleanup(func() {
		System.ServerProtocol, System.Domain, Data.Playlist.M3U.Groups.Text, System.AppName = savedProtocol, savedDomain, savedGroups, savedName
	})
	// The legacy copy names the guide threadfin.xml in the playlist header;
	// these tests compare the algorithm, so use the same name.
	System.AppName = legacyAppName
	System.ServerProtocol.M3U, System.ServerProtocol.XML, System.Domain = "http", "http", "threadfin.local:34400"

	Data.XEPG.Channels = map[string]interface{}{}
	for i := 0; i < n; i++ {
		group := fmt.Sprintf("Group %d", i%5)
		ch := XEPGChannelStruct{
			XEPG: fmt.Sprintf("x-ID.%d", i), XActive: i%9 != 0, XHideChannel: i%13 == 0,
			XChannelID: fmt.Sprint(1000 + i), TvgChno: fmt.Sprint(1000 + i), XName: fmt.Sprintf("Channel %d", i),
			XGroupTitle: group, GroupTitle: group, FileM3UID: "M1", TvgLogo: map[bool]string{true: "http://logo/x.png"}[i%2 == 0],
			URL: fmt.Sprintf("http://provider/%d.ts", i%(n-3)), // a few duplicate URLs
		}
		if i%11 == 0 {
			ch.XCategory = "Sports"
		}
		Data.XEPG.Channels[ch.XEPG] = ch
	}
	// expected channel counts per group; Group 4 is capped
	Data.Playlist.M3U.Groups.Text = []string{"Group 0 (999)", "Group 4 (10)", "Sports (999)"}
}

// buildM3U must return what it returned before for groups, and for no
// groups the full playlist it saved (it used to return "").
func TestBuildM3UMatchesLegacy(t *testing.T) {
	setupM3UChannels(t, 200)

	for _, groups := range [][]string{{"Group 1"}, {"Group 0", "Group 4"}, {"Sports"}, {"missing"}} {
		want, err := legacyBuildM3U(groups)
		if err != nil {
			t.Fatal(err)
		}
		got, err := buildM3U(groups)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("groups %v differ:\n got  %q\n want %q", groups, got, want)
		}
		if len(groups) == 1 && groups[0] == "Group 1" && strings.Count(got, "#EXTINF") < 20 {
			t.Fatalf("too few channels: %q", got)
		}
	}

	file := System.Folder.Data + "threadfin.m3u"
	if s, err := legacyBuildM3U(nil); err != nil || s != "" {
		t.Fatalf("legacy without groups: %q, %v", s, err)
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(file)

	got, err := buildM3U(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatal("playlist without groups differs from the file the old code saved")
	}
	saved, err := os.ReadFile(file)
	if err != nil || string(saved) != got {
		t.Fatalf("saved playlist differs from the returned one (%v)", err)
	}

	os.Remove(file)
	if err := saveM3UFile(nil); err != nil {
		t.Fatal(err)
	}
	if saved, _ := os.ReadFile(file); string(saved) != string(want) {
		t.Fatal("saveM3UFile differs from the file the old code saved")
	}
}

func TestBuildM3UGroupsSpeed(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	setupM3UChannels(t, 8000)
	groups := []string{"Group 0", "Group 1", "Group 2", "Group 3", "Group 4"}

	start := time.Now()
	want, _ := legacyBuildM3U(groups)
	legacy := time.Since(start)

	start = time.Now()
	got, _ := buildM3U(groups)
	current := time.Since(start)

	if got != want {
		t.Fatal("output differs")
	}
	t.Logf("8,000 channels in groups: before %v, now %v", legacy, current)
}
