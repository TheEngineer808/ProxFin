package src

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// The file name of an uploaded logo comes from the web interface; it must
// not be able to write outside the upload folder.
func TestUploadLogoStaysInTheUploadFolder(t *testing.T) {
	dir := t.TempDir()
	uploads := filepath.Join(dir, "uploads") + string(os.PathSeparator)
	os.MkdirAll(uploads, 0755)

	saved := System.Folder.ImagesUpload
	System.Folder.ImagesUpload = uploads
	t.Cleanup(func() { System.Folder.ImagesUpload = saved })

	data := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("png"))

	for _, name := range []string{"../escaped.png", "..\\escaped.png", "a/../../escaped.png", "/abs/escaped.png"} {
		if _, err := uploadLogo(data, name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "escaped.png")); err == nil {
			t.Fatalf("%s: written outside the upload folder", name)
		}
		if _, err := os.Stat(filepath.Join(uploads, "escaped.png")); err != nil {
			t.Errorf("%s: expected uploads/escaped.png: %v", name, err)
		}
		os.Remove(filepath.Join(uploads, "escaped.png"))
	}

	for _, name := range []string{"..", "", "/"} {
		if _, err := uploadLogo(data, name); err == nil {
			t.Errorf("%q: expected an error", name)
		}
	}
}
