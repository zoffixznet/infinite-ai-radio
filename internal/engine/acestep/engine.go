package acestep

import (
	"context"
	"fmt"

	"iar/internal/engine"
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

// Plan implements the planning half of phased generation: the planner
// writes the song (metadata, words, audio codes) and no audio exists
// yet. Plans always think - the planner LM is the whole job - and with
// the dit-from-disk patch active a plan job clears the diffusion model
// off the card first, so a batch of plans runs with the card nearly
// empty.
func (e *Engine) Plan(ctx context.Context, spec engine.Spec) (*engine.Plan, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine not ready")
	}
	req := e.request(spec)
	req.Thinking = true
	res, err := e.be.Client().Plan(ctx, req)
	if err != nil {
		return nil, err
	}
	return &engine.Plan{
		Spec:          spec,
		Caption:       res.Caption,
		Lyrics:        res.Lyrics,
		AudioCodes:    res.AudioCodes,
		Seconds:       res.Seconds,
		BPM:           res.BPM,
		KeyScale:      res.KeyScale,
		TimeSignature: res.TimeSignature,
	}, nil
}

// Render turns a plan into audio. Every conditioning value the
// diffusion model needs travels in the request, and thinking is off,
// so the planner LM is never touched: only the diffusion model mounts.
func (e *Engine) Render(ctx context.Context, plan *engine.Plan) (*engine.Track, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine not ready")
	}
	req := GenerateRequest{
		AudioFormat:     "wav",
		AudioCodeString: plan.AudioCodes,
		Thinking:        false,
		Prompt:          plan.Caption,
		Lyrics:          plan.Lyrics,
		AudioDuration:   plan.Seconds,
		BPM:             plan.BPM,
		KeyScale:        plan.KeyScale,
		TimeSignature:   plan.TimeSignature,
		VocalLanguage:   plan.Spec.VocalLanguage,
		InferenceSteps:  e.opts.InferenceSteps,
		BatchSize:       1,
		UseRandomSeed:   plan.Spec.Seed < 0,
		Seed:            plan.Spec.Seed,
	}
	res, err := e.be.Client().Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	e.noteGenerationOK()
	return &engine.Track{
		Samples: res.Samples,
		Spec:    plan.Spec,
		Prompt:  plan.Caption,
		Lyrics:  plan.Lyrics,
		Seed:    res.Seed,
		GenTime: res.Elapsed,
	}, nil
}

// request builds the shared request shape for a spec (used by both the
// fused Generate path and the planning half of phased generation).
func (e *Engine) request(spec engine.Spec) GenerateRequest {
	req := GenerateRequest{
		AudioFormat:    "wav",
		AudioDuration:  float64(spec.Seconds),
		InferenceSteps: e.opts.InferenceSteps,
		Thinking:       e.opts.Thinking,
		BatchSize:      1,
		UseRandomSeed:  spec.Seed < 0,
		Seed:           spec.Seed,
		BPM:            spec.BPM,
		KeyScale:       spec.KeyScale,
		TimeSignature:  spec.TimeSignature,
		VocalLanguage:  spec.VocalLanguage,
	}
	if spec.SampleQuery != "" {
		req.SampleMode = true
		req.SampleQuery = spec.SampleQuery
		// Sample mode needs the planner LM to invent caption and lyrics;
		// the structured constraints above still apply as user metadata.
		req.Thinking = true
	} else {
		req.Prompt = spec.Prompt
		req.Lyrics = spec.Lyrics
		if spec.Vocal() {
			// A track with written lyrics is a complete song; its
			// length follows from the words, the way songs actually
			// get made. Omitting the duration lets the engine's
			// planner read the lyrics and derive a fitting length
			// (bounded by the max_track_seconds ceiling), instead of
			// squeezing or padding the song to track_seconds.
			// Instrumentals keep the exact requested length - they
			// are background music cut to a schedule, not songs.
			req.AudioDuration = 0
		}
	}
	// Negative conditioning only works through the planner LM; without
	// thinking there is no negative lever, so the fields are dropped.
	if spec.NegativePrompt != "" && req.Thinking {
		req.LMNegativePrompt = spec.NegativePrompt
		req.LMCfgScale = spec.LMCfgScale
	}
	return req
}

// Generate implements engine.Engine (the fused single-job path).
func (e *Engine) Generate(ctx context.Context, spec engine.Spec) (*engine.Track, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine not ready")
	}
	res, err := e.be.Client().Generate(ctx, e.request(spec))
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
