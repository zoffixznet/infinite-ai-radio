// Package tone is a music engine that needs nothing: no model, no
// graphics card. Every song it makes is a short chord of pure tones,
// picked from the description so the same request sounds the same
// twice, made instantly. It is what plays on a machine without a card,
// and what the end-to-end tests drive the whole radio with - the
// store, the ladder, the phone and the exports all run exactly as they
// do with the real engine, only the songs are simpler.
package tone

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"

	"iar/internal/audio"
	"iar/internal/engine"
)

// DefaultSeconds is how long a tone song runs: long enough to hear the
// radio move from one to the next, short enough that a store fills in
// moments.
const DefaultSeconds = 20

// Engine synthesizes tone songs.
type Engine struct {
	seconds int
}

// New returns an engine whose songs run for seconds (DefaultSeconds
// when zero or less).
func New(seconds int) *Engine {
	if seconds <= 0 {
		seconds = DefaultSeconds
	}
	return &Engine{seconds: seconds}
}

// Name implements engine.Engine.
func (e *Engine) Name() string { return "tone" }

// Ready implements engine.Engine: always.
func (e *Engine) Ready() bool { return true }

// Plan settles a song without rendering it: the description as its
// caption, the words as given (or the instrumental marker), and the
// chord it will be made of as its score.
func (e *Engine) Plan(ctx context.Context, spec engine.Spec) (*engine.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lyrics := spec.Lyrics
	if lyrics == "" {
		lyrics = engine.InstrumentalLyrics
	}
	caption := spec.Prompt
	if caption == "" {
		caption = spec.SampleQuery
	}
	return &engine.Plan{
		Spec:       spec,
		Caption:    caption,
		Lyrics:     lyrics,
		AudioCodes: fmt.Sprintf("tone:%d", chordOf(caption)),
		Seconds:    float64(e.seconds),
		BPM:        90,
		KeyScale:   "C major",
	}, nil
}

// Render turns a plan into audio.
func (e *Engine) Render(ctx context.Context, plan *engine.Plan) (*engine.Track, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seed := chordOf(plan.Caption)
	frames := e.seconds * audio.SampleRate
	samples := make([]int16, frames*audio.Channels)
	// Three partials in a just-intonation major triad on a root picked
	// from the description, each a little quieter than the last.
	root := 110.0 * math.Pow(2, float64(seed%24)/12)
	partials := []struct{ ratio, amp float64 }{{1, 0.55}, {1.25, 0.3}, {1.5, 0.2}}
	for f := 0; f < frames; f++ {
		t := float64(f) / audio.SampleRate
		v := 0.0
		for _, p := range partials {
			v += p.amp * math.Sin(2*math.Pi*root*p.ratio*t)
		}
		s := int16(6000 * v)
		samples[f*audio.Channels] = s
		samples[f*audio.Channels+1] = s
	}
	audio.ApplyEdgeFades(samples, audio.SampleRate/2)
	return &engine.Track{
		Samples: samples,
		Spec:    plan.Spec,
		Prompt:  plan.Caption,
		Lyrics:  plan.Lyrics,
		Seed:    fmt.Sprint(seed),
	}, nil
}

// Generate implements engine.Engine: a plan and its render in one go.
func (e *Engine) Generate(ctx context.Context, spec engine.Spec) (*engine.Track, error) {
	plan, err := e.Plan(ctx, spec)
	if err != nil {
		return nil, err
	}
	return e.Render(ctx, plan)
}

// chordOf hashes a description to the chord it is played as.
func chordOf(caption string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(caption))
	return h.Sum64()
}
