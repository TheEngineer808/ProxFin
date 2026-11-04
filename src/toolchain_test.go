package src

import (
	"bytes"
	"compress/gzip"
	"io"
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

// The playlist and guide are rewritten while clients fetch them from disk:
// a reader must always get a complete file, and a failed or abandoned
// rewrite must leave the old file and no temporary file behind.
func TestAtomicFileReplacement(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "threadfin.m3u")

	write := func(content []byte) {
		f, err := createAtomic(file)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Discard()
		for i := 0; i < len(content); i += 4096 { // in pieces, like the builders
			end := min(i+4096, len(content))
			if _, err := f.Write(content[i:end]); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	versions := [][]byte{
		append([]byte("#EXTM3U\n"), bytes.Repeat([]byte("a"), 1<<20)...),
		append([]byte("#EXTM3U\n"), bytes.Repeat([]byte("b"), 2<<20)...),
	}
	write(versions[0])

	var stop atomic.Bool
	var partial atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			b, err := os.ReadFile(file)
			if err != nil || !(bytes.Equal(b, versions[0]) || bytes.Equal(b, versions[1])) {
				partial.Add(1)
			}
		}
	}()
	for i := 0; i < 20; i++ {
		write(versions[i%2])
	}
	stop.Store(true)
	wg.Wait()
	if n := partial.Load(); n > 0 {
		t.Errorf("a reader saw a partial file %d times", n)
	}

	// An abandoned rewrite keeps the old file and leaves nothing behind.
	f, err := createAtomic(file)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("half"))
	f.Discard()
	if b, _ := os.ReadFile(file); !bytes.Equal(b, versions[1]) {
		t.Error("an abandoned rewrite changed the file")
	}

	// The compressed copy is complete (gzip footer included).
	if err := compressGZIPFile(file, file+".gz"); err != nil {
		t.Fatal(err)
	}
	gz, _ := os.Open(file + ".gz")
	zr, err := gzip.NewReader(gz)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	gz.Close()
	if err != nil || !bytes.Equal(got, versions[1]) {
		t.Errorf("gz content: %d bytes, %v", len(got), err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected only the file and its .gz, found %v", names)
	}
}
