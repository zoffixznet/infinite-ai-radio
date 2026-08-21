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

// Engine adapts the ACE-Step client and sidecar to the engine.Engine
// interface.
type Engine struct {
	client  *Client
	sidecar *Sidecar
	opts    Options
}

// NewEngine wires a client and its supervised sidecar into an Engine.
func NewEngine(client *Client, sidecar *Sidecar, opts Options) *Engine {
	if opts.InferenceSteps <= 0 {
		opts.InferenceSteps = 8
	}
	return &Engine{client: client, sidecar: sidecar, opts: opts}
}

// Name implements engine.Engine.
func (e *Engine) Name() string { return "acestep" }

// Ready implements engine.Engine.
func (e *Engine) Ready() bool { return e.sidecar.Ready() }

// Sidecar exposes the supervisor for status displays and diagnostics.
func (e *Engine) Sidecar() *Sidecar { return e.sidecar }

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
	res, err := e.client.Generate(ctx, req)
	if err != nil {
		return nil, err
	}
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
