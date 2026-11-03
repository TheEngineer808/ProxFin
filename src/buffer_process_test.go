package src

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"threadfin/src/internal/streambuf"
)

func commandFor(t *testing.T, url string) []string {
	t.Helper()
	_, args, err := bufferCommand("ffmpeg", streambuf.Source{PlaylistID: testPlaylistID, URL: url})
	if err != nil {
		t.Fatal(err)
	}
	return args
}

func indexOf(args []string, a string) int {
	for i, x := range args {
		if x == a {
			return i
		}
	}
	return -1
}

func TestFFmpegReconnectOnlyForHTTPSources(t *testing.T) {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	oldPath, oldOptions, oldFiles := Settings.FFmpegPath, Settings.FFmpegOptions, Settings.Files.M3U
	t.Cleanup(func() {
		Settings.FFmpegPath, Settings.FFmpegOptions, Settings.Files.M3U = oldPath, oldOptions, oldFiles
	})
	Settings.FFmpegPath = path
	Settings.FFmpegOptions = defaultFFmpegOptions
	Settings.Files.M3U = map[string]interface{}{testPlaylistID: map[string]interface{}{"buffer": "ffmpeg"}}

	for _, url := range []string{"http://provider/live.ts", "HTTPS://provider/live.m3u8"} {
		args := commandFor(t, url)
		r, i := indexOf(args, "-reconnect"), indexOf(args, "-i")
		if r < 0 || r > i {
			t.Fatalf("%s: no -reconnect before -i: %v", url, args)
		}
	}
	for _, url := range []string{"/media/channel.ts", "rtsp://camera/live", "udp://@239.0.0.1:1234", "srt://host:9000"} {
		if args := commandFor(t, url); indexOf(args, "-reconnect") >= 0 {
			t.Fatalf("%s: reconnect options would make ffmpeg fail: %v", url, args)
		}
	}

	Settings.FFmpegOptions = "-reconnect 1 -i [URL] -c copy -f mpegts pipe:1"
	if args := commandFor(t, "http://provider/live.ts"); strings.Count(strings.Join(args, " "), "-reconnect ") != 1 {
		t.Fatalf("user's own -reconnect duplicated: %v", args)
	}
}

// Older ffmpeg builds lack some options; only the supported ones are used.
func TestFFmpegReconnectArgsFollowTheInstalledFFmpeg(t *testing.T) {
	dir := t.TempDir()
	write := func(name, help string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("#!/bin/sh\ncat <<'EOF'\n"+help+"\nEOF\n"), 0755)
		return p
	}
	old := write("ffmpeg41", "http AVOptions:\n  -reconnect         <boolean>\n  -reconnect_streamed <boolean>\n  -reconnect_delay_max <int>")
	none := write("noreconnect", "Unknown protocol 'http'.")

	if got := strings.Join(ffmpegReconnectArgs(old), " "); got != "-reconnect 1 -reconnect_streamed 1 -reconnect_delay_max 2" {
		t.Fatalf("ffmpeg without reconnect_on_network_error: %q", got)
	}
	if got := ffmpegReconnectArgs(none); len(got) != 0 {
		t.Fatalf("ffmpeg without http reconnect: %q", got)
	}
}

func TestOldDefaultFFmpegOptionsAreMigrated(t *testing.T) {
	s := SettingsStruct{FFmpegOptions: previousFFmpegDefaultOptions}
	System.FFmpeg.DefaultOptions = defaultFFmpegOptions // set at start-up
	migrateFFmpegOptions(&s)
	if s.FFmpegOptions != defaultFFmpegOptions || !strings.Contains(s.FFmpegOptions, "-map 0:v? -map 0:a:0?") {
		t.Fatalf("old default not migrated: %q", s.FFmpegOptions)
	}

	custom := "-hide_banner -i [URL] -c copy -f mpegts pipe:1"
	s = SettingsStruct{FFmpegOptions: custom}
	migrateFFmpegOptions(&s)
	if s.FFmpegOptions != custom {
		t.Fatalf("custom options changed: %q", s.FFmpegOptions)
	}
}
