package src

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
)

// A reader must never see a half-written file while it is being replaced,
// and replacing it must not leave temporary files or open descriptors behind.
func TestSaveMapToJSONFileIsAtomic(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "settings.json")

	small := map[string]interface{}{"a": "b"}
	large := map[string]interface{}{"payload": string(bytes.Repeat([]byte("x"), 1<<20))}
	if err := saveMapToJSONFile(file, small); err != nil {
		t.Fatal(err)
	}

	// With the GC off, a leaked descriptor can't be closed by a finalizer.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	fdsBefore := openFDs(t)

	var stop atomic.Bool
	var torn atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			if m, err := loadJSONFileToMap(file); err != nil || (m["a"] == nil && m["payload"] == nil) {
				torn.Add(1)
			}
		}
	}()

	for i := 0; i < 20; i++ {
		data := small
		if i%2 == 0 {
			data = large
		}
		if err := saveMapToJSONFile(file, data); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	wg.Wait()

	if n := torn.Load(); n > 0 {
		t.Errorf("a reader saw a partial or missing file %d times", n)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected only settings.json in the folder, found %v", names)
	}

	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0644 {
		t.Errorf("unexpected file mode %v (%v)", info.Mode(), err)
	}

	// Each save used to leak the descriptor of a stray os.Create.
	if after := openFDs(t); after > fdsBefore+2 {
		t.Errorf("open descriptors grew from %d to %d", fdsBefore, after)
	}
}

func TestLoadJSONFileToMapReportsMissingFile(t *testing.T) {
	_, err := loadJSONFileToMap(filepath.Join(t.TempDir(), "missing.json"))
	if !os.IsNotExist(err) {
		t.Errorf("expected a not-exist error, got %v", err)
	}
}

func openFDs(t *testing.T) int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("needs /proc")
	}
	return len(entries)
}
