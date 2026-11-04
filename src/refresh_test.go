package src

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// While a refresh runs (scheduled, or started from the web UI or the API),
// another entry point into the refresh pipeline must wait for it instead of
// writing the same maps at the same time.
func TestRefreshEntryPointsWaitForARunningRefresh(t *testing.T) {
	saved := System.File.XEPG
	System.File.XEPG = filepath.Join(t.TempDir(), "xepg.json")
	t.Cleanup(func() { System.File.XEPG = saved })
	os.WriteFile(System.File.XEPG, []byte("not json"), 0644) // fails fast once it runs

	refreshMutex.Lock() // a refresh is running

	done := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		enablePPV(rec, httptest.NewRequest(http.MethodGet, "/ppv/enable", nil))
		done <- rec.Code
	}()

	select {
	case <-done:
		refreshMutex.Unlock()
		t.Fatal("/ppv/enable ran while a refresh was in progress")
	case <-time.After(300 * time.Millisecond):
	}

	refreshMutex.Unlock() // the refresh ends

	select {
	case code := <-done:
		if code != http.StatusInternalServerError {
			t.Errorf("expected the request to run and fail on the bad file, got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("/ppv/enable did not run after the refresh finished")
	}

	for _, cmd := range []string{"saveSettings", "saveFilesM3U", "updateFileM3U", "saveFilter", "saveEpgMapping", "ThreadfinRestore"} {
		if !wsRefreshCommands[cmd] {
			t.Errorf("web UI command %s must wait for a running refresh", cmd)
		}
	}
	for _, cmd := range []string{"getServerConfig", "updateLog"} {
		if wsRefreshCommands[cmd] {
			t.Errorf("read-only web UI command %s must not wait for a refresh", cmd)
		}
	}
}
