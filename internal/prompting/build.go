package prompting

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"bgm/internal/engine"
	"bgm/internal/session"
)

// Builder turns a session's steering context into generation specs.
//
// The deterministic path is always used immediately; an optional Ollama
// helper polishes prompts and writes lyrics strictly in the background,
// and its results are used only when they are already available by the
// time a spec is needed. No generation ever waits for the helper.
type Builder struct {
	ollama *Ollama
	log    *slog.Logger

	mu       sync.Mutex
	cache    map[string]string
	pending  map[string]bool
	usable   bool // set after a successful background probe
	failures int  // consecutive helper failures
	disabled bool // set after too many failures
	runCtx   context.Context
}

// maxOllamaFailures is how many consecutive failed helper calls disable
// the integration for the rest of the run.
const maxOllamaFailures = 2

// chatTimeout bounds each background helper call.
const chatTimeout = 30 * time.Second

// probeTimeout bounds the initial usability probe: a real chat round-trip
// with the target model, not just a daemon ping.
const probeTimeout = 25 * time.Second

// NewBuilder returns a Builder. ollama may be nil to disable the helper.
func NewBuilder(ollama *Ollama, log *slog.Logger) *Builder {
	if log == nil {
		log = slog.Default()
	}
	return &Builder{
		ollama:  ollama,
		log:     log,
		cache:   map[string]string{},
		pending: map[string]bool{},
		runCtx:  context.Background(),
	}
}

// ProbeAsync verifies in the background that the helper model actually
// answers a chat within a reasonable time. Until it succeeds, the helper
// is not consulted at all. Never blocks.
func (b *Builder) ProbeAsync(ctx context.Context) {
	b.mu.Lock()
	b.runCtx = ctx
	b.mu.Unlock()
	if b.ollama == nil {
		return
	}
	go func() {
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		if !b.ollama.Available(probeCtx) {
			b.log.Info("helper model daemon not reachable, using deterministic prompts", "event", "ollama_unavailable")
			return
		}
		start := time.Now()
		_, err := b.ollama.Chat(probeCtx, "You are a health check.", "Reply with exactly: OK")
		if err != nil {
			b.log.Info("helper model too slow or failing, using deterministic prompts",
				"event", "ollama_unusable", "error", err.Error(), "waited_seconds", time.Since(start).Seconds())
			return
		}
		b.mu.Lock()
		b.usable = true
		b.mu.Unlock()
		b.log.Info("helper model usable", "event", "ollama_usable",
			"model", b.ollama.Model(), "roundtrip_seconds", time.Since(start).Seconds())
	}()
}

// BuildSpec produces the spec for the session's next track. It never
// blocks on the helper model.
func (b *Builder) BuildSpec(ctx context.Context, s *session.Session, seconds int) engine.Spec {
	spec := engine.Spec{Seconds: seconds, Seed: -1}
	spec.Prompt = b.buildPrompt(s)
	if !s.Vocal {
		spec.Lyrics = engine.InstrumentalLyrics
		return spec
	}
	if lyrics := b.buildLyrics(s, spec.Prompt); lyrics != "" {
		spec.Lyrics = lyrics
		return spec
	}
	// No lyrics ready: the engine's own planner invents caption and
	// lyrics from a description (sample mode). Vocals stay local.
	query := spec.Prompt + ", with sung vocals"
	if s.LyricsTheme != "" {
		query += " about " + s.LyricsTheme
	}
	spec.Prompt = ""
	spec.SampleQuery = query
	return spec
}

// buildPrompt merges base prompt and tweaks deterministically, upgrading
// to an already-finished helper rewrite when one exists.
func (b *Builder) buildPrompt(s *session.Session) string {
	merged := mergePrompt(s)
	if len(s.Tweaks) == 0 || !b.helperUsable() {
		return merged
	}
	key := "p|" + merged
	if v, ok := b.lookup(key); ok {
		return v
	}
	// Snapshot now: the caller may keep mutating the session while the
	// background rewrite runs.
	snap := s.Snapshot()
	b.fillAsync(key, func(ctx context.Context) (string, error) {
		return b.rewrite(ctx, snap)
	})
	return merged
}

// buildLyrics returns helper-written lyrics when they are ready, kicking
// off a background write otherwise.
func (b *Builder) buildLyrics(s *session.Session, prompt string) string {
	if !b.helperUsable() {
		return ""
	}
	theme := s.LyricsTheme
	if theme == "" {
		theme = "matching the mood of the music"
	}
	key := "l|" + prompt + "|" + theme
	if v, ok := b.lookup(key); ok {
		return v
	}
	b.fillAsync(key, func(ctx context.Context) (string, error) {
		user := "Music style: " + prompt + "\nLyrics theme: " + theme
		return b.ollama.Chat(ctx, lyricsSystem, user)
	})
	return ""
}

// fillAsync runs one helper call in the background and caches its result.
// At most one call per key is in flight.
func (b *Builder) fillAsync(key string, fn func(ctx context.Context) (string, error)) {
	b.mu.Lock()
	if b.pending[key] {
		b.mu.Unlock()
		return
	}
	b.pending[key] = true
	runCtx := b.runCtx
	b.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(runCtx, chatTimeout)
		defer cancel()
		out, err := fn(ctx)
		b.mu.Lock()
		delete(b.pending, key)
		b.mu.Unlock()
		if err != nil {
			b.noteFailure(err)
			return
		}
		b.noteSuccess()
		b.store(key, out)
		b.log.Info("helper result ready", "event", "helper_ready", "key_kind", key[:1])
	}()
}

// helperUsable reports whether the helper should be consulted.
func (b *Builder) helperUsable() bool {
	if b.ollama == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.usable && !b.disabled
}

// noteFailure counts a failed helper call and disables the helper after
// too many in a row.
func (b *Builder) noteFailure(err error) {
	b.mu.Lock()
	b.failures++
	disable := b.failures >= maxOllamaFailures && !b.disabled
	if disable {
		b.disabled = true
	}
	b.mu.Unlock()
	b.log.Debug("helper call failed", "event", "ollama_fallback", "error", err.Error())
	if disable {
		b.log.Info("helper model disabled after repeated failures", "event", "ollama_disabled")
	}
}

// noteSuccess resets the consecutive failure counter.
func (b *Builder) noteSuccess() {
	b.mu.Lock()
	b.failures = 0
	b.mu.Unlock()
}

func (b *Builder) lookup(key string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.cache[key]
	return v, ok
}

func (b *Builder) store(key, v string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.cache) > 64 {
		b.cache = map[string]string{}
	}
	b.cache[key] = v
}

// mergePrompt is the deterministic path: base prompt plus tweak phrases
// in order.
func mergePrompt(s *session.Session) string {
	parts := []string{s.BasePrompt}
	parts = append(parts, s.TweakPhrases()...)
	if s.Vocal {
		parts = append(parts, "with vocals")
	}
	return strings.Join(parts, ", ")
}

const rewriteSystem = `You rewrite prompts for a music-generation model.
The user gives a base description and a list of adjustments, newest last.
Produce ONE final prompt: short comma-separated tags and phrases (genre,
mood, tempo, instrumentation), at most 35 words. Later adjustments override
earlier ones and the base when they conflict. Output ONLY the prompt text,
no quotes, no explanations.`

func (b *Builder) rewrite(ctx context.Context, s *session.Session) (string, error) {
	var sb strings.Builder
	sb.WriteString("Base: " + s.BasePrompt + "\nAdjustments:\n")
	for _, t := range s.Tweaks {
		sb.WriteString("- " + t.Raw + "\n")
	}
	if s.Vocal {
		sb.WriteString("The track will have sung vocals.\n")
	} else {
		sb.WriteString("The track is instrumental.\n")
	}
	out, err := b.ollama.Chat(ctx, rewriteSystem, sb.String())
	if err != nil {
		return "", err
	}
	return sanitizeLine(out, 350), nil
}

const lyricsSystem = `You write short song lyrics for an AI music model.
Write 8-14 short lines: a verse and a chorus. Mark sections with [verse]
and [chorus] on their own lines. Simple, singable, English. Output ONLY the
lyrics, no title, no explanations.`

// sanitizeLine flattens an LLM reply to one trimmed line of at most max
// bytes.
func sanitizeLine(s string, max int) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max]
	}
	return s
}
