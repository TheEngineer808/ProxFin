package src

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func zipFiles(sourceFiles []string, target string) (err error) {

	zipfile, err := os.Create(target)
	if err != nil {
		return err
	}

	archive := zip.NewWriter(zipfile)

	// A backup that failed half-way (e.g. the disk is full) must be
	// reported as failed and not left behind looking like a good one.
	defer func() {
		if closeErr := archive.Close(); err == nil {
			err = closeErr
		}
		if closeErr := zipfile.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(target)
		}
	}()

	for _, source := range sourceFiles {

		info, err := os.Stat(source)
		if os.IsNotExist(err) {
			// Optional files (e.g. pms.json) may not exist: back up the rest.
			continue
		}
		if err != nil {
			return err
		}

		var baseDir string
		if info.IsDir() {
			baseDir = filepath.Base(System.Folder.Data)
		}

		err = filepath.Walk(source, func(path string, info os.FileInfo, err error) error {

			if err != nil {
				return err
			}

			header, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}

			if baseDir != "" {
				header.Name = filepath.Join(strings.TrimPrefix(path, System.Folder.Config))
			}

			if info.IsDir() {
				header.Name += string(os.PathSeparator)
			} else {
				header.Method = zip.Deflate
			}

			writer, err := archive.CreateHeader(header)
			if err != nil {
				return err
			}

			if info.IsDir() {
				return nil
			}

			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()

			_, err = io.Copy(writer, file)

			return err

		})

		if err != nil {
			return err
		}

	}

	return nil
}

func extractZIP(archive, target string) (err error) {

	reader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer reader.Close()

	if err := os.MkdirAll(target, 0755); err != nil {
		return err
	}

	for _, file := range reader.File {
		if err := extractZIPFile(file, target); err != nil {
			return err
		}
	}

	return
}

// extractZIPFile extracts one archive entry below target. Entries that
// would end up outside it ("../", absolute paths) are rejected: backups
// are uploaded through the web interface.
func extractZIPFile(file *zip.File, target string) error {

	path := filepath.Join(target, file.Name)

	rel, err := filepath.Rel(target, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(file.Name) {
		return fmt.Errorf("invalid file path in archive: %s", file.Name)
	}

	if file.FileInfo().IsDir() {
		return os.MkdirAll(path, 0755)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	fileReader, err := file.Open()
	if err != nil {
		return err
	}
	defer fileReader.Close()

	targetFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode().Perm())
	if err != nil {
		return err
	}

	if _, err := io.Copy(targetFile, fileReader); err != nil {
		targetFile.Close()
		return err
	}

	return targetFile.Close()
}

func extractGZIP(gzipBody []byte, fileSource string) (body []byte, err error) {

	var b = bytes.NewBuffer(gzipBody)

	var r io.Reader
	r, err = gzip.NewReader(b)
	if err != nil {
		// Keine gzip Datei
		body = gzipBody
		err = nil
		return
	}

	showInfo("Extract gzip:" + fileSource)

	var resB bytes.Buffer
	_, err = resB.ReadFrom(r)
	if err != nil {
		body = gzipBody
		err = nil
		return
	}

	body = resB.Bytes()
	return
}

func compressGZIP(data *[]byte, file string) (err error) {

	if len(file) != 0 {

		f, err := os.Create(file)
		if err != nil {
			return err
		}

		w := gzip.NewWriter(f)
		w.Write(*data)
		w.Close()
	}

	return
}

func compressGZIPFile(sourcePath, targetPath string) (err error) {
	in, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer in.Close()

	// Renamed into place when complete, like the XML file itself.
	out, err := createAtomic(targetPath)
	if err != nil {
		return err
	}
	defer out.Discard()

	gw := gzip.NewWriter(out)
	if _, err = io.Copy(gw, in); err != nil {
		return err
	}
	// Close writes the gzip footer; an error here means a broken file.
	if err = gw.Close(); err != nil {
		return err
	}

	return out.Commit()
}
