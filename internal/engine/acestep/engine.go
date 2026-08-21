package acestep

import (
	"context"
	"fmt"

	"bgm/internal/engine"
)

// Options tunes generation requests.
type Options struct {
	// InferenceSteps for the turbo model (1-20).
	InferenceSteps int
	// Thinking enables the engine's planner LM.
	Thinking bool
}

// Backend is the connection to a running (or starting) ACE-Step API
// server: either an in-process supervised child (Sidecar) or a shared
// engine daemon from a previous run (Remote).
type Backend interface {
	// Ready reports whether the server answers with models loaded.
	Ready() bool
	// Client returns the REST client for the server's current address.
	Client() *Client
	// Phase names what the server is doing: "starting engine",
	// "loading models", "ready" or "unavailable".
	Phase() string
	// Tail returns recent server output lines for diagnostics.
	Tail() []string
}

// Engine adapts an ACE-Step backend to the engine.Engine interface.
type Engine struct {
	be   Backend
	opts Options
}

// NewEngine wraps a backend into an Engine.
func NewEngine(be Backend, opts Options) *Engine {
	if opts.InferenceSteps <= 0 {
		opts.InferenceSteps = 8
	}
	return &Engine{be: be, opts: opts}
}

// Name implements engine.Engine.
func (e *Engine) Name() string { return "acestep" }

// Ready implements engine.Engine.
func (e *Engine) Ready() bool { return e.be.Ready() }

// Phase reports the backend's startup phase for progress displays.
func (e *Engine) Phase() string { return e.be.Phase() }

// Backend exposes the underlying backend for diagnostics.
func (e *Engine) Backend() Backend { return e.be }

// Tail returns recent engine output lines for diagnostics.
func (e *Engine) Tail() []string { return e.be.Tail() }

// RestartEngine asks the backend to force-restart the engine process.
// It reports whether a restart was actually initiated.
func (e *Engine) RestartEngine(reason string) bool {
	if r, ok := e.be.(interface{ RestartEngine(string) bool }); ok {
		return r.RestartEngine(reason)
	}
	return false
}

// noteGenerationOK informs the backend that generation works again so
// forced-restart backoff can reset.
func (e *Engine) noteGenerationOK() {
	if n, ok := e.be.(interface{ NoteGenerationOK() }); ok {
		n.NoteGenerationOK()
	}
}

// Generate implements engine.Engine.
func (e *Engine) Generate(ctx context.Context, spec engine.Spec) (*engine.Track, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine not ready")
	}
	req := GenerateRequest{
		AudioFormat:    "wav",
		AudioDuration:  float64(spec.Seconds),
		InferenceSteps: e.opts.InferenceSteps,
		Thinking:       e.opts.Thinking,
		BatchSize:      1,
		UseRandomSeed:  spec.Seed < 0,
		Seed:           spec.Seed,
	}
	if spec.SampleQuery != "" {
		req.SampleMode = true
		req.SampleQuery = spec.SampleQuery
		// Sample mode needs the planner LM to invent caption and lyrics.
		req.Thinking = true
	} else {
		req.Prompt = spec.Prompt
		req.Lyrics = spec.Lyrics
	}
	res, err := e.be.Client().Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	e.noteGenerationOK()
	return &engine.Track{
		Samples: res.Samples,
		Spec:    spec,
		Prompt:  orDefault(res.Prompt, spec.Prompt),
		Lyrics:  orDefault(res.Lyrics, spec.Lyrics),
		Seed:    res.Seed,
		GenTime: res.Elapsed,
	}, nil
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
