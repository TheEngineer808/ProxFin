package src

import (
	"fmt"
	"math/rand"
	"runtime/debug"
	"time"
)

// InitMaintenance : Wartungsprozess initialisieren
func InitMaintenance() (err error) {

	System.TimeForAutoUpdate = fmt.Sprintf("0%d%d", randomTime(0, 2), randomTime(10, 59))

	go maintenance()

	return
}

func maintenance() {

	for {

		var t = time.Now()

		// Aktualisierung der Playlist und XMLTV Dateien
		systemMutex.Lock()
		if System.ScanInProgress == 0 {
			systemMutex.Unlock()
			for _, schedule := range Settings.Update {

				if schedule == t.Format("1504") {
					runScheduledUpdate(schedule)
				}

			}

			// Update Threadfin (Binary)
			systemMutex.Lock()
			if System.TimeForAutoUpdate == t.Format("1504") {
				systemMutex.Unlock()
				BinaryUpdate()
			} else {
				systemMutex.Unlock()
			}

		} else {
			systemMutex.Unlock()
		}

		time.Sleep(60 * time.Second)

	}

	return
}

func randomTime(min, max int) int {
	return rand.Intn(max-min) + min
}

// runScheduledUpdate refreshes the provider files and rebuilds the
// databases at a scheduled time. It holds refreshMutex for the whole run
// (so the XEPG build runs in this goroutine instead of in the background),
// and a panic is logged instead of ending the process, which would stop
// every stream.
func runScheduledUpdate(schedule string) {

	refreshMutex.Lock()
	defer refreshMutex.Unlock()

	defer func() {
		if r := recover(); r != nil {
			ShowError(fmt.Errorf("scheduled update %s failed: %v\n%s", schedule, r, debug.Stack()), 0)
			systemMutex.Lock()
			System.ScanInProgress = 0
			systemMutex.Unlock()
		}
	}()

	showInfo("Update:" + schedule)

	// Backup erstellen
	err := ThreadfinAutoBackup()
	if err != nil {
		ShowError(err, 000)
	}

	// Playlist und XMLTV Dateien aktualisieren
	getProviderData("m3u", "")
	getProviderData("hdhr", "")

	if Settings.EpgSource == "XEPG" {
		getProviderData("xmltv", "")
	}

	// Datenbank für DVR erstellen
	err = buildDatabaseDVR()
	if err != nil {
		ShowError(err, 000)
	}

	systemMutex.Lock()
	if !Settings.CacheImages && System.ImageCachingInProgress == 0 {
		systemMutex.Unlock()
		removeChildItems(System.Folder.ImagesCache)
	} else {
		systemMutex.Unlock()
	}

	// XEPG Dateien erstellen
	buildXEPG(false)
}
