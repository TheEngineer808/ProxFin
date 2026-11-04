package src

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"sync"
)

func makeInteraceFromHDHR(content []byte, playlistName, id string) (channels []interface{}, err error) {

	var hdhrData []interface{}

	err = json.Unmarshal(content, &hdhrData)
	if err == nil {

		for _, d := range hdhrData {

			// Skip malformed entries instead of panicking (this runs in the
			// maintenance goroutine, where a panic ends the process).
			data, ok := d.(map[string]interface{})
			if !ok {
				continue
			}
			guideName, ok1 := data["GuideName"].(string)
			url, ok2 := data["URL"].(string)
			guideNumber, ok3 := data["GuideNumber"].(string)
			if !ok1 || !ok2 || !ok3 {
				continue
			}

			var channel = make(map[string]string)

			channel["group-title"] = playlistName
			channel["name"] = guideName
			channel["tvg-id"] = guideName
			channel["url"] = url
			channel["ID-"+id] = guideNumber
			channel["_uuid.key"] = "ID-" + id
			channel["_values"] = playlistName + " " + channel["name"]

			channels = append(channels, channel)

		}

	}

	return
}

func getCapability() (xmlContent []byte, err error) {

	var capability Capability
	var buffer bytes.Buffer

	capability.Xmlns = "urn:schemas-upnp-org:device-1-0"
	capability.URLBase = System.ServerProtocol.WEB + "://" + System.Domain

	capability.SpecVersion.Major = 1
	capability.SpecVersion.Minor = 0

	capability.Device.DeviceType = "urn:schemas-upnp-org:device:MediaServer:1"
	capability.Device.FriendlyName = System.Name
	capability.Device.Manufacturer = "Silicondust"
	capability.Device.ModelName = "HDTC-2US"
	capability.Device.ModelNumber = "HDTC-2US"
	capability.Device.SerialNumber = ""
	capability.Device.UDN = "uuid:" + System.DeviceID

	output, err := xml.MarshalIndent(capability, " ", "  ")
	if err != nil {
		ShowError(err, 1003)
	}

	buffer.Write([]byte(xml.Header))
	buffer.Write([]byte(output))
	xmlContent = buffer.Bytes()

	return
}

func getDiscover() (jsonContent []byte, err error) {

	var discover Discover

	discover.BaseURL = System.ServerProtocol.WEB + "://" + System.Domain
	discover.DeviceAuth = System.AppName
	discover.DeviceID = System.DeviceID
	discover.FirmwareName = "bin_" + System.Version
	discover.FirmwareVersion = System.Version
	discover.FriendlyName = System.Name

	discover.LineupURL = fmt.Sprintf("%s://%s/lineup.json", System.ServerProtocol.DVR, System.Domain)
	discover.Manufacturer = "Golang"
	discover.ModelNumber = System.Version
	discover.TunerCount = Settings.Tuner

	jsonContent, err = json.MarshalIndent(discover, "", "  ")

	return
}

func getLineupStatus() (jsonContent []byte, err error) {

	var lineupStatus LineupStatus

	lineupStatus.ScanInProgress = System.ScanInProgress
	lineupStatus.ScanPossible = 0
	lineupStatus.Source = "Cable"
	lineupStatus.SourceList = []string{"Cable"}

	jsonContent, err = json.MarshalIndent(lineupStatus, "", "  ")

	return
}

// lineupMutex serialises building the lineup: Plex and the XEPG build can
// request it at the same time, and the PMS guide numbers below are shared.
var lineupMutex sync.Mutex

func getLineup() (jsonContent []byte, err error) {

	lineupMutex.Lock()
	defer lineupMutex.Unlock()

	var lineup Lineup

	switch Settings.EpgSource {

	case "PMS":
		for i, dsa := range Data.Streams.Active {

			var m3uChannel M3UChannelStructXEPG

			err = json.Unmarshal([]byte(mapToJSON(dsa)), &m3uChannel)
			if err != nil {
				return
			}

			var stream LineupStream
			stream.GuideName = m3uChannel.Name
			switch len(m3uChannel.UUIDValue) {

			case 0:
				stream.GuideNumber = fmt.Sprintf("%d", i+1000)
				guideNumber, err := getGuideNumberPMS(stream.GuideName)
				if err != nil {
					ShowError(err, 0)
				}

				stream.GuideNumber = guideNumber

			default:
				stream.GuideNumber = m3uChannel.UUIDValue

			}

			stream.URL, err = createStreamingURL("DVR", m3uChannel.FileM3UID, stream.GuideNumber, m3uChannel.Name, m3uChannel.URL, nil, nil, nil)
			if err == nil {
				lineup = append(lineup, stream)
			} else {
				ShowError(err, 1202)
			}

		}

	case "XEPG":
		for _, dxc := range Data.XEPG.Channels {

			var xepgChannel XEPGChannelStruct
			err = json.Unmarshal([]byte(mapToJSON(dxc)), &xepgChannel)
			if err != nil {
				return
			}

			if xepgChannel.XActive == true && !xepgChannel.XHideChannel {
				var stream LineupStream
				stream.GuideName = xepgChannel.XName
				stream.GuideNumber = xepgChannel.XChannelID
				//stream.URL = fmt.Sprintf("%s://%s/stream/%s-%s", System.ServerProtocol.DVR, System.Domain, xepgChannel.FileM3UID, base64.StdEncoding.EncodeToString([]byte(xepgChannel.URL)))
				stream.URL, err = createStreamingURL("DVR", xepgChannel.FileM3UID, xepgChannel.XChannelID, xepgChannel.XName, xepgChannel.URL, xepgChannel.BackupChannel1, xepgChannel.BackupChannel2, xepgChannel.BackupChannel3)
				if err == nil {
					lineup = append(lineup, stream)
				} else {
					ShowError(err, 1202)
				}

			}

		}

	}

	jsonContent, err = json.MarshalIndent(lineup, "", "  ")

	// New guide numbers are saved once per lineup, not once per channel.
	if pmsGuideNumbers.dirty {
		if errSave := saveMapToJSONFile(System.File.PMS, Data.Cache.PMS); errSave != nil {
			ShowError(errSave, 0)
		}
	}

	Data.Cache.PMS = nil
	pmsGuideNumbers.used = nil

	saveStreamingURLs()

	return
}

// pmsGuideNumbers tracks the guide numbers in Data.Cache.PMS while a
// lineup is built (lineupMutex held).
var pmsGuideNumbers struct {
	used  map[string]bool // ids in use
	next  int             // every id below id-<next> is in use
	dirty bool            // new ids to save
}

// getGuideNumberPMS returns the stable guide number of a channel without
// one (EPG source PMS), assigning the smallest free "id-N" to new channels.
// It used to rebuild the list of all ids for every candidate number and
// save the whole file for every new channel: O(n³) per lineup request
// (2000 new channels took 104 s). Within one lineup ids are only added, so
// the smallest free id never decreases and a cursor finds it in O(1).
func getGuideNumberPMS(channelName string) (pmsID string, err error) {

	if pmsGuideNumbers.used == nil {

		Data.Cache.PMS = make(map[string]string)
		pmsGuideNumbers.used = make(map[string]bool)
		pmsGuideNumbers.next = 0
		pmsGuideNumbers.dirty = false

		pms, err := loadJSONFileToMap(System.File.PMS)

		if err != nil {
			return "", err
		}

		for key, value := range pms {
			if id, ok := value.(string); ok {
				Data.Cache.PMS[key] = id
				pmsGuideNumbers.used[id] = true
			}
		}

	}

	if value, ok := Data.Cache.PMS[channelName]; ok {
		return value, nil
	}

	for {
		pmsID = fmt.Sprintf("id-%d", pmsGuideNumbers.next)
		pmsGuideNumbers.next++
		if !pmsGuideNumbers.used[pmsID] {
			break
		}
	}

	Data.Cache.PMS[channelName] = pmsID
	pmsGuideNumbers.used[pmsID] = true
	pmsGuideNumbers.dirty = true

	return
}
