package src

import (
	"os"
	"strings"
)

// ProxFin is based on Threadfin. These names are kept from it so that an
// existing Threadfin install keeps working after switching: they are stored
// in its settings and databases, or used by the DVR clients (Plex, Emby,
// Jellyfin) that were set up with it.
const (
	// legacyAppName is Threadfin's name in folders, files and URLs.
	legacyAppName = "threadfin"

	// providerFileKey is the key of each playlist and guide entry in
	// settings.json that names its local copy.
	providerFileKey = "file.threadfin"

	// dummyXMLTVFile is the built-in dummy guide. Channels that use it store
	// its name in xepg.json; legacyDummyXMLTVFile is the name Threadfin
	// stored, converted when the database is loaded.
	dummyXMLTVFile       = "ProxFin Dummy"
	legacyDummyXMLTVFile = "Threadfin Dummy"
)

// isDummyXMLTVFile reports whether name is the dummy guide.
func isDummyXMLTVFile(name string) bool {
	return name == dummyXMLTVFile || name == legacyDummyXMLTVFile
}

// migrateLegacyDummy renames the dummy guide of channels mapped to it by
// Threadfin. It reports whether any channel changed.
func migrateLegacyDummy(channels map[string]interface{}) (changed bool) {
	for _, c := range channels {
		if channel, ok := c.(map[string]interface{}); ok && channel["x-xmltv-file"] == legacyDummyXMLTVFile {
			channel["x-xmltv-file"] = dummyXMLTVFile
			changed = true
		}
	}
	return
}

// defaultConfigFolder returns the configuration folder used when none is
// given: ~/.proxfin, or Threadfin's ~/.threadfin if it exists and there is
// no ProxFin folder yet, so switching from Threadfin keeps every setting,
// playlist and mapping.
func defaultConfigFolder() (folder string, legacy bool) {
	return defaultConfigFolderIn(GetUserHomeDirectory())
}

func defaultConfigFolderIn(home string) (folder string, legacy bool) {

	home = strings.TrimRight(home, string(os.PathSeparator)) + string(os.PathSeparator)
	folder = home + "." + System.AppName + string(os.PathSeparator)
	var threadfin = home + "." + legacyAppName + string(os.PathSeparator)

	if !isDir(folder) && isDir(threadfin) {
		return threadfin, true
	}

	return folder, false
}

// dataFileCandidates returns the files to serve for a request of name in
// the data folder: the file under its current name first, then under
// Threadfin's name (threadfin.m3u, threadfin.xml, threadfin.xml.gz). DVR
// clients set up with Threadfin's URLs keep working, and the files are
// found before ProxFin has written its own.
func dataFileCandidates(name string) []string {

	var current, legacy = name, name

	switch {
	case strings.HasPrefix(name, legacyAppName+"."):
		current = System.AppName + strings.TrimPrefix(name, legacyAppName)
	case strings.HasPrefix(name, System.AppName+"."):
		legacy = legacyAppName + strings.TrimPrefix(name, System.AppName)
	}

	if current == legacy {
		return []string{System.Folder.Data + current}
	}

	return []string{System.Folder.Data + current, System.Folder.Data + legacy}
}

// existingDataFile returns the first of dataFileCandidates(name) that
// exists, or the first one.
func existingDataFile(name string) string {

	var candidates = dataFileCandidates(name)

	for _, file := range candidates {
		if info, err := os.Stat(file); err == nil && !info.IsDir() {
			return file
		}
	}

	return candidates[0]
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
