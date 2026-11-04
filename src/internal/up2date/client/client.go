package up2date

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"time"
)

// ClientInfo : Information about the key (NAME OS, ARCH, UUID, KEY)
type ClientInfo struct {
	Arch   string `json:"arch,required"`
	Branch string `json:"branch,required"`
	CMD    string `json:"cmd,omitempty"`
	Name   string `json:"name,required"`
	OS     string `json:"os,required"`
	URL    string `json:"url,required"`

	Response ServerResponse `json:"response,omitempty"`
}

// ServerResponse : Response from server after client request
type ServerResponse struct {
	Status    bool   `json:"status,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Version   string `json:"version,omitempty"`
	UpdateBIN string `json:"update.url.bin,omitempty"`
	UpdateZIP string `json:"update.url.zip,omitempty"`
	Filename  string `json:"filename.bin,omitempty"`
}

// Updater : Client infos
var Updater ClientInfo

// UpdateURL : URL for the new binary
var UpdateURL string

// requestTimeout bounds every request to the update server, so a stalled
// server can't block the caller (startup, or the maintenance scheduler).
const requestTimeout = 30 * time.Second

// Init : Init
func Init() {
	Updater.OS = runtime.GOOS
	Updater.Arch = runtime.GOARCH
	removeOldBinary()
}

// GetVersion : Information about the latest version
func GetVersion() (err error) {

	Updater.CMD = "getVersion"
	err = serverRequest()
	return
}

func serverRequest() (err error) {

	var serverResponse ServerResponse
	jsonByte, err := json.MarshalIndent(Updater, "", "  ")
	if err == nil {

		// Serververbindung prüfen
		u, err := url.Parse(Updater.URL)
		if err != nil {
			return err
		}
		var server = u.Host

		timeout := time.Duration(1 * time.Second)
		conn, err := net.DialTimeout("tcp", server, timeout)
		if err != nil {
			return err
		}
		conn.Close()

		// Check redirect 301 <---> 308
		redirect, err := http.NewRequest("POST", Updater.URL, nil)
		if err != nil {
			return err
		}

		client := &http.Client{
			Timeout: requestTimeout,
			CheckRedirect: func(redirect *http.Request, via []*http.Request) error {
				return errors.New("Redirect")
			},
		}

		resp, err := client.Do(redirect)
		if resp != nil {
			resp.Body.Close()
		}

		if err != nil {
			// Redirect (status code 301 <---> 308). Any other error has no
			// response at all.
			if resp == nil || resp.StatusCode < 301 || resp.StatusCode > 308 {
				return err
			}
			Updater.URL = resp.Header.Get("Location")
		}
		// ---

		req, err := http.NewRequest("POST", Updater.URL, bytes.NewBuffer(jsonByte))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err = (&http.Client{Timeout: requestTimeout}).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("%d: %s (%s)", resp.StatusCode, http.StatusText(resp.StatusCode), Updater.URL)
			return err
		}

		Updater.CMD = ""

		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return err
		}

		err = json.Unmarshal(body, &serverResponse)

		if err != nil {
			return err
		}

		Updater.Response = serverResponse

	}

	return
}
