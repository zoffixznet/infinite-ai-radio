package audio

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestRingReadNeverBlocksAndPadsSilence(t *testing.T) {
	r := NewRing(64)
	buf := make([]byte, 16)
	for i := range buf {
		buf[i] = 0xAA
	}
	n, err := r.Read(buf)
	if err != nil || n != len(buf) {
		t.Fatalf("Read = %d, %v; want full silent read", n, err)
	}
	if !bytes.Equal(buf, make([]byte, 16)) {
		t.Fatalf("empty ring read returned non-silence: %v", buf)
	}
	// No underrun counted before the first write.
	if got := r.Underruns(); got != 0 {
		t.Fatalf("Underruns before start = %d; want 0", got)
	}
}

func TestRingRoundTripAndUnderrunCount(t *testing.T) {
	r := NewRing(64)
	data := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	if _, err := r.Write(data); err != nil {
		t.Fatal(err)
	}
	if got := r.Buffered(); got != len(data) {
		t.Fatalf("Buffered = %d; want %d", got, len(data))
	}
	out := make([]byte, 12)
	if _, err := r.Read(out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out[:8], data) {
		t.Fatalf("read data mismatch: %v", out)
	}
	if !bytes.Equal(out[8:], make([]byte, 4)) {
		t.Fatalf("short read not padded with silence: %v", out[8:])
	}
	if got := r.Underruns(); got != 1 {
		t.Fatalf("Underruns = %d; want 1", got)
	}
}

func TestRingWriteBlocksUntilRead(t *testing.T) {
	r := NewRing(FrameBytes) // tiny: one frame
	if _, err := r.Write(make([]byte, FrameBytes)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		r.Write(make([]byte, FrameBytes)) // must block until a read frees space
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("write to full ring did not block")
	case <-time.After(50 * time.Millisecond):
	}
	buf := make([]byte, FrameBytes)
	r.Read(buf)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("write did not unblock after read")
	}
}

func TestRingClose(t *testing.T) {
	r := NewRing(64)
	r.Write([]byte{1, 2, 3, 4})
	r.Close()
	if _, err := r.Write([]byte{9}); err != io.ErrClosedPipe {
		t.Fatalf("write after close = %v; want ErrClosedPipe", err)
	}
	buf := make([]byte, 4)
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("draining read after close: %v", err)
	}
	if _, err := r.Read(buf); err != io.EOF {
		t.Fatalf("read after drain = %v; want EOF", err)
	}
}
