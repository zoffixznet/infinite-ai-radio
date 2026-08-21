// Package enginetest provides a fast in-memory engine for tests.
package enginetest

import (
	"context"
	"math"
	"sync"
	"time"

	"bgm/internal/audio"
	"bgm/internal/engine"
)

// Mock is an engine.Engine that synthesizes a sine tone instantly. It
// records every spec it was asked to generate.
type Mock struct {
	mu sync.Mutex
	// ReadyState controls Ready(); defaults to true.
	readyState bool
	// Delay is added to each generation to simulate slowness.
	Delay time.Duration
	// FailNext makes the next Generate call return an error.
	FailNext bool
	// Freq is the tone frequency (default 440 Hz).
	Freq float64

	specs []engine.Spec
}

// NewMock returns a ready mock engine.
func NewMock() *Mock { return &Mock{readyState: true, Freq: 440} }

// Name implements engine.Engine.
func (m *Mock) Name() string { return "mock" }

// Ready implements engine.Engine.
func (m *Mock) Ready() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readyState
}

// SetReady flips readiness.
func (m *Mock) SetReady(ready bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readyState = ready
}

// Specs returns a copy of all generated specs.
func (m *Mock) Specs() []engine.Spec {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]engine.Spec(nil), m.specs...)
}

// Generate implements engine.Engine.
func (m *Mock) Generate(ctx context.Context, spec engine.Spec) (*engine.Track, error) {
	m.mu.Lock()
	fail := m.FailNext
	m.FailNext = false
	delay := m.Delay
	freq := m.Freq
	m.specs = append(m.specs, spec)
	m.mu.Unlock()

	if delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	if fail {
		return nil, context.DeadlineExceeded
	}
	frames := spec.Seconds * audio.SampleRate
	samples := make([]int16, frames*audio.Channels)
	for f := 0; f < frames; f++ {
		v := int16(8000 * math.Sin(2*math.Pi*freq*float64(f)/audio.SampleRate))
		samples[f*audio.Channels] = v
		samples[f*audio.Channels+1] = v
	}
	lyrics := spec.Lyrics
	if spec.SampleQuery != "" {
		lyrics = "invented lyrics about " + spec.SampleQuery
	}
	return &engine.Track{
		Samples: samples,
		Spec:    spec,
		Prompt:  spec.Prompt,
		Lyrics:  lyrics,
		Seed:    "1",
	}, nil
}
