package streambuf

import (
	"sync"
	"time"
)

// chunk is one immutable piece of the stream, as read from the process:
// whole TS packets in TS mode. Viewers share chunks without copying them.
// Its memory is pooled and reference counted (see pool.go).
type chunk struct {
	data  []byte
	class int // pool size class of data
	refs  refs
	seq   uint64
	at    time.Time
	join  int    // offset of the first join point (keyframe) in data, or -1
	psi   []byte // program tables valid at that point (TS only; not pooled)
}

// ring holds the most recent part of a stream for all its viewers.
//
// The writer appends chunks and never waits for viewers. Each viewer keeps
// its own position (a chunk sequence number); a viewer that falls further
// behind than the buffer reaches is moved forward to the live position
// instead of holding anything up.
type ring struct {
	mu sync.Mutex

	chunks []*chunk
	first  uint64 // seq of chunks[0]
	next   uint64 // seq of the next chunk to be appended
	bytes  int

	maxBytes int
	maxAge   time.Duration

	// Closed and replaced whenever a chunk is appended or the ring is
	// closed, waking every waiting viewer at once.
	notify chan struct{}

	closed bool
	err    error

	firstData time.Time // arrival of the first chunk ever
}

func newRing(maxBytes int, maxAge time.Duration) *ring {
	return &ring{maxBytes: maxBytes, maxAge: maxAge, notify: make(chan struct{})}
}

func (r *ring) append(c *chunk) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		c.release()
		return
	}

	c.seq = r.next
	r.next++
	r.chunks = append(r.chunks, c)
	r.bytes += len(c.data)
	if r.firstData.IsZero() {
		r.firstData = c.at
	}

	// Trim, always keeping the newest chunk.
	for len(r.chunks) > 1 && (r.bytes > r.maxBytes || c.at.Sub(r.chunks[0].at) > r.maxAge) {
		old := r.chunks[0]
		r.bytes -= len(old.data)
		old.release()
		r.chunks[0] = nil
		r.chunks = r.chunks[1:]
		r.first++
	}

	close(r.notify)
	r.notify = make(chan struct{})
}

func (r *ring) close(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}
	r.closed = true
	r.err = err
	close(r.notify)
}

type readResult int

const (
	readOK      readResult = iota
	readWait               // nothing new yet: wait on the returned channel
	readOverrun            // the position is no longer in the buffer
	readClosed             // the stream has ended and everything was read
)

// read appends up to max chunks from position *seq to dst and advances *seq.
// Each returned chunk is retained for the caller, who must release it.
func (r *ring) read(seq *uint64, max int, dst []*chunk) ([]*chunk, <-chan struct{}, readResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if *seq < r.first {
		return dst, nil, readOverrun, nil
	}

	avail := int(r.next - *seq)
	if avail == 0 {
		if r.closed {
			return dst, nil, readClosed, r.err
		}
		return dst, r.notify, readWait, nil
	}

	if avail > max {
		avail = max
	}
	start := int(*seq - r.first)
	for _, c := range r.chunks[start : start+avail] {
		c.retain()
		dst = append(dst, c)
	}
	*seq += uint64(avail)
	return dst, nil, readOK, nil
}

// joinPoint is where a viewer starts: chunk seq, byte offset into that
// chunk, and the program tables to send before it.
type joinPoint struct {
	seq    uint64
	offset int
	psi    []byte
}

// join picks the start position for a new viewer.
//
// It prefers the newest keyframe that leaves at least prebuffer bytes
// between it and the live position, so the viewer starts on a clean
// picture and immediately receives a cushion of buffered data. While a new
// stream is still filling, it waits for that, but for no longer than
// maxWait after the first data arrived (low-bitrate channels such as radio
// would otherwise wait a long time); then it takes the best start available.
//
// With live set, it ignores prebuffer and picks the newest keyframe (used to
// move a viewer that fell behind back to the live position).
func (r *ring) join(prebuffer int, maxWait time.Duration, live bool, now time.Time) (joinPoint, <-chan struct{}, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.chunks) == 0 {
		if r.closed {
			return joinPoint{}, nil, false, r.err
		}
		return joinPoint{}, r.notify, false, nil
	}

	if live {
		prebuffer = 0
	}

	// Walk back from the live position. best: newest join point with enough
	// data after it. oldest: oldest join point. byBytes: newest chunk start
	// with enough data after it, for streams without join points.
	oldest, best, byBytes := -1, -1, -1
	acc := 0
	for i := len(r.chunks) - 1; i >= 0; i-- {
		c := r.chunks[i]
		acc += len(c.data)
		if byBytes < 0 && acc >= prebuffer {
			byBytes = i
		}
		if c.join < 0 {
			continue
		}
		oldest = i
		if acc-c.join >= prebuffer {
			best = i
			break
		}
	}

	if best < 0 {
		waited := now.Sub(r.firstData) >= maxWait
		switch {
		case live:
			// No join point in the buffer: start at the newest chunk.
			best = byBytes
		case !waited && !r.closed:
			return joinPoint{}, r.notify, false, nil
		case oldest >= 0:
			best = oldest // as much cushion as there is, from a clean start
		case byBytes >= 0:
			best = byBytes
		default:
			best = 0
		}
	}

	c := r.chunks[best]
	off := c.join
	if off < 0 {
		off = 0
	}
	return joinPoint{seq: c.seq, offset: off, psi: c.psi}, nil, true, nil
}

// waitedSince reports when the first data arrived (zero if none yet).
func (r *ring) waitedSince() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.firstData
}
