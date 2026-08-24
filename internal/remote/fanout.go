package remote

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"sync"
)

// Streamer encodes the mastered PCM stream to MP3 once (one long-lived
// ffmpeg process) and fans the result out to any number of HTTP clients.
// A rolling pre-buffer of recent MP3 bytes is replayed to new clients so
// playback starts immediately and joins land on an MP3 frame boundary.
type Streamer struct {
	log *slog.Logger

	feed chan []byte // PCM chunks awaiting encode

	mu      sync.Mutex
	clients map[int]*streamClient
	nextID  int
	pre     []byte // rolling recent MP3 bytes
	closed  bool
}

// streamClient is one subscribed listener.
type streamClient struct {
	ch chan []byte
	// resyncs counts how often the client fell behind and had queued
	// audio dropped to catch it back up to live.
	resyncs int
}

// Sizing: at ~192 kbps MP3 is ~24 KB/s.
const (
	preBufferBytes  = 96 * 1024 // ~4s replayed to new clients
	clientChanSlots = 256       // per-client queue of ~4KB chunks (~40s)
	feedSlots       = 32        // PCM chunks (100ms each) awaiting encode
)

// NewStreamer returns an unstarted streamer.
func NewStreamer(log *slog.Logger) *Streamer {
	return &Streamer{
		log:     log,
		feed:    make(chan []byte, feedSlots),
		clients: map[int]*streamClient{},
	}
}

// Write accepts a mastered PCM chunk. It never blocks: when the encoder
// falls behind, chunks are dropped (the live stream stays live).
func (s *Streamer) Write(p []byte) (int, error) {
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case s.feed <- cp:
	default:
	}
	return len(p), nil
}

// Start launches the encoder and its pumps. It returns after starting
// goroutines; the streamer runs until ctx ends.
func (s *Streamer) Start(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		// The input is fully specified raw PCM: skip stream probing,
		// which would otherwise buffer several seconds before encoding.
		"-probesize", "32", "-analyzeduration", "0",
		"-f", "s16le", "-ar", "48000", "-ac", "2", "-i", "-",
		"-f", "mp3", "-codec:a", "libmp3lame", "-b:a", "192k",
		// Live stream: flush every packet instead of buffering ~32KB.
		"-flush_packets", "1",
		"-",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go s.pumpIn(ctx, stdin)
	go s.pumpOut(stdout)
	go func() {
		cmd.Wait()
		s.mu.Lock()
		s.closed = true
		for id, c := range s.clients {
			close(c.ch)
			delete(s.clients, id)
		}
		s.mu.Unlock()
		if ctx.Err() == nil {
			s.log.Warn("stream encoder exited", "event", "remote_encoder_exit")
		}
	}()
	return nil
}

// pumpIn feeds queued PCM into the encoder.
func (s *Streamer) pumpIn(ctx context.Context, stdin io.WriteCloser) {
	defer stdin.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case chunk := <-s.feed:
			if _, err := stdin.Write(chunk); err != nil {
				return
			}
		}
	}
}

// pumpOut reads encoded MP3 and broadcasts it.
func (s *Streamer) pumpOut(stdout io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.broadcast(chunk)
		}
		if err != nil {
			return
		}
	}
}

// broadcast appends to the pre-buffer and delivers to every client. A
// client whose queue is full has its oldest queued chunks dropped and
// stays subscribed: it resyncs closer to live instead of being
// disconnected, so a weak connection degrades to skipped audio rather
// than a dead stream.
func (s *Streamer) broadcast(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pre = append(s.pre, chunk...)
	if over := len(s.pre) - preBufferBytes; over > 0 {
		s.pre = append([]byte(nil), s.pre[over:]...)
	}
	for id, c := range s.clients {
		select {
		case c.ch <- chunk:
			continue
		default:
		}
		// Full queue: drain the oldest half. broadcast is the only
		// sender, so after draining there is always room again (the
		// reader can only shrink the queue further).
		dropped := 0
		for len(c.ch) > clientChanSlots/2 {
			select {
			case <-c.ch:
				dropped++
			default:
			}
		}
		select {
		case c.ch <- chunk:
		default:
		}
		c.resyncs++
		if c.resyncs == 1 || c.resyncs%20 == 0 {
			s.log.Info("slow stream client resynced to live", "event", "remote_client_resync",
				"id", id, "dropped_chunks", dropped, "resyncs", c.resyncs)
		}
	}
}

// Subscribe registers a client. It returns the replayed pre-buffer
// (aligned to an MP3 frame), the live channel, and an unsubscribe func.
func (s *Streamer) Subscribe() (pre []byte, ch <-chan []byte, cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	c := make(chan []byte, clientChanSlots)
	if s.closed {
		close(c)
		return nil, c, func() {}
	}
	s.clients[id] = &streamClient{ch: c}
	pre = alignToFrame(s.pre)
	cancel = func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if cc, ok := s.clients[id]; ok {
			close(cc.ch)
			delete(s.clients, id)
		}
	}
	return pre, c, cancel
}

// Listeners reports the connected client count.
func (s *Streamer) Listeners() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

// alignToFrame returns b starting at the first MP3 frame sync so a
// mid-stream join begins on a clean frame.
func alignToFrame(b []byte) []byte {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == 0xFF && b[i+1]&0xE0 == 0xE0 {
			out := make([]byte, len(b)-i)
			copy(out, b[i:])
			return out
		}
	}
	return nil
}
