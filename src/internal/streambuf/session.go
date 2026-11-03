// Package streambuf is Threadfin's stream buffer: one upstream connection
// (an ffmpeg or VLC process) per channel, shared by every viewer of that
// channel through an in-memory buffer.
//
//   - Data reaches viewers as soon as the process outputs it: no polling,
//     no temporary files, no fixed-size bursts.
//   - Viewers joining a running channel start on a video keyframe, with the
//     program tables sent first, and immediately get a cushion of buffered
//     data, so playback starts at once and cleanly.
//   - A slow viewer never slows the channel down or holds up other viewers.
//   - Sources that drop are reconnected; sources that fail are replaced by
//     the configured backups, while viewers stay connected.
//   - Tuner limits are enforced in one place, under one lock.
package streambuf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrNoTuner means the playlist's tuner limit is reached.
	ErrNoTuner = errors.New("no tuner available")
	// ErrStreamFailed means every source failed; it wraps the last error.
	ErrStreamFailed = errors.New("stream failed")
	// ErrStopped means the stream was stopped (no viewers left, or its
	// tuner was needed for another channel).
	ErrStopped = errors.New("stream stopped")

	errStartTimeout = errors.New("no data from the source")
	errStalled      = errors.New("the source stopped sending data")
)

// Config controls one stream. DefaultConfig returns the recommended values.
type Config struct {
	// Bytes a viewer receives at once when it starts (a cushion against
	// network jitter). 0 starts every viewer at the live position.
	Prebuffer int
	// Longest a brand-new stream waits to collect Prebuffer before the
	// first viewer starts anyway.
	PrebufferMaxWait time.Duration

	// How much recent data is kept: viewers can fall this far behind (a
	// network hiccup on their side) and catch up without losing anything.
	Retention      time.Duration
	MaxBufferBytes int

	// How long a source may take to send its first data, and how long a
	// running source may go silent, before it counts as failed.
	StartTimeout time.Duration
	StallTimeout time.Duration

	// A source that streamed at least this long before dropping is
	// reconnected first; one that fails sooner is replaced by the next
	// backup. RetryBackoff is the pause before retrying a source that
	// already failed (it grows with each failure, up to 2 s).
	HealthyAfter time.Duration
	RetryBackoff time.Duration

	// How long the stream keeps running after its last viewer leaves.
	// Clients such as Plex probe a stream and reconnect right away; the
	// second request then reuses the running stream instead of tuning
	// again. A lingering stream gives up its tuner at once if needed.
	Linger time.Duration

	// How long a new channel waits for a tuner when the limit is reached
	// (covers clients that open the new channel before closing the old).
	TunerWait time.Duration

	// Size of each read from the process.
	ReadSize int

	// A viewer at the live position receives data in pieces of at least
	// CoalesceBytes, or after the oldest part waited CoalesceDelay. ffmpeg
	// writes every frame separately; sending each on its own costs about
	// twice the CPU for no benefit to playback.
	CoalesceBytes int
	CoalesceDelay time.Duration
}

// DefaultConfig returns the recommended settings.
func DefaultConfig() Config {
	return Config{
		Prebuffer:        1 << 20,
		PrebufferMaxWait: 3 * time.Second,
		Retention:        15 * time.Second,
		MaxBufferBytes:   48 << 20,
		StartTimeout:     20 * time.Second,
		StallTimeout:     15 * time.Second,
		HealthyAfter:     10 * time.Second,
		RetryBackoff:     500 * time.Millisecond,
		Linger:           5 * time.Second,
		TunerWait:        2 * time.Second,
		ReadSize:         64 << 10,
		CoalesceBytes:    64 << 10,
		CoalesceDelay:    100 * time.Millisecond,
	}
}

// Source is one way to receive a channel: the primary URL or a backup.
type Source struct {
	PlaylistID   string
	PlaylistName string
	TunerLimit   int
	URL          string
	Label        string // "primary", "backup 1", ...
}

// Process is a running ffmpeg/VLC (or test) process. Read returns its
// output; Stop kills it (it must be safe to call more than once and from
// any goroutine); Wait returns its exit error once Read has returned EOF.
type Process interface {
	io.Reader
	Stop()
	Wait() error
}

// Launcher starts the process for a source.
type Launcher func(src Source) (Process, error)

// Request describes a channel to watch.
type Request struct {
	Channel string   // name, for the log
	Program string   // "FFMPEG" or "VLC", for the log
	Sources []Source // primary first, then backups
	Launch  Launcher
	Config  Config
}

// Logger receives the buffer's log messages. Nil functions are skipped.
type Logger struct {
	Info      func(string)
	Debug     func(string)
	Highlight func(string)
	Error     func(error)
}

func (l Logger) info(format string, a ...interface{}) {
	if l.Info != nil {
		l.Info(fmt.Sprintf(format, a...))
	}
}

func (l Logger) debug(format string, a ...interface{}) {
	if l.Debug != nil {
		l.Debug(fmt.Sprintf(format, a...))
	}
}

func (l Logger) highlight(format string, a ...interface{}) {
	if l.Highlight != nil {
		l.Highlight(fmt.Sprintf(format, a...))
	}
}

func (l Logger) error(err error) {
	if l.Error != nil {
		l.Error(err)
	}
}

// Manager owns every running stream and the tuner counts. All bookkeeping
// happens under its single mutex; the data path does not touch it.
type Manager struct {
	log Logger

	mu       sync.Mutex
	sessions map[string]*session
	tuners   map[string]int // playlist ID -> tuners in use
	freed    chan struct{}  // closed and replaced when a tuner is released
}

// NewManager creates an empty manager.
func NewManager(log Logger) *Manager {
	return &Manager{
		log:      log,
		sessions: map[string]*session{},
		tuners:   map[string]int{},
		freed:    make(chan struct{}),
	}
}

// Stats is a snapshot for the web interface.
type Stats struct {
	Streams   int // running channels
	Viewers   int // connected viewers
	Playlists int // playlists with at least one running channel
}

// Stats returns the current counts.
func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()

	var st Stats
	for _, s := range m.sessions {
		st.Streams++
		st.Viewers += s.viewers
	}
	st.Playlists = len(m.tuners)
	return st
}

func sessionKey(src Source) string {
	return src.PlaylistID + "\x00" + src.URL
}

// Join connects a viewer to a channel, starting the stream if it is not
// already running. Viewers of the same channel share one stream. When the
// tuner limit of the primary source's playlist is reached, Join waits up
// to Config.TunerWait for a tuner and then returns ErrNoTuner.
func (m *Manager) Join(ctx context.Context, req Request) (*Viewer, error) {
	if len(req.Sources) == 0 {
		return nil, errors.New("no source")
	}

	primary := req.Sources[0]
	key := sessionKey(primary)
	deadline := time.Now().Add(req.Config.TunerWait)

	m.mu.Lock()
	for {
		if s := m.sessions[key]; s != nil {
			v := s.addViewerLocked()
			n := s.viewers
			m.mu.Unlock()
			m.log.info("Streaming Status:Channel: %s (Clients: %d)", req.Channel, n)
			return v, nil
		}

		if m.tuners[primary.PlaylistID] < primary.TunerLimit || m.evictLingeringLocked(primary.PlaylistID) {
			s := newSession(m, key, req)
			m.sessions[key] = s
			m.tuners[primary.PlaylistID]++
			s.tunerPlaylist = primary.PlaylistID
			used := m.tuners[primary.PlaylistID]
			v := s.addViewerLocked()
			m.mu.Unlock()

			m.log.info("Streaming Status:Playlist: %s - Tuner: %d / %d", primary.PlaylistName, used, primary.TunerLimit)
			go s.run()
			return v, nil
		}

		freed := m.freed
		m.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, ErrNoTuner
		}
		timer := time.NewTimer(remaining)
		select {
		case <-freed:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
		timer.Stop()
		m.mu.Lock()
	}
}

// evictLingeringLocked stops a stream of the playlist that has no viewers,
// freeing its tuner. It reports whether one was found.
func (m *Manager) evictLingeringLocked(playlistID string) bool {
	for _, s := range m.sessions {
		if s.tunerPlaylist == playlistID && s.viewers == 0 {
			m.log.debug("Streaming Status:Channel: %s - giving its tuner to a new channel", s.req.Channel)
			m.stopLocked(s)
			return true
		}
	}
	return false
}

// stopLocked marks the session as ended: it leaves the map (new viewers of
// the channel start a fresh stream) and gives back its tuner at once.
func (m *Manager) stopLocked(s *session) {
	if s.closing {
		return
	}
	s.closing = true
	if s.lingerTimer != nil {
		s.lingerTimer.Stop()
	}
	if m.sessions[s.key] == s {
		delete(m.sessions, s.key)
	}
	m.releaseTunerLocked(s)
	s.cancel()
}

func (m *Manager) releaseTunerLocked(s *session) {
	if s.tunerPlaylist == "" {
		return
	}
	if m.tuners[s.tunerPlaylist]--; m.tuners[s.tunerPlaylist] <= 0 {
		delete(m.tuners, s.tunerPlaylist)
	}
	s.tunerPlaylist = ""
	close(m.freed)
	m.freed = make(chan struct{})
}

// moveTuner makes the session hold a tuner of the source's playlist,
// giving back the one it holds now. It reports false if none is free.
func (m *Manager) moveTuner(s *session, src Source) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s.closing {
		return false
	}
	if s.tunerPlaylist == src.PlaylistID {
		return true
	}
	if m.tuners[src.PlaylistID] >= src.TunerLimit && !m.evictLingeringLocked(src.PlaylistID) {
		return false
	}
	m.releaseTunerLocked(s)
	m.tuners[src.PlaylistID]++
	s.tunerPlaylist = src.PlaylistID
	return true
}

func (m *Manager) viewerLeft(s *session) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s.viewers--
	m.log.info("Streaming Status:Channel: %s (Clients: %d)", s.req.Channel, s.viewers)

	if s.viewers > 0 || s.closing {
		return
	}

	linger := s.req.Config.Linger
	if linger <= 0 {
		m.stopLocked(s)
		return
	}
	s.lingerTimer = time.AfterFunc(linger, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if s.viewers == 0 {
			m.stopLocked(s)
		}
	})
}

// session is one running channel.
type session struct {
	m    *Manager
	key  string
	req  Request
	ring *ring

	ctx    context.Context
	cancel context.CancelFunc

	// Guarded by m.mu.
	viewers       int
	closing       bool
	tunerPlaylist string
	lingerTimer   *time.Timer

	launches atomic.Int32 // processes started (for tests)
}

func newSession(m *Manager, key string, req Request) *session {
	cfg := req.Config
	maxBytes := cfg.MaxBufferBytes
	if min := 4 * cfg.Prebuffer; maxBytes < min {
		maxBytes = min
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &session{
		m:      m,
		key:    key,
		req:    req,
		ring:   newRing(maxBytes, cfg.Retention),
		ctx:    ctx,
		cancel: cancel,
	}
}

func (s *session) addViewerLocked() *Viewer {
	s.viewers++
	if s.lingerTimer != nil {
		s.lingerTimer.Stop()
		s.lingerTimer = nil
	}
	return &Viewer{s: s}
}

// run supervises the stream until it is stopped or every source failed.
func (s *session) run() {
	var finalErr = ErrStopped
	defer func() {
		s.m.mu.Lock()
		s.m.stopLocked(s)
		s.m.mu.Unlock()
		s.ring.close(finalErr)
		if errors.Is(finalErr, ErrStopped) {
			s.m.log.info("Streaming Status:Channel: %s - No client is using this channel anymore. Streaming Server connection has ended", s.req.Channel)
		}
	}()

	cfg := s.req.Config
	sources := s.req.Sources
	idx, failures := 0, 0

	// Every source may fail once in a row, and the first one a second time,
	// before giving up.
	maxFailures := len(sources) + 1

	for s.ctx.Err() == nil {
		src := sources[idx]

		var streamed time.Duration
		var err error

		if !s.m.moveTuner(s, src) {
			err = fmt.Errorf("%s: no free tuner on playlist %s", src.Label, src.PlaylistName)
		} else {
			if idx > 0 {
				s.m.log.highlight("START OF %s STREAM", upper(src.Label))
				s.m.log.info("Streaming Status:Channel: %s - switching to %s: %s", s.req.Channel, src.Label, src.URL)
			}
			streamed, err = s.attempt(src)
		}

		if s.ctx.Err() != nil {
			return
		}

		if streamed >= cfg.HealthyAfter {
			// It worked for a while: most likely a hiccup. Reconnect at once.
			failures = 0
			s.m.log.info("Streaming Status:Channel: %s - %s dropped after %v (%v), reconnecting", s.req.Channel, src.Label, streamed.Round(time.Second), err)
			continue
		}

		failures++
		s.m.log.error(fmt.Errorf("Channel %s, %s: %w", s.req.Channel, src.Label, err))

		if failures >= maxFailures {
			finalErr = fmt.Errorf("%w: %v", ErrStreamFailed, err)
			return
		}

		// Switch to the next source at once; pause only before trying a
		// source that has already failed in this run of failures.
		idx = (idx + 1) % len(sources)
		if failures >= len(sources) {
			backoff := time.Duration(failures) * cfg.RetryBackoff
			if backoff > 2*time.Second {
				backoff = 2 * time.Second
			}
			if !s.sleep(backoff) {
				return
			}
		}
	}
}

func (s *session) sleep(d time.Duration) bool {
	if d <= 0 {
		return s.ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// attempt runs one process until it ends, stalls, or the session stops.
// It returns how long the source delivered data.
func (s *session) attempt(src Source) (streamed time.Duration, err error) {
	cfg := s.req.Config

	s.m.log.info("Streaming Status:Channel: %s - starting %s (%s)", s.req.Channel, s.req.Program, src.Label)

	proc, err := s.req.Launch(src)
	if err != nil {
		return 0, err
	}
	s.launches.Add(1)

	start := time.Now()
	var lastData atomic.Int64 // unix nanoseconds, 0 = no data yet
	var failure atomic.Value  // error set by the watchdog
	done := make(chan struct{})

	// Watchdog: the read below blocks while the process sends nothing, so
	// stalls and stop requests are handled here; killing the process ends
	// the read.
	go func() {
		tick := minDuration(time.Second, cfg.StallTimeout/4, cfg.StartTimeout/4)
		if tick <= 0 {
			tick = time.Second
		}
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-s.ctx.Done():
				proc.Stop()
				return
			case now := <-t.C:
				last := lastData.Load()
				switch {
				case last == 0 && now.Sub(start) > cfg.StartTimeout:
					failure.Store(fmt.Errorf("%w for %v", errStartTimeout, cfg.StartTimeout))
				case last != 0 && now.Sub(time.Unix(0, last)) > cfg.StallTimeout:
					failure.Store(fmt.Errorf("%w for %v", errStalled, cfg.StallTimeout))
				default:
					continue
				}
				proc.Stop()
				return
			}
		}
	}()

	var pk packetizer
	ins := newInspector()
	var firstData time.Time
	buf := make([]byte, cfg.ReadSize)

	for {
		n, rerr := proc.Read(buf)
		if n > 0 {
			now := time.Now()
			lastData.Store(now.UnixNano())
			if firstData.IsZero() {
				firstData = now
				s.m.log.info("Streaming Status:Channel: %s - receiving data from %s", s.req.Channel, s.req.Program)
			}
			if out := pk.push(buf[:n]); len(out) > 0 {
				s.ring.append(makeChunk(out, pk.mode, ins, now))
			}
		}
		if rerr != nil {
			if out := pk.flush(); len(out) > 0 {
				s.ring.append(makeChunk(out, pk.mode, ins, time.Now()))
			}
			break
		}
	}

	close(done)
	proc.Stop()
	waitErr := proc.Wait()

	if pk.dropped > 0 {
		s.m.log.debug("Streaming Status:Channel: %s - skipped %d corrupt bytes", s.req.Channel, pk.dropped)
	}

	if !firstData.IsZero() {
		streamed = time.Unix(0, lastData.Load()).Sub(firstData)
	}

	switch f := failure.Load(); {
	case f != nil:
		err = f.(error)
	case waitErr != nil:
		err = waitErr
	default:
		err = errors.New("the source ended the stream")
	}
	return streamed, err
}

// makeChunk copies output from the packetizer into a pooled chunk (held by
// the ring once appended) and finds its first join point.
func makeChunk(out []byte, mode packetizerMode, ins *inspector, at time.Time) *chunk {
	data, class := newChunkData(out)
	c := &chunk{data: data, class: class, at: at, join: -1}
	c.refs.n.Store(1)

	if mode != modeTS {
		c.join = 0 // no structure known: any position is as good as another
		return c
	}

	for off := 0; off+tsPacketSize <= len(data); off += tsPacketSize {
		if ins.packet(data[off:off+tsPacketSize]) && c.join < 0 {
			c.join = off
			c.psi = ins.psi
		}
	}
	return c
}

func minDuration(ds ...time.Duration) time.Duration {
	m := ds[0]
	for _, d := range ds[1:] {
		if d > 0 && (m <= 0 || d < m) {
			m = d
		}
	}
	return m
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// maxBatch is the most chunks a viewer receives per Next call.
const maxBatch = 256

// Viewer is one client watching a channel; each client connection has its
// own. Next and WaitReady must be called from one goroutine at a time.
// Close may be called from any goroutine; slices returned by Next must not
// be used after it.
type Viewer struct {
	s *session

	joined bool
	seq    uint64
	offset int    // start offset into the first chunk after a join
	psi    []byte // program tables to send first after a join

	scratch []*chunk
	out     [][]byte
	skips   int
	timer   *time.Timer

	// Chunks of the batch returned by the last Next call, released at the
	// next call or on Close.
	mu     sync.Mutex
	held   []*chunk
	closed bool

	closeOnce sync.Once
}

// ErrViewerClosed is returned by Next after Close.
var ErrViewerClosed = errors.New("viewer closed")

func (v *Viewer) releaseHeld() {
	for i, c := range v.held {
		c.release()
		v.held[i] = nil
	}
	v.held = v.held[:0]
}

// Channel returns the channel name.
func (v *Viewer) Channel() string { return v.s.req.Channel }

// WaitReady blocks until the viewer can start (enough data is buffered),
// the stream fails, or ctx ends.
func (v *Viewer) WaitReady(ctx context.Context) error {
	if v.joined {
		return nil
	}
	return v.joinWait(ctx, false)
}

func (v *Viewer) joinWait(ctx context.Context, live bool) error {
	cfg := v.s.req.Config
	for {
		jp, wait, ok, err := v.s.ring.join(cfg.Prebuffer, cfg.PrebufferMaxWait, live, time.Now())
		if ok {
			v.joined = true
			v.seq, v.offset, v.psi = jp.seq, jp.offset, jp.psi
			return nil
		}
		if err != nil {
			return err
		}

		// Wake up for new data, or when the prebuffer wait is over.
		var timer *time.Timer
		var timeout <-chan time.Time
		if first := v.s.ring.waitedSince(); !first.IsZero() {
			timer = time.NewTimer(time.Until(first.Add(cfg.PrebufferMaxWait)) + time.Millisecond)
			timeout = timer.C
		}

		select {
		case <-wait:
		case <-timeout:
		case <-ctx.Done():
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// Next blocks until there is data for the viewer and returns it. The
// returned slices are shared and must not be modified, and they are only
// valid until the next call to Next or Close. It returns an error when the
// stream has ended (ErrStreamFailed or ErrStopped) or ctx ends.
func (v *Viewer) Next(ctx context.Context) ([][]byte, error) {
	v.mu.Lock()
	v.releaseHeld()
	closed := v.closed
	v.mu.Unlock()
	if closed {
		return nil, ErrViewerClosed
	}

	if err := v.WaitReady(ctx); err != nil {
		return nil, err
	}

	// At the live position, data is collected until there is
	// CoalesceBytes of it or its oldest part has waited CoalesceDelay, so
	// it can be sent with few system calls. Data that is already older
	// (a burst after joining, a viewer catching up) is returned at once.
	cfg := v.s.req.Config
	out := v.out[:0]
	size := 0
	var oldest time.Time

	for {
		chunks, wait, res, err := v.s.ring.read(&v.seq, maxBatch-len(out), v.scratch[:0])
		v.scratch = chunks[:0]

		if len(chunks) > 0 {
			v.mu.Lock()
			if v.closed {
				for _, c := range chunks {
					c.release()
				}
				v.mu.Unlock()
				return nil, ErrViewerClosed
			}
			v.held = append(v.held, chunks...)
			v.mu.Unlock()
		}

		switch res {
		case readOK:
			for i, c := range chunks {
				data := c.data
				if v.offset > 0 || v.psi != nil { // first chunk after a join
					if v.psi != nil {
						out = append(out, v.psi)
						size += len(v.psi)
					}
					data = data[v.offset:]
					v.offset, v.psi = 0, nil
				}
				if len(data) > 0 {
					out = append(out, data)
					size += len(data)
				}
				if oldest.IsZero() {
					oldest = c.at
				}
				chunks[i] = nil
			}
			if size >= cfg.CoalesceBytes || len(out) >= maxBatch || time.Since(oldest) >= cfg.CoalesceDelay {
				v.out = out
				return out, nil
			}

		case readOverrun:
			if len(out) > 0 {
				v.out = out
				return out, nil
			}
			v.skips++
			v.s.m.log.info("Streaming Status:Channel: %s - a client could not keep up and was moved to the live position", v.s.req.Channel)
			if err := v.joinWait(ctx, true); err != nil {
				return nil, err
			}

		case readClosed:
			if len(out) > 0 {
				v.out = out
				return out, nil // the error follows on the next call
			}
			return nil, err

		case readWait:
			if len(out) == 0 {
				select {
				case <-wait:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				continue
			}

			d := cfg.CoalesceDelay - time.Since(oldest)
			if d <= 0 {
				v.out = out
				return out, nil
			}
			select {
			case <-wait:
			case <-v.after(d):
				v.out = out
				return out, nil
			case <-ctx.Done():
				v.out = out
				return out, ctx.Err()
			}
		}
	}
}

// after returns a channel that receives after d, reusing one timer.
func (v *Viewer) after(d time.Duration) <-chan time.Time {
	if v.timer == nil {
		v.timer = time.NewTimer(d)
		return v.timer.C
	}
	if !v.timer.Stop() {
		select {
		case <-v.timer.C:
		default:
		}
	}
	v.timer.Reset(d)
	return v.timer.C
}

// Close disconnects the viewer and releases the data of its last batch.
// The stream stops once its last viewer has left (after Config.Linger).
// Close may be called more than once.
func (v *Viewer) Close() {
	v.mu.Lock()
	v.closed = true
	v.releaseHeld()
	v.mu.Unlock()
	v.closeOnce.Do(func() { v.s.m.viewerLeft(v.s) })
}
