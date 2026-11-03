package streambuf

import "bytes"

// MPEG transport stream handling.
//
// The buffer does not demux or rewrite the stream. It only needs to know:
//   - where packets start, so chunks always hold whole 188-byte packets;
//   - the current PAT/PMT, so a viewer joining mid-stream can be sent the
//     program tables first;
//   - where video keyframes start, so a joining viewer begins on a picture
//     the decoder can show immediately instead of seconds of grey or green.

const (
	tsPacketSize = 188
	tsSyncByte   = 0x47

	// Bytes to inspect before deciding the output is not MPEG-TS and passing
	// it through untouched (e.g. a custom "-f matroska" in the ffmpeg options).
	tsDetectLimit = 64 * 1024
)

type packetizerMode int

const (
	modeDetect packetizerMode = iota
	modeTS
	modeOpaque
)

// packetizer turns the arbitrary reads from a process pipe into whole,
// aligned TS packets, and resynchronises if the stream is corrupted.
// Anything that is not TS is passed through as is.
type packetizer struct {
	pending  []byte
	consumed int    // bytes at the start of pending returned by the last call
	out      []byte // assembles output in the rare case it is not contiguous
	mode     packetizerMode
	dropped  int64 // bytes discarded while finding packet sync
}

// push adds data read from the process and returns the bytes that are ready
// to be published: whole packets in TS mode, everything in pass-through mode.
// The returned slice is only valid until the next call.
func (p *packetizer) push(in []byte) []byte {
	if p.consumed > 0 {
		p.discard(p.consumed)
		p.consumed = 0
	}
	p.pending = append(p.pending, in...)
	p.out = p.out[:0]

	for {
		switch p.mode {

		case modeOpaque:
			return p.publish(len(p.pending))

		case modeDetect:
			if off, ok := findSync(p.pending); ok {
				p.discard(off)
				p.mode = modeTS
				continue
			}
			if len(p.pending) >= tsDetectLimit {
				p.mode = modeOpaque
				continue
			}
			return nil

		case modeTS:
			// A packet is complete once the next one is seen to start with a
			// sync byte too; the newest packet waits for the next read. This
			// rejects a truncated packet followed by noise.
			n := 0
			for n+tsPacketSize < len(p.pending) && p.pending[n] == tsSyncByte && p.pending[n+tsPacketSize] == tsSyncByte {
				n += tsPacketSize
			}

			rest := p.pending[n:]
			if len(rest) == 0 || (rest[0] == tsSyncByte && len(rest) <= tsPacketSize) {
				// The rest is empty, or the newest packet, still waiting to
				// be confirmed.
				return p.publish(n)
			}

			if n > 0 {
				p.out = append(p.out, p.pending[:n]...)
				p.discard(n)
			}

			// Lost sync: skip to the next position followed by two more sync
			// bytes at packet distance.
			if off, ok := findSync(p.pending); ok {
				p.dropped += int64(off)
				p.discard(off)
				continue
			}

			// Not enough data to find sync yet. Keep only the tail that could
			// still hold the start of a valid packet.
			if keep := 2 * tsPacketSize; len(p.pending) > keep {
				drop := len(p.pending) - keep
				p.dropped += int64(drop)
				p.discard(drop)
			}
			return p.publish(0)
		}
	}
}

// publish returns the first n bytes of pending (after anything assembled
// in out), to be discarded at the next call.
func (p *packetizer) publish(n int) []byte {
	if len(p.out) > 0 {
		p.out = append(p.out, p.pending[:n]...)
		p.discard(n)
		return p.out
	}
	if n == 0 {
		return nil
	}
	p.consumed = n
	return p.pending[:n]
}

// flush returns what is still held back when the stream ends: the newest
// packet if it is whole (pass-through mode never holds anything back). The
// returned slice is only valid until the next call.
func (p *packetizer) flush() []byte {
	if p.consumed > 0 {
		p.discard(p.consumed)
		p.consumed = 0
	}
	if p.mode != modeTS || len(p.pending) != tsPacketSize || p.pending[0] != tsSyncByte {
		p.pending = p.pending[:0]
		return nil
	}
	p.consumed = len(p.pending)
	return p.pending
}

func (p *packetizer) discard(n int) {
	m := copy(p.pending, p.pending[n:])
	p.pending = p.pending[:m]
}

// findSync returns the first offset with sync bytes at it and at the next
// two packet positions.
func findSync(b []byte) (int, bool) {
	for i := 0; i+2*tsPacketSize < len(b); i++ {
		if b[i] == tsSyncByte && b[i+tsPacketSize] == tsSyncByte && b[i+2*tsPacketSize] == tsSyncByte {
			return i, true
		}
	}
	return 0, false
}

// Stream types (ISO/IEC 13818-1 and common extensions) carrying video.
var videoStreamTypes = map[byte]bool{
	0x01: true, // MPEG-1 video
	0x02: true, // MPEG-2 video
	0x10: true, // MPEG-4 part 2
	0x1B: true, // H.264
	0x24: true, // H.265
	0x33: true, // H.266
	0x42: true, // AVS
	0xD1: true, // Dirac
	0xEA: true, // VC-1
}

// If a video PID has had this many frame starts without a single random
// access flag, the muxer does not set them; join on any frame start instead.
const noRAIFallbackFrames = 250

// inspector tracks the program tables of a TS and finds join points.
type inspector struct {
	pmtPIDs   map[uint16]bool
	videoPIDs map[uint16]bool
	esPIDs    map[uint16]bool

	pat  []byte
	pmts map[uint16][]byte

	// PAT followed by the PMTs: what a joining viewer is sent first. It is
	// replaced (never modified) when the tables change.
	psi []byte

	videoFrames int
	videoRAIs   int
}

func newInspector() *inspector {
	return &inspector{
		pmtPIDs:   map[uint16]bool{},
		videoPIDs: map[uint16]bool{},
		esPIDs:    map[uint16]bool{},
		pmts:      map[uint16][]byte{},
	}
}

// packet inspects one 188-byte packet and reports whether a viewer can
// start playback at it.
func (in *inspector) packet(pkt []byte) bool {
	pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
	pusi := pkt[1]&0x40 != 0

	switch {
	case pid == 0:
		if pusi {
			in.parsePAT(pkt)
		}
		return false
	case in.pmtPIDs[pid]:
		if pusi {
			in.parsePMT(pid, pkt)
		}
		return false
	}

	if !pusi {
		return false
	}

	if len(in.videoPIDs) > 0 {
		if !in.videoPIDs[pid] {
			return false
		}
		in.videoFrames++
		if hasRAI(pkt) {
			in.videoRAIs++
			return true
		}
		return in.videoRAIs == 0 && in.videoFrames >= noRAIFallbackFrames
	}

	// Audio-only program (radio): every frame start is a join point.
	return in.esPIDs[pid]
}

func hasRAI(pkt []byte) bool {
	afc := pkt[3] >> 4 & 3
	return afc&2 != 0 && pkt[4] > 0 && pkt[5]&0x40 != 0
}

// section returns the PSI section starting in this packet, or nil if there
// is none or it continues into the next packet (not needed for PAT/PMT in
// practice; such tables are simply not cached).
func section(pkt []byte) []byte {
	afc := pkt[3] >> 4 & 3
	off := 4
	if afc&2 != 0 {
		off += 1 + int(pkt[4])
	}
	if afc&1 == 0 || off >= tsPacketSize {
		return nil
	}
	pl := pkt[off:]
	start := 1 + int(pl[0])
	if start+3 > len(pl) {
		return nil
	}
	sec := pl[start:]
	length := 3 + (int(sec[1]&0x0f)<<8 | int(sec[2]))
	if length > len(sec) || length < 12 {
		return nil
	}
	if sec[5]&0x01 == 0 { // not yet applicable ("next" table)
		return nil
	}
	return sec[:length]
}

func (in *inspector) parsePAT(pkt []byte) {
	sec := section(pkt)
	if sec == nil || sec[0] != 0x00 {
		return
	}

	pmtPIDs := map[uint16]bool{}
	for i := 8; i+4 <= len(sec)-4; i += 4 {
		program := uint16(sec[i])<<8 | uint16(sec[i+1])
		pid := uint16(sec[i+2]&0x1f)<<8 | uint16(sec[i+3])
		if program != 0 { // program 0 is the network information table
			pmtPIDs[pid] = true
		}
	}

	if !samePIDs(pmtPIDs, in.pmtPIDs) {
		in.pmtPIDs = pmtPIDs
		for pid := range in.pmts {
			if !pmtPIDs[pid] {
				delete(in.pmts, pid)
			}
		}
		in.videoPIDs = map[uint16]bool{}
		in.esPIDs = map[uint16]bool{}
	}

	if !samePacket(in.pat, pkt) {
		in.pat = append([]byte(nil), pkt...)
		in.rebuildPSI()
	}
}

func (in *inspector) parsePMT(pid uint16, pkt []byte) {
	sec := section(pkt)
	if sec == nil || sec[0] != 0x02 {
		return
	}

	video := map[uint16]bool{}
	es := map[uint16]bool{}

	infoLen := int(sec[10]&0x0f)<<8 | int(sec[11])
	end := len(sec) - 4 // CRC
	for i := 12 + infoLen; i+5 <= end; {
		streamType := sec[i]
		esPID := uint16(sec[i+1]&0x1f)<<8 | uint16(sec[i+2])
		esInfoLen := int(sec[i+3]&0x0f)<<8 | int(sec[i+4])
		es[esPID] = true
		if videoStreamTypes[streamType] {
			video[esPID] = true
		}
		i += 5 + esInfoLen
	}

	if !samePIDs(video, in.videoPIDs) {
		in.videoFrames, in.videoRAIs = 0, 0
	}
	in.videoPIDs = video
	in.esPIDs = es

	if !samePacket(in.pmts[pid], pkt) {
		in.pmts[pid] = append([]byte(nil), pkt...)
		in.rebuildPSI()
	}
}

func (in *inspector) rebuildPSI() {
	if in.pat == nil || len(in.pmts) == 0 {
		in.psi = nil
		return
	}
	psi := make([]byte, 0, tsPacketSize*(1+len(in.pmts)))
	psi = append(psi, in.pat...)
	for _, pid := range sortedPIDs(in.pmts) {
		psi = append(psi, in.pmts[pid]...)
	}
	in.psi = psi
}

// samePacket compares two packets, ignoring the continuity counter, which
// changes on every repetition of the same table.
func samePacket(a, b []byte) bool {
	return len(a) == len(b) && len(a) == tsPacketSize &&
		bytes.Equal(a[:3], b[:3]) && a[3]&0xf0 == b[3]&0xf0 && bytes.Equal(a[4:], b[4:])
}

func samePIDs(a, b map[uint16]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedPIDs(m map[uint16][]byte) []uint16 {
	pids := make([]uint16, 0, len(m))
	for pid := range m {
		pids = append(pids, pid)
	}
	for i := 1; i < len(pids); i++ {
		for j := i; j > 0 && pids[j] < pids[j-1]; j-- {
			pids[j], pids[j-1] = pids[j-1], pids[j]
		}
	}
	return pids
}
