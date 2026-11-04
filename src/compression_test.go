package src

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// Backups are uploaded through the web interface; an entry must not be able
// to write outside the restore folder.
func TestExtractZIPRejectsPathsOutsideTheTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "restore")

	for _, name := range []string{"../escaped.txt", "data/../../escaped.txt"} {
		archive := filepath.Join(dir, "evil.zip")
		writeZip(t, archive, map[string]string{name: "x"})
		if err := extractZIP(archive, target); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "escaped.txt")); err == nil {
			t.Fatalf("%s: a file was written outside the target folder", name)
		}
	}
}

func TestBackupRoundTrip(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	os.WriteFile(settings, []byte(`{"a":1}`), 0644)

	archive := filepath.Join(dir, "backup.zip")
	// A missing optional file used to end the backup early, silently.
	if err := zipFiles([]string{filepath.Join(dir, "pms.json"), settings}, archive); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "restore")
	if err := extractZIP(archive, target); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(target, "settings.json")); err != nil || string(b) != `{"a":1}` {
		t.Errorf("restored settings.json: %q, %v", b, err)
	}

	// Entries in sub-folders without folder entries of their own.
	nested := filepath.Join(dir, "nested.zip")
	writeZip(t, nested, map[string]string{"data/img/logo.png": "png"})
	if err := extractZIP(nested, target); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(target, "data", "img", "logo.png")); err != nil || string(b) != "png" {
		t.Errorf("nested file: %q, %v", b, err)
	}
}
