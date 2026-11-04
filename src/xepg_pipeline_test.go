package src

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"threadfin/src/internal/imgcache"
)

// usePipelineFolders points the files and caches the playlist/EPG pipeline
// uses at a temporary folder and restores them when the test ends.
func usePipelineFolders(t *testing.T) string {
	t.Helper()

	savedSystemFolder, savedSystemFile, savedGZ, savedInfo := System.Folder, System.File, System.Compressed.GZxml, System.Flag.Info
	savedImages, savedXMLTVCache := Data.Cache.Images, Data.Cache.XMLTV
	savedXEPG, savedXMLTV, savedStreams, savedFilter := Data.XEPG, Data.XMLTV, Data.Streams, Data.Filter
	t.Cleanup(func() {
		System.Folder, System.File, System.Compressed.GZxml, System.Flag.Info = savedSystemFolder, savedSystemFile, savedGZ, savedInfo
		Data.Cache.Images, Data.Cache.XMLTV = savedImages, savedXMLTVCache
		Data.XEPG, Data.XMLTV, Data.Streams, Data.Filter = savedXEPG, savedXMLTV, savedStreams, savedFilter
	})

	dir := t.TempDir()
	System.Flag.Info = true
	System.Folder.Data = dir + string(os.PathSeparator)
	System.Folder.ImagesCache = filepath.Join(dir, "images") + string(os.PathSeparator)
	if err := os.MkdirAll(System.Folder.ImagesCache, 0755); err != nil {
		t.Fatal(err)
	}
	System.File.XML = filepath.Join(dir, "threadfin.xml")
	System.Compressed.GZxml = filepath.Join(dir, "threadfin.xml.gz")
	System.File.XEPG = filepath.Join(dir, "xepg.json")
	System.File.URLS = filepath.Join(dir, "urls.json")

	images, err := imgcache.New(System.Folder.ImagesCache, "http://localhost/images/", false)
	if err != nil {
		t.Fatal(err)
	}
	Data.Cache.Images = images
	Data.Cache.XMLTV = make(map[string]XMLTV)

	return dir
}

// The .gz file was compressed before the last buffered part of the XML was
// written, so it lacked up to the last 1 MB, including </tv>.
func TestXMLTVGzipIsComplete(t *testing.T) {
	usePipelineFolders(t)

	Data.XMLTV.Files = []string{}
	Data.Streams.Active = []interface{}{map[string]string{"name": "x"}}
	Data.XEPG.Channels = make(map[string]interface{})
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("x-ID.%d", i)
		Data.XEPG.Channels[id] = XEPGChannelStruct{
			XEPG: id, XActive: true, XChannelID: fmt.Sprint(1000 + i), TvgChno: fmt.Sprint(1000 + i),
			XName: fmt.Sprintf("Channel %d", i), XmltvFile: "Threadfin Dummy", XMapping: "30_Minutes",
		}
	}

	if err := createXMLTVFile(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(System.File.XML)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 1<<20 {
		t.Fatalf("XML is only %d bytes; the test needs more than the 1 MB buffer", len(raw))
	}
	if !strings.HasSuffix(string(raw), "</tv>\n") {
		t.Fatal("XML file is not complete")
	}

	f, err := os.Open(System.Compressed.GZxml)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	unpacked, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unpacked, raw) {
		t.Fatalf("gzip holds %d bytes, XML file %d", len(unpacked), len(raw))
	}
}
