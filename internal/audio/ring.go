package audio

import (
	"io"
	"sync"
)

// Ring is a byte ring buffer connecting the mixer (writer) to the playback
// backend (reader).
//
// Read never blocks: when the buffer holds less data than requested the
// remainder is filled with silence (zero bytes), so a stalled generator can
// never stutter or crash playback; silence is the floor. Write blocks until
// space is available, which is what paces the mixer.
type Ring struct {
	mu       sync.Mutex
	notFull  *sync.Cond
	buf      []byte
	r, w     int // read/write offsets
	n        int // bytes currently buffered
	closed   bool
	started  bool // set once the first byte has been written
	underrun int64
}

// NewRing returns a ring buffer holding up to capacity bytes, rounded up to
// whole frames.
func NewRing(capacity int) *Ring {
	if capacity < FrameBytes {
		capacity = FrameBytes
	}
	if rem := capacity % FrameBytes; rem != 0 {
		capacity += FrameBytes - rem
	}
	rb := &Ring{buf: make([]byte, capacity)}
	rb.notFull = sync.NewCond(&rb.mu)
	return rb
}

// Read fills p completely and never blocks. Missing data is zero-filled
// (silence). It returns io.EOF only once the ring has been closed and fully
// drained.
func (rb *Ring) Read(p []byte) (int, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	avail := rb.n
	if avail > len(p) {
		avail = len(p)
	}
	for i := 0; i < avail; i++ {
		p[i] = rb.buf[rb.r]
		rb.r = (rb.r + 1) % len(rb.buf)
	}
	rb.n -= avail
	if avail > 0 {
		rb.notFull.Broadcast()
	}
	if avail < len(p) {
		if rb.closed {
			if avail == 0 {
				return 0, io.EOF
			}
			// Pad the final short read with silence.
			zero(p[avail:])
			return len(p), nil
		}
		if rb.started {
			rb.underrun++
		}
		zero(p[avail:])
	}
	return len(p), nil
}

// Write stores p into the ring, blocking while the ring is full. It returns
// io.ErrClosedPipe after Close.
func (rb *Ring) Write(p []byte) (int, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	written := 0
	for written < len(p) {
		for rb.n == len(rb.buf) && !rb.closed {
			rb.notFull.Wait()
		}
		if rb.closed {
			return written, io.ErrClosedPipe
		}
		rb.started = true
		for rb.n < len(rb.buf) && written < len(p) {
			rb.buf[rb.w] = p[written]
			rb.w = (rb.w + 1) % len(rb.buf)
			rb.n++
			written++
		}
	}
	return written, nil
}

// Close marks the ring closed. Pending and future writes fail; reads drain
// the remaining data and then report io.EOF.
func (rb *Ring) Close() error {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.closed = true
	rb.notFull.Broadcast()
	return nil
}

// Buffered reports how many bytes are currently stored.
func (rb *Ring) Buffered() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.n
}

// Capacity reports the total byte capacity.
func (rb *Ring) Capacity() int { return len(rb.buf) }

// Underruns reports how many reads found an empty or short buffer after
// playback had started.
func (rb *Ring) Underruns() int64 {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.underrun
}

func zero(p []byte) {
	for i := range p {
		p[i] = 0
	}
}
