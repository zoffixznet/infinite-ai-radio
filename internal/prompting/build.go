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

// Builder turns a session's steering context into generation specs. With an
// Ollama client it polishes prompts and writes lyrics; without one it falls
// back to deterministic merging that always works.
type Builder struct {
	ollama *Ollama
	log    *slog.Logger

	mu       sync.Mutex
	cache    map[string]cached
	failures int  // consecutive Ollama failures
	disabled bool // set after too many failures; deterministic from then on
}

// maxOllamaFailures is how many consecutive failed LLM calls disable the
// integration for the rest of the run. On machines where the helper model
// is slow to load (GPU busy with the music engine), repeated timeouts
// would otherwise delay every generation.
const maxOllamaFailures = 2

// chatTimeout bounds each helper-model call so a slow or stuck daemon can
// only briefly delay the next generation.
const chatTimeout = 30 * time.Second

// cached holds LLM output for one steering context so the model is only
// consulted when the context actually changes.
type cached struct {
	prompt string
	lyrics string
}

// NewBuilder returns a Builder. ollama may be nil to disable LLM
// assistance.
func NewBuilder(ollama *Ollama, log *slog.Logger) *Builder {
	if log == nil {
		log = slog.Default()
	}
	return &Builder{ollama: ollama, log: log, cache: map[string]cached{}}
}

// BuildSpec produces the spec for the session's next track.
func (b *Builder) BuildSpec(ctx context.Context, s *session.Session, seconds int) engine.Spec {
	spec := engine.Spec{
		Seconds: seconds,
		Seed:    -1,
	}
	spec.Prompt = b.buildPrompt(ctx, s)
	if !s.Vocal {
		spec.Lyrics = engine.InstrumentalLyrics
		return spec
	}
	if lyrics := b.buildLyrics(ctx, s, spec.Prompt); lyrics != "" {
		spec.Lyrics = lyrics
		return spec
	}
	// No local lyric writer: let the engine's own planner invent caption
	// and lyrics from a description (sample mode). Vocals stay local.
	query := spec.Prompt + ", with sung vocals"
	if s.LyricsTheme != "" {
		query += " about " + s.LyricsTheme
	}
	spec.Prompt = ""
	spec.SampleQuery = query
	return spec
}

// buildPrompt merges base prompt and tweaks, using Ollama when available.
func (b *Builder) buildPrompt(ctx context.Context, s *session.Session) string {
	merged := mergePrompt(s)
	if !b.ollamaUsable() || len(s.Tweaks) == 0 {
		return merged
	}
	key := "p|" + merged
	if c, ok := b.lookup(key); ok {
		return c.prompt
	}
	rewritten, err := b.rewrite(ctx, s)
	if err != nil {
		b.noteFailure(err)
		return merged
	}
	b.noteSuccess()
	b.store(key, cached{prompt: rewritten})
	b.log.Info("prompt rewritten", "event", "prompt_rewritten", "prompt", rewritten)
	return rewritten
}

// ollamaUsable reports whether the helper model should still be consulted.
func (b *Builder) ollamaUsable() bool {
	if b.ollama == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.disabled
}

// noteFailure counts a failed helper call and disables the integration
// after too many in a row.
func (b *Builder) noteFailure(err error) {
	b.mu.Lock()
	b.failures++
	disable := b.failures >= maxOllamaFailures && !b.disabled
	if disable {
		b.disabled = true
	}
	b.mu.Unlock()
	b.log.Debug("helper model call failed, using deterministic path", "event", "ollama_fallback", "error", err.Error())
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

// mergePrompt is the deterministic fallback: base prompt plus tweak
// phrases in order.
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
	cctx, cancel := context.WithTimeout(ctx, chatTimeout)
	defer cancel()
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
	out, err := b.ollama.Chat(cctx, rewriteSystem, sb.String())
	if err != nil {
		return "", err
	}
	return sanitizeLine(out, 350), nil
}

const lyricsSystem = `You write short song lyrics for an AI music model.
Write 8-14 short lines: a verse and a chorus. Mark sections with [verse]
and [chorus] on their own lines. Simple, singable, English. Output ONLY the
lyrics, no title, no explanations.`

// buildLyrics asks Ollama for lyrics; returns "" when unavailable, in
// which case the engine's own planner writes them.
func (b *Builder) buildLyrics(ctx context.Context, s *session.Session, prompt string) string {
	if !b.ollamaUsable() {
		return ""
	}
	theme := s.LyricsTheme
	if theme == "" {
		theme = "matching the mood of the music"
	}
	key := "l|" + prompt + "|" + theme
	if c, ok := b.lookup(key); ok {
		return c.lyrics
	}
	cctx, cancel := context.WithTimeout(ctx, chatTimeout)
	defer cancel()
	user := "Music style: " + prompt + "\nLyrics theme: " + theme
	out, err := b.ollama.Chat(cctx, lyricsSystem, user)
	if err != nil {
		b.noteFailure(err)
		return ""
	}
	b.noteSuccess()
	b.store(key, cached{lyrics: out})
	return out
}

func (b *Builder) lookup(key string) (cached, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.cache[key]
	return c, ok
}

func (b *Builder) store(key string, c cached) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.cache) > 64 {
		b.cache = map[string]cached{}
	}
	b.cache[key] = c
}

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
