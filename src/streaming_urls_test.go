package src

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func setupStreamingURLs(t *testing.T) {
	t.Helper()
	streamingURLsMu.Lock()
	Data.Cache.StreamingURLS, streamingURLsAge, streamingURLsGen = nil, nil, 0
	streamingURLsMu.Unlock()
	old := System.File.URLS
	System.File.URLS = filepath.Join(t.TempDir(), "urls.json")
	t.Cleanup(func() { System.File.URLS = old })
}

func streamID(t *testing.T, channel int) string {
	t.Helper()
	u, err := createStreamingURL("M3U", "Mtest", fmt.Sprint(channel), fmt.Sprint("chan", channel), fmt.Sprint("http://provider/", channel), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Base(u)
}

// A playlist/EPG update resets the cache and fills it again only at its
// end; channels must stay tunable in between.
func TestStreamingURLsStayTunableDuringRebuild(t *testing.T) {
	setupStreamingURLs(t)
	id1 := streamID(t, 1)

	// A rebuild: two resets, then the M3U is generated.
	resetStreamingURLs()
	saveStreamingURLs()
	resetStreamingURLs()
	if s, err := getStreamInfo(id1); err != nil || s.URL != "http://provider/1" {
		t.Fatalf("during the rebuild: %+v, %v", s, err)
	}
	id2 := streamID(t, 2) // channel 1 is gone from the playlist
	saveStreamingURLs()

	// The next rebuild, with a lineup fetch in the middle.
	resetStreamingURLs()
	id9 := streamID(t, 9)
	resetStreamingURLs()
	for _, id := range []string{id2, id9} {
		if _, err := getStreamInfo(id); err != nil {
			t.Fatalf("during the second rebuild: %v", err)
		}
	}
	if _, err := getStreamInfo(id1); err == nil {
		t.Fatalf("a channel removed two rebuilds ago is still served")
	}
}

// A changed channel (e.g. new backup channels) is picked up by the next
// rebuild.
func TestStreamingURLsRefreshedByRebuild(t *testing.T) {
	setupStreamingURLs(t)
	id := streamID(t, 1)
	backup := &BackupStream{PlaylistID: "Mtest", URL: "http://backup/1"}

	createStreamingURL("M3U", "Mtest", "1", "chan1", "http://provider/1", backup, nil, nil)
	if s, _ := getStreamInfo(id); s.BackupChannel1 != nil {
		t.Fatalf("entry changed within one generation")
	}

	resetStreamingURLs()
	createStreamingURL("M3U", "Mtest", "1", "chan1", "http://provider/1", backup, nil, nil)
	if s, _ := getStreamInfo(id); s.BackupChannel1 == nil || s.BackupChannel1.URL != backup.URL {
		t.Fatalf("rebuild did not pick up the new backup channel: %+v", s)
	}
}

func TestStreamingURLsLoadFromDiskAfterRestart(t *testing.T) {
	setupStreamingURLs(t)
	id := streamID(t, 3)
	saveStreamingURLs()

	streamingURLsMu.Lock()
	Data.Cache.StreamingURLS, streamingURLsAge = nil, nil // restart
	streamingURLsMu.Unlock()

	s, err := getStreamInfo(id)
	if err != nil || s.URL != "http://provider/3" {
		t.Fatalf("after restart: %+v, %v", s, err)
	}
}

// Tunes while an update rebuilds the cache and clients fetch the lineup
// (run with -race; the old code could crash here).
func TestStreamingURLsConcurrentAccess(t *testing.T) {
	setupStreamingURLs(t)
	ids := make([]string, 50)
	for i := range ids {
		ids[i] = streamID(t, i)
	}
	saveStreamingURLs()

	var wg sync.WaitGroup
	done := make(chan struct{})

	wg.Add(1)
	go func() { // rebuilds, one at a time as in Threadfin
		defer wg.Done()
		defer close(done)
		for r := 0; r < 30; r++ {
			resetStreamingURLs()
			saveStreamingURLs()
			for i := 0; i < len(ids)/2; i++ {
				streamID(t, i)
			}
			resetStreamingURLs()
			for i := range ids {
				streamID(t, i)
			}
			saveStreamingURLs()
		}
	}()

	for g := 0; g < 2; g++ { // lineup fetches
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				for i := range ids {
					streamID(t, i)
				}
				saveStreamingURLs()
			}
		}()
	}

	for g := 0; g < 4; g++ { // tunes
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for r := 0; ; r++ {
				select {
				case <-done:
					return
				default:
				}
				if _, err := getStreamInfo(ids[(r+g)%len(ids)]); err != nil {
					t.Errorf("tune during a rebuild failed: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
