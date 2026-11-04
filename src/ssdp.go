package src

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/koron/go-ssdp"
)

var (
	ssdpMutex sync.Mutex
	ssdpStop  chan struct{} // closed to end the running advertiser
	ssdpDone  chan struct{} // closed once it has said goodbye
)

// SSDP : SSPD / DLNA Server
func SSDP() (err error) {

	if Settings.SSDP == false || System.Flag.Info == true {
		return
	}

	// Init runs again after a restore: replace the running advertiser
	// instead of starting a second one that keeps announcing the old
	// device.
	stopSSDP()

	showInfo(fmt.Sprintf("SSDP / DLNA:%t", Settings.SSDP))

	ad, err := ssdp.Advertise(
		fmt.Sprintf("upnp:rootdevice"),                           // send as "ST"
		fmt.Sprintf("uuid:%s::upnp:rootdevice", System.DeviceID), // send as "USN"
		fmt.Sprintf("%s/device.xml", System.URLBase),             // send as "LOCATION"
		System.AppName, // send as "SERVER"
		1800)           // send as "maxAge" in "CACHE-CONTROL"

	if err != nil {
		return
	}

	// Debug SSDP
	if System.Flag.Debug == 3 {
		ssdp.Logger = log.New(os.Stderr, "[SSDP] ", log.LstdFlags)
	}

	stop, done := make(chan struct{}), make(chan struct{})
	ssdpMutex.Lock()
	ssdpStop, ssdpDone = stop, done
	ssdpMutex.Unlock()

	go func(adv *ssdp.Advertiser) {

		defer close(done)

		aliveTick := time.NewTicker(300 * time.Second)
		defer aliveTick.Stop()

		for {

			select {

			case <-aliveTick.C:
				if err := adv.Alive(); err != nil {
					ShowError(err, 0)
					adv.Bye()
					adv.Close()
					return
				}

			case <-stop:
				adv.Bye()
				adv.Close()
				return

			}

		}

	}(ad)

	return
}

// stopSSDP ends the SSDP announcement (saying goodbye, so DVR clients drop
// the tuner at once) and waits for it. Shutdown is handled by main, which
// stops the streams first.
func stopSSDP() {

	ssdpMutex.Lock()
	stop, done := ssdpStop, ssdpDone
	ssdpStop, ssdpDone = nil, nil
	ssdpMutex.Unlock()

	if stop == nil {
		return
	}

	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

// Shutdown stops what must not outlive ProxFin: the stream processes
// (FFmpeg/VLC run in their own process groups, so they would keep running
// and holding the providers' connections) and the SSDP announcement.
func Shutdown() {

	showInfo(System.Name + ":Shutting down")

	if err := streamManager.Shutdown(5 * time.Second); err != nil {
		ShowError(err, 0)
	}

	stopSSDP()
}
