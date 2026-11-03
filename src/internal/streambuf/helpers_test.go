package streambuf

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- synthetic MPEG-TS ----------------------------------------------------

const (
	testPMTPID   = 0x1000
	testVideoPID = 0x100
	testAudioPID = 0x101
)

// tsPacket builds a packet. With rai set it carries an adaptation field with
// the random access flag, as muxers write it on keyframes.
func tsPacket(pid uint16, pusi, rai bool, cc byte, payload []byte) []byte {
	p := make([]byte, tsPacketSize)
	p[0] = tsSyncByte
	p[1] = byte(pid>>8) & 0x1f
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	off := 4
	if rai {
		p[3] = 0x30 | cc&0x0f // adaptation field + payload
		p[4] = 1              // adaptation field length
		p[5] = 0x40           // random access indicator
		off = 6
	} else {
		p[3] = 0x10 | cc&0x0f // payload only
	}
	n := copy(p[off:], payload)
	for i := off + n; i < tsPacketSize; i++ {
		p[i] = 0xff
	}
	return p
}

func psiPacket(pid uint16, cc byte, sec []byte) []byte {
	return tsPacket(pid, true, false, cc, append([]byte{0}, sec...)) // pointer field 0
}

// psiSection wraps a table body in a section header and a (dummy) CRC.
func psiSection(tableID byte, id uint16, body []byte) []byte {
	length := 5 + len(body) + 4
	s := []byte{tableID, 0xb0 | byte(length>>8)&0x0f, byte(length), byte(id >> 8), byte(id), 0xc1, 0, 0}
	s = append(s, body...)
	return append(s, 0, 0, 0, 0)
}

func patPacket(cc byte) []byte {
	body := []byte{0, 1, 0xe0 | byte(testPMTPID>>8), byte(testPMTPID & 0xff)}
	return psiPacket(0, cc, psiSection(0x00, 1, body))
}

// pmtPacket lists elementary streams as (stream type, PID) pairs.
func pmtPacket(cc byte, streams ...[2]uint16) []byte {
	body := []byte{0xe0 | byte(testVideoPID>>8), byte(testVideoPID & 0xff), 0xf0, 0x00}
	for _, st := range streams {
		body = append(body, byte(st[0]), 0xe0|byte(st[1]>>8), byte(st[1]), 0xf0, 0x00)
	}
	return psiPacket(testPMTPID, cc, psiSection(0x02, 1, body))
}

var avStreams = [][2]uint16{{0x1b, testVideoPID}, {0x0f, testAudioPID}}

// frameMagic marks the first video packet of a frame: magic, frame number,
// keyframe flag. Lets tests see where a viewer's video starts.
var frameMagic = []byte("FRM!")

// tsGen produces a synthetic A/V transport stream: PAT+PMT every 10 frames,
// 4 video packets and 1 audio packet per frame, a keyframe every gop frames.
type tsGen struct {
	gop   int
	frame int
	cc    map[uint16]byte
}

func newTSGen(gop int) *tsGen { return &tsGen{gop: gop, cc: map[uint16]byte{}} }

func (g *tsGen) next(pid uint16) byte {
	c := g.cc[pid]
	g.cc[pid] = c + 1
	return c & 0x0f
}

func (g *tsGen) frameBytes() []byte {
	var out []byte
	if g.frame%10 == 0 {
		out = append(out, patPacket(g.next(0))...)
		out = append(out, pmtPacket(g.next(testPMTPID), avStreams...)...)
	}
	key := g.frame%g.gop == 0
	hdr := make([]byte, 9)
	copy(hdr, frameMagic)
	binary.BigEndian.PutUint32(hdr[4:], uint32(g.frame))
	if key {
		hdr[8] = 1
	}
	out = append(out, tsPacket(testVideoPID, true, key, g.next(testVideoPID), hdr)...)
	for i := 0; i < 3; i++ {
		out = append(out, tsPacket(testVideoPID, false, false, g.next(testVideoPID), nil)...)
	}
	// ffmpeg flags every audio frame as random access.
	out = append(out, tsPacket(testAudioPID, true, true, g.next(testAudioPID), []byte("AUD"))...)
	g.frame++
	return out
}

// firstFrame finds the first video frame header in a TS byte stream.
func firstFrame(t *testing.T, b []byte) (frame int, key bool) {
	t.Helper()
	for off := 0; off+tsPacketSize <= len(b); off += tsPacketSize {
		p := b[off : off+tsPacketSize]
		pid := uint16(p[1]&0x1f)<<8 | uint16(p[2])
		if pid != testVideoPID || p[1]&0x40 == 0 {
			continue
		}
		pl := p[4:]
		if p[3]&0x20 != 0 {
			pl = p[5+int(p[4]):]
		}
		if len(pl) >= 9 && string(pl[:4]) == string(frameMagic) {
			return int(binary.BigEndian.Uint32(pl[4:8])), pl[8] == 1
		}
	}
	t.Fatalf("no video frame found in %d bytes", len(b))
	return 0, false
}

// ---- fake processes -----------------------------------------------------

// behaviour of a fake source.
type behaviour struct {
	runFor  time.Duration // stream this long, then end (0 = forever)
	hang    bool          // after runFor, stop sending but stay alive
	failNow bool          // exit at once without output
	exitErr error         // error returned by Wait
	rate    time.Duration // pause between frames (default 2ms)
	gop     int           // keyframe interval in frames (default 25)
}

type fakeProc struct {
	pr       *io.PipeReader
	pw       *io.PipeWriter
	stop     chan struct{}
	stopOnce sync.Once
	exited   chan struct{}
	exitErr  error
}

func startFake(b behaviour) *fakeProc {
	pr, pw := io.Pipe()
	p := &fakeProc{pr: pr, pw: pw, stop: make(chan struct{}), exited: make(chan struct{}), exitErr: b.exitErr}
	if b.rate == 0 {
		b.rate = 2 * time.Millisecond
	}
	if b.gop == 0 {
		b.gop = 25
	}
	go func() {
		defer close(p.exited)
		defer pw.Close()
		if b.failNow {
			return
		}
		gen := newTSGen(b.gop)
		start := time.Now()
		for b.runFor == 0 || time.Since(start) < b.runFor {
			if _, err := pw.Write(gen.frameBytes()); err != nil {
				return
			}
			select {
			case <-p.stop:
				return
			case <-time.After(b.rate):
			}
		}
		if b.hang {
			<-p.stop
		}
	}()
	return p
}

func (p *fakeProc) Read(b []byte) (int, error) { return p.pr.Read(b) }

func (p *fakeProc) Stop() {
	p.stopOnce.Do(func() {
		close(p.stop)
		p.pr.CloseWithError(io.EOF)
	})
}

func (p *fakeProc) Wait() error {
	<-p.exited
	return p.exitErr
}

// fakeLauncher starts fake processes by URL and records every launch.
type fakeLauncher struct {
	mu         sync.Mutex
	behaviours map[string]behaviour
	launches   []string
	running    atomic.Int32
}

func newFakeLauncher(b map[string]behaviour) *fakeLauncher {
	return &fakeLauncher{behaviours: b}
}

func (l *fakeLauncher) launch(src Source) (Process, error) {
	l.mu.Lock()
	b, ok := l.behaviours[src.URL]
	l.launches = append(l.launches, src.URL)
	l.mu.Unlock()
	if !ok {
		return nil, errors.New("unknown test source " + src.URL)
	}
	p := startFake(b)
	l.running.Add(1)
	go func() { <-p.exited; l.running.Add(-1) }()
	return p, nil
}

func (l *fakeLauncher) launched() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.launches...)
}

// ---- helpers ------------------------------------------------------------

// Released chunk memory is overwritten, so any use after release corrupts
// the packets the tests check.
func init() { poisonReleased = true }

func testConfig() Config {
	c := DefaultConfig()
	c.Prebuffer = 32 << 10
	c.PrebufferMaxWait = 500 * time.Millisecond
	c.Retention = 5 * time.Second
	c.StartTimeout = time.Second
	c.StallTimeout = 500 * time.Millisecond
	c.HealthyAfter = 400 * time.Millisecond
	c.RetryBackoff = 20 * time.Millisecond
	c.Linger = 200 * time.Millisecond
	c.TunerWait = 0
	c.CoalesceDelay = 10 * time.Millisecond
	return c
}

func src(playlist, url string) Source {
	return Source{PlaylistID: playlist, PlaylistName: playlist, TunerLimit: 2, URL: url, Label: url}
}

func request(l *fakeLauncher, cfg Config, sources ...Source) Request {
	return Request{Channel: sources[0].URL, Program: "TEST", Sources: sources, Launch: l.launch, Config: cfg}
}

// readFor reads from the viewer for d and returns everything received.
func readFor(t *testing.T, v *Viewer, d time.Duration) ([]byte, error) {
	t.Helper()
	ctx, cancel := contextWithTimeout(d)
	defer cancel()
	var out []byte
	for {
		bufs, err := v.Next(ctx)
		for _, b := range bufs {
			out = append(out, b...)
		}
		if err != nil {
			if ctx.Err() != nil {
				return out, nil
			}
			return out, err
		}
	}
}

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// eventually polls cond until it holds or d elapses.
func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for: %s", d, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
