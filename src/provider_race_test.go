package src

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// A refresh updates the provider entries in place (download counters, last
// update, compatibility) while streams read them when tuning. Without a lock
// this is a fatal "concurrent map read and map write" (run with -race).
func TestProviderSettingsReadDuringRefresh(t *testing.T) {
	dir := t.TempDir() + string(os.PathSeparator)
	playlist := filepath.Join(dir, "source.m3u")
	os.WriteFile(playlist, []byte("#EXTM3U\n#EXTINF:-1 tvg-id=\"a\" group-title=\"G\",A\nhttp://example/a\n"), 0644)

	savedFiles, savedData, savedSettingsFile, savedInfo := Settings.Files, System.Folder.Data, System.File.Settings, System.Flag.Info
	t.Cleanup(func() {
		Settings.Files, System.Folder.Data, System.File.Settings, System.Flag.Info = savedFiles, savedData, savedSettingsFile, savedInfo
	})
	System.Folder.Data = dir
	System.File.Settings = filepath.Join(dir, "settings.json")
	System.Flag.Info = true

	Settings.Files.M3U = map[string]interface{}{
		"Mrace": map[string]interface{}{
			"name": "race", "file.source": playlist, "tuner": 2.0, "type": "m3u",
			"counter.download": 0.0, "counter.error": 0.0,
		},
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_ = getProviderParameter("Mrace", "m3u", "name")
				_ = getProviderParameter("Mrace", "m3u", "tuner")
			}
		}()
	}

	for i := 0; i < 20; i++ {
		if err := getProviderData("m3u", ""); err != nil {
			t.Fatal(err)
		}
		setProviderCompatibility("Mrace", "m3u", map[string]int{"tvg.id": 100})
	}
	stop.Store(true)
	wg.Wait()

	if got := getProviderParameter("Mrace", "m3u", "name"); got != "race" {
		t.Errorf("name: %q", got)
	}
}
