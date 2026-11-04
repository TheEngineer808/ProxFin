package streambuf

import (
	"sync"
	"sync/atomic"
)

// Chunk memory is pooled. With one fresh allocation per chunk, the heap
// grows to twice the buffered data before the garbage collector runs, and
// every chunk lands on new memory that has to be zeroed and faulted in;
// that cost dominated the CPU use of the buffer. Pooled buffers stay hot.
//
// Buffers come in size classes 2, 3, 4, 6, 8, 12 ... 128 KB (each 1.33 or
// 1.5 times the one before), so a small read (an audio frame) does not
// occupy a large buffer and no buffer is more than half empty.

var classSizes = func() []int {
	var sizes []int
	for size := 2 << 10; size <= 128<<10; size *= 2 {
		sizes = append(sizes, size)
		if size < 128<<10 {
			sizes = append(sizes, size*3/2)
		}
	}
	return sizes
}()

var pools = make([]sync.Pool, len(classSizes))

// poisonReleased fills released buffers with 0xEE (tests: any use after
// release then shows up as corrupted packets).
var poisonReleased bool

func classOf(n int) int {
	for c, size := range classSizes {
		if n <= size {
			return c
		}
	}
	return -1
}

// newChunkData returns a pooled copy of data, the pool buffer that holds
// it and the buffer's size class (nil and -1 if data is too large to
// pool). The buffer goes back to the pool with releaseChunkData; keeping
// the pointer the pool handed out means putting it back allocates nothing.
func newChunkData(data []byte) ([]byte, *[]byte, int) {
	class := classOf(len(data))
	if class < 0 {
		return append([]byte(nil), data...), nil, -1
	}
	buf, _ := pools[class].Get().(*[]byte)
	if buf == nil {
		b := make([]byte, classSizes[class])
		buf = &b
	}
	out := (*buf)[:len(data)]
	copy(out, data)
	return out, buf, class
}

func releaseChunkData(buf *[]byte, class int) {
	if buf == nil {
		return
	}
	if poisonReleased {
		for i := range *buf {
			(*buf)[i] = 0xEE
		}
	}
	pools[class].Put(buf)
}

// refs counts the holders of a chunk: the ring while the chunk is in it,
// and each viewer whose current batch contains it.
type refs struct {
	n atomic.Int32
}

func (c *chunk) retain() { c.refs.n.Add(1) }

func (c *chunk) release() {
	switch n := c.refs.n.Add(-1); {
	case n == 0:
		releaseChunkData(c.buf, c.class)
		c.data, c.buf = nil, nil
	case n < 0:
		panic("streambuf: chunk released more often than retained")
	}
}
