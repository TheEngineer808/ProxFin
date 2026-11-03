package src

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"threadfin/src/internal/streambuf"
)

// Log lines from a process shown in the normal log before debug level only.
const processInfoLines = 20

// Last log lines kept to explain why a process failed.
const processTailLines = 6

// bufferLauncher starts ffmpeg or VLC for a source.
func bufferLauncher(program string) streambuf.Launcher {
	return func(src streambuf.Source) (streambuf.Process, error) {
		path, args, err := bufferCommand(program, src)
		if err != nil {
			return nil, err
		}
		showInfo(fmt.Sprintf("%s path:%s", strings.ToUpper(program), path))
		showInfo("Streaming URL:" + src.URL)
		showDebug(fmt.Sprintf("BUFFER DEBUG: %s:%s %s", strings.ToUpper(program), path, args), 1)
		return startBufferProcess(strings.ToUpper(program), path, args)
	}
}

// bufferCommand builds the command line from the settings, with the user
// agent, proxy and headers of the source's playlist.
func bufferCommand(program string, src streambuf.Source) (path string, args []string, err error) {
	url := src.URL
	t := playlistType(src.PlaylistID)
	proxyIP := getProviderParameter(src.PlaylistID, t, "http_proxy.ip")
	proxyPort := getProviderParameter(src.PlaylistID, t, "http_proxy.port")
	referer := getProviderParameter(src.PlaylistID, t, "http_headers.referer")
	origin := getProviderParameter(src.PlaylistID, t, "http_headers.origin")

	var options string
	switch program {
	case "ffmpeg":
		if Settings.FFmpegForceHttp {
			url = strings.Replace(url, "https://", "http://", -1)
			showInfo("Forcing URL to HTTP for FFMPEG: " + url)
		}
		path, options = Settings.FFmpegPath, Settings.FFmpegOptions
	case "vlc":
		path, options = Settings.VLCPath, Settings.VLCOptions
	default:
		return "", nil, fmt.Errorf("unknown buffer program %q", program)
	}

	if err = checkFile(path); err != nil {
		return "", nil, fmt.Errorf("%s not found (%s): %w", program, path, err)
	}

	for i, a := range strings.Split(options, " ") {
		switch program {
		case "ffmpeg":
			a = strings.Replace(a, "[URL]", url, -1)
			if i == 0 {
				if len(Settings.UserAgent) != 0 {
					args = []string{"-user_agent", Settings.UserAgent}
				}
				if proxyIP != "" && proxyPort != "" {
					args = append(args, "-http_proxy", fmt.Sprintf("http://%s:%s", proxyIP, proxyPort))
				}
				var headers string
				if referer != "" {
					headers += fmt.Sprintf("Referer: %s\r\n", referer)
				}
				if origin != "" {
					headers += fmt.Sprintf("Origin: %s\r\n", origin)
				}
				if headers != "" {
					args = append(args, "-headers", headers)
				}
			}
			args = append(args, a)

		case "vlc":
			if a != "[URL]" {
				args = append(args, a)
				continue
			}
			args = append(args, url)
			if len(Settings.UserAgent) != 0 {
				args = append(args, fmt.Sprintf(":http-user-agent=%s", Settings.UserAgent))
			}
			if referer != "" {
				args = append(args, fmt.Sprintf(":http-referrer=%s", referer))
			}
			if proxyIP != "" && proxyPort != "" {
				args = append(args, fmt.Sprintf(":http-proxy=%s:%s", proxyIP, proxyPort))
			}
		}
	}

	return path, args, nil
}

// bufferProcess is a running ffmpeg/VLC whose output feeds the buffer.
type bufferProcess struct {
	name   string
	cmd    *exec.Cmd
	stdout io.ReadCloser

	receiving atomic.Bool // output has started
	stopOnce  sync.Once

	logDone chan struct{}
	tailMu  sync.Mutex
	tail    []string
}

func startBufferProcess(name, path string, args []string) (*bufferProcess, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = append(os.Environ(), "DISPLAY=:0") // avoids issues with VLC
	setProcessGroup(cmd)

	// If a child process keeps the pipes open after the process is
	// stopped, Wait still returns.
	cmd.WaitDelay = 2 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s could not be started: %w", name, err)
	}

	p := &bufferProcess{name: name, cmd: cmd, stdout: stdout, logDone: make(chan struct{})}
	go p.readLog(stderr)
	return p, nil
}

func (p *bufferProcess) Read(b []byte) (int, error) {
	n, err := p.stdout.Read(b)
	if n > 0 && !p.receiving.Load() {
		p.receiving.Store(true)
	}
	return n, err
}

// Stop kills the process and anything it started.
func (p *bufferProcess) Stop() {
	p.stopOnce.Do(func() { killProcessGroup(p.cmd) })
}

func (p *bufferProcess) Wait() error {
	err := p.cmd.Wait()

	select {
	case <-p.logDone: // the full log, for the error message
	case <-time.After(200 * time.Millisecond):
	}

	p.tailMu.Lock()
	tail := strings.Join(p.tail, " | ")
	p.tailMu.Unlock()

	var exitErr *exec.ExitError
	switch {
	case err == nil && tail == "":
		return fmt.Errorf("%s ended the stream", p.name)
	case err == nil:
		return fmt.Errorf("%s ended the stream: %s", p.name, tail)
	case errors.As(err, &exitErr) && tail != "":
		return fmt.Errorf("%s exited (%v): %s", p.name, err, tail)
	default:
		return fmt.Errorf("%s exited: %w", p.name, err)
	}
}

// readLog shows the process log: in the normal log while the stream is
// starting, then only in debug mode. stderr is always drained, so a burst of
// log output can never block the process (and with it the video).
func (p *bufferProcess) readLog(stderr io.Reader) {
	defer close(p.logDone)

	scanner := bufio.NewScanner(stderr)
	shown := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		p.tailMu.Lock()
		p.tail = append(p.tail, line)
		if len(p.tail) > processTailLines {
			p.tail = p.tail[1:]
		}
		p.tailMu.Unlock()

		msg := fmt.Sprintf("%s log:%s", p.name, line)
		if !p.receiving.Load() && shown < processInfoLines {
			shown++
			showInfo(msg)
		} else {
			showDebug(msg, 1)
		}
	}
	io.Copy(io.Discard, stderr)
}
