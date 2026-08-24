package prompting

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"iar/internal/engine"
	"iar/internal/session"
)

// Builder turns a session's steering context into generation specs.
//
// The deterministic path is always used immediately and is complete on
// its own; an optional Ollama helper writes lyrics and proposes
// structured spec refinements strictly in the background, and its
// results are used only when they are already available by the time a
// spec is needed. No generation ever waits for the helper.
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

// BuildSpec produces the spec for the session's next track from the
// structured steering state. This is the single choke point between
// steering and the engine; it never blocks on the helper model.
func (b *Builder) BuildSpec(ctx context.Context, s *session.Session, seconds int) engine.Spec {
	_ = ctx
	r := Render(s)
	spec := engine.Spec{
		Seconds:        seconds,
		Seed:           -1,
		Prompt:         r.Caption,
		BPM:            r.BPM,
		KeyScale:       r.KeyScale,
		TimeSignature:  r.TimeSignature,
		VocalLanguage:  r.VocalLanguage,
		NegativePrompt: r.NegativePrompt,
		LMCfgScale:     r.LMCfgScale,
	}
	if !s.Vocal {
		spec.Lyrics = engine.InstrumentalLyrics
		return spec
	}
	if lyrics := b.buildLyrics(s, r.Caption); lyrics != "" {
		spec.Lyrics = lyrics
		return spec
	}
	// No lyrics ready: the engine's own planner invents caption and
	// lyrics from the rendered description (sample mode). The
	// structured constraints above still apply as user metadata, so
	// steering reaches vocal tracks too.
	query := r.Caption + ", with sung vocals"
	if s.LyricsTheme != "" {
		query += " about " + s.LyricsTheme
	}
	spec.Prompt = ""
	spec.SampleQuery = query
	return spec
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

// SpecUpdate is a helper-proposed structured update to the steering
// spec, produced as schema-constrained JSON so it can be validated and
// merged mechanically.
type SpecUpdate struct {
	Genre         []string       `json:"genre"`
	Instruments   map[string]int `json:"instruments"`
	Mood          []string       `json:"mood"`
	BPM           int            `json:"bpm"`
	KeyScale      string         `json:"key_scale"`
	TimeSignature string         `json:"time_signature"`
	VocalLanguage string         `json:"vocal_language"`
	Production    []string       `json:"production"`
	Negatives     []string       `json:"negatives"`
}

// specUpdateSchema constrains the helper model's JSON output.
var specUpdateSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"genre":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"instruments":    map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "integer"}},
		"mood":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"bpm":            map[string]any{"type": "integer"},
		"key_scale":      map[string]any{"type": "string"},
		"time_signature": map[string]any{"type": "string"},
		"vocal_language": map[string]any{"type": "string"},
		"production":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"negatives":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
}

const refineSystem = `You refine steering for a music-generation model.
Given the current structured description and one user adjustment, output a
JSON update: only fields the adjustment affects, everything else empty.
Weights are 1-3. "negatives" lists what the music must AVOID. Never put a
negated thing in a positive field. bpm 0 means unchanged.`

const expandSystem = `You expand a short music description into structured
fields for a music-generation model: genre (1-3 tags), instruments with
weight 1, mood words, production words, bpm (or 0), key_scale like
"C major" (or empty). Leave "negatives" empty unless the description
excludes something. Output JSON only.`

// RefineAsync asks the helper model to interpret one steering input as
// a structured update, delivering the validated result to apply when it
// is ready. Never blocks; does nothing when the helper is unusable.
func (b *Builder) RefineAsync(snap *session.Session, raw string, apply func(SpecUpdate)) {
	if !b.helperUsable() {
		return
	}
	specJSON, _ := json.Marshal(snap.Spec)
	user := "Base description: " + snap.BasePrompt +
		"\nCurrent spec: " + string(specJSON) +
		"\nUser adjustment: " + raw
	b.updateAsync("r|"+raw+"|"+string(specJSON), refineSystem, user, apply)
}

// ExpandAsync asks the helper model to enrich a vague seed prompt into
// structured fields (background, best-effort).
func (b *Builder) ExpandAsync(snap *session.Session, apply func(SpecUpdate)) {
	if !b.helperUsable() {
		return
	}
	b.updateAsync("e|"+snap.BasePrompt, expandSystem, "Music description: "+snap.BasePrompt, apply)
}

// updateAsync runs one schema-constrained helper call in the background.
func (b *Builder) updateAsync(key, system, user string, apply func(SpecUpdate)) {
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
		out, err := b.ollama.ChatJSON(ctx, system, user, specUpdateSchema)
		b.mu.Lock()
		delete(b.pending, key)
		b.mu.Unlock()
		if err != nil {
			b.noteFailure(err)
			return
		}
		var u SpecUpdate
		if err := json.Unmarshal([]byte(out), &u); err != nil {
			b.noteFailure(err)
			return
		}
		b.noteSuccess()
		sanitizeUpdate(&u)
		b.log.Info("helper spec update ready", "event", "helper_spec_update", "kind", key[:1])
		apply(u)
	}()
}

// validLanguages are the vocal language codes the engine accepts.
var validLanguages = map[string]bool{}

func init() {
	for _, code := range []string{
		"ar", "az", "bg", "bn", "ca", "cs", "da", "de", "el", "en",
		"es", "fa", "fi", "fr", "he", "hi", "hr", "ht", "hu", "id",
		"is", "it", "ja", "ko", "la", "lt", "ms", "ne", "nl", "no",
		"pa", "pl", "pt", "ro", "ru", "sa", "sk", "sr", "sv", "sw",
		"ta", "te", "th", "tl", "tr", "uk", "ur", "vi", "yue", "zh",
	} {
		validLanguages[code] = true
	}
}

var keyScaleRe = regexp.MustCompile(`^[A-Ga-g][#b]? (major|minor)$`)

// sanitizeUpdate clamps and validates everything a helper model
// proposed before it can touch the session.
func sanitizeUpdate(u *SpecUpdate) {
	clampList := func(list []string) []string {
		var out []string
		for _, v := range list {
			v = sanitizeLine(strings.ToLower(v), 40)
			if v != "" && len(out) < 8 {
				out = append(out, v)
			}
		}
		return out
	}
	u.Genre = clampList(u.Genre)
	u.Mood = clampList(u.Mood)
	u.Production = clampList(u.Production)
	u.Negatives = clampList(u.Negatives)
	clean := map[string]int{}
	for k, w := range u.Instruments {
		k = sanitizeLine(strings.ToLower(k), 40)
		if k == "" || len(clean) >= 8 {
			continue
		}
		switch {
		case w <= 0:
			// A zero-or-negative weight is a negation.
			if len(u.Negatives) < 8 {
				u.Negatives = append(u.Negatives, k)
			}
		case w > 3:
			clean[k] = 3
		default:
			clean[k] = w
		}
	}
	u.Instruments = clean
	if u.BPM != 0 {
		u.BPM = clampBPM(u.BPM)
	}
	u.KeyScale = strings.TrimSpace(u.KeyScale)
	if u.KeyScale != "" {
		if keyScaleRe.MatchString(u.KeyScale) {
			u.KeyScale = strings.ToUpper(u.KeyScale[:1]) + u.KeyScale[1:]
		} else {
			u.KeyScale = ""
		}
	}
	switch u.TimeSignature {
	case "2", "3", "4", "6":
	default:
		u.TimeSignature = ""
	}
	if !validLanguages[strings.ToLower(u.VocalLanguage)] {
		u.VocalLanguage = ""
	} else {
		u.VocalLanguage = strings.ToLower(u.VocalLanguage)
	}
}

// MergeUpdate folds a sanitized helper update into the session's spec,
// reporting whether anything changed. Values the user set explicitly
// (bpm, key, time signature, language) are never overridden.
func MergeUpdate(s *session.Session, u SpecUpdate) bool {
	EnsureSpec(s)
	spec := s.Spec
	before := spec.Clone()
	for _, g := range u.Genre {
		if !negated(spec, g) && len(spec.Genre) < 8 {
			addWord(&spec.Genre, g)
		}
	}
	for k, w := range u.Instruments {
		if negated(spec, k) {
			continue
		}
		if spec.Instruments == nil {
			spec.Instruments = map[string]int{}
		}
		if w > spec.Instruments[k] {
			spec.Instruments[k] = w
		}
	}
	for _, m := range u.Mood {
		if !negated(spec, m) && len(spec.Mood) < 8 {
			addWord(&spec.Mood, m)
		}
	}
	for _, p := range u.Production {
		if !negated(spec, p) && len(spec.Production) < 8 {
			addWord(&spec.Production, p)
		}
	}
	for _, n := range u.Negatives {
		removeMatching(spec, n)
		addNegative(spec, n)
	}
	if spec.BPM == 0 {
		spec.BPM = u.BPM
	}
	if spec.KeyScale == "" {
		spec.KeyScale = u.KeyScale
	}
	if spec.TimeSignature == "" {
		spec.TimeSignature = u.TimeSignature
	}
	if spec.VocalLanguage == "" {
		spec.VocalLanguage = u.VocalLanguage
	}
	return !before.Equal(spec)
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
