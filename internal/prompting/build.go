package prompting

import (
	"context"
	"encoding/json"
	"fmt"
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

	mu      sync.Mutex
	cache   map[string]string
	pending map[string]bool
	// defaultGen names the lyric generator used when the session does
	// not pick one (set from configuration; empty falls back to the
	// built-in default).
	defaultGen string
	// languages is the configured vocal-language catalogue; each track
	// draws at random from the ones a session has switched on. An empty
	// catalogue leaves the language to the engine.
	languages []Language
	// lyrReady queues finished lyrics per steering context, consumed
	// one track at a time so consecutive tracks get fresh words.
	// lyrLast keeps the last lyrics actually used per context (reused
	// when no fresh write finished in time: stale beats
	// machine-invented). lyrHooks remembers recent hook lines per
	// context so follow-up writes avoid repeating a chorus.
	lyrReady map[string][]string
	lyrLast  map[string]string
	lyrHooks map[string][]string
	usable   bool // set after a successful background probe
	failures int  // consecutive helper failures
	// restAfter is when the helper may be consulted again. A run of
	// failures rests the helper rather than retiring it: the graphics
	// card is shared, and a model that timed out while something else
	// had the card is busy, not broken.
	restAfter time.Time
	runCtx    context.Context
}

// maxOllamaFailures is how many consecutive failed helper calls rest the
// helper.
const maxOllamaFailures = 2

// helperRest is how long the helper is left alone after a run of
// failures. Retiring it for the whole run instead means one busy minute
// costs a night of engine-invented lyrics, which is how a configured
// language quietly stops being sung.
const helperRest = 4 * time.Minute

// chatTimeout bounds each background helper call. It is far longer than
// the work needs because the helper degrades instead of failing: with
// the graphics card full, the daemon runs the model on the CPU, where a
// cold load plus a short reply is minutes, not seconds. A slow call is
// slow, not broken, and treating it as broken is what used to stand the
// lyric writer down for the rest of the evening.
const chatTimeout = 3 * time.Minute

// probeTimeout bounds one usability probe: a real chat round-trip with
// the target model, not just a daemon ping. It is generous because the
// first call also loads the model - minutes on the CPU when the
// graphics card has no room - and may be queued behind whatever else
// is using the model.
const probeTimeout = 3 * time.Minute

// probeRetry is the first wait before probing again, doubling up to
// probeRetryMax. A daemon that was cold or busy at startup is worth
// asking again; giving up once means no written lyrics all run.
const (
	probeRetry    = 2 * time.Minute
	probeRetryMax = 15 * time.Minute
)

// NewBuilder returns a Builder. ollama may be nil to disable the helper.
func NewBuilder(ollama *Ollama, log *slog.Logger) *Builder {
	if log == nil {
		log = slog.Default()
	}
	return &Builder{
		ollama:   ollama,
		log:      log,
		cache:    map[string]string{},
		pending:  map[string]bool{},
		lyrReady: map[string][]string{},
		lyrLast:  map[string]string{},
		lyrHooks: map[string][]string{},
		runCtx:   context.Background(),
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
		for wait := probeRetry; ; wait = min(2*wait, probeRetryMax) {
			if b.probeOnce(ctx) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()
}

// probeOnce runs one usability probe, reporting whether the helper
// answered.
func (b *Builder) probeOnce(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if !b.ollama.Available(probeCtx) {
		b.log.Info("helper model daemon not reachable, using deterministic prompts", "event", "ollama_unavailable")
		return false
	}
	start := time.Now()
	if _, err := b.ollama.Chat(probeCtx, "You are a health check.", "Reply with exactly: OK"); err != nil {
		b.log.Info("helper model too slow or failing, using deterministic prompts",
			"event", "ollama_unusable", "error", err.Error(), "waited_seconds", time.Since(start).Seconds())
		return false
	}
	b.mu.Lock()
	b.usable = true
	b.mu.Unlock()
	b.log.Info("helper model usable", "event", "ollama_usable",
		"model", b.ollama.Model(), "roundtrip_seconds", time.Since(start).Seconds())
	return true
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
	// A language steered in by hand ("sing in french") pins the session
	// to it; otherwise one of the switched-on languages is drawn at
	// random. A language a preset happens to carry is only a default:
	// it loses to a configured catalogue, or the list the listener is
	// editing would quietly do nothing. With neither, the engine sings
	// in whatever language it likes.
	lang := Language{Code: r.VocalLanguage, Name: LanguageName(r.VocalLanguage)}
	if !r.LanguagePinned {
		switch drawn, ok, configured := b.nextLanguage(s.Languages); {
		case ok:
			lang = drawn
		case configured:
			// A list exists and every language in it is switched off:
			// the listener asked for the engine's own choice, not for
			// whatever language a preset happened to carry.
			lang = Language{}
		}
	}
	spec.VocalLanguage = lang.Code
	spec.VocalLanguageName = lang.Name
	if lyrics := b.buildLyrics(s, r, lang, seconds); lyrics != "" {
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
	if lang.Name != "" {
		// The planner reads the query, not the request fields, so the
		// language has to be said in words as well.
		query += ", sung in " + lang.Name
	}
	spec.Prompt = ""
	spec.SampleQuery = query
	return spec
}

// lyricsReadyTarget is how many unused lyric sheets the builder keeps
// written ahead per steering context.
const lyricsReadyTarget = 1

// maxAvoidHooks caps the remembered hook lines per steering context.
const maxAvoidHooks = 4

// maxLyricsBytes caps a generator's output defensively.
const maxLyricsBytes = 4000

// SetLanguages installs the configured vocal-language catalogue. An
// empty list leaves every track's language to the music engine.
func (b *Builder) SetLanguages(names []string) {
	langs := ParseLanguages(names)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.languages = langs
}

// Languages reports the configured vocal-language catalogue.
func (b *Builder) Languages() []Language {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Language(nil), b.languages...)
}

// nextLanguage draws the language for the next track from the ones the
// session has switched on. configured reports that a catalogue exists at
// all: with one, the listener's switches are the whole story, and
// switching every language off means the engine chooses. Without one, a
// language the session carries from a preset stands.
func (b *Builder) nextLanguage(picked map[string]bool) (lang Language, ok, configured bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.languages) == 0 {
		return Language{}, false, false
	}
	l, ok := pickLanguage(EnabledLanguages(b.languages, picked))
	return l, ok, true
}

// SetDefaultGenerator picks the lyric generator used when the session
// does not name one. The caller validates the name.
func (b *Builder) SetDefaultGenerator(name string) {
	b.mu.Lock()
	b.defaultGen = strings.ToLower(strings.TrimSpace(name))
	b.mu.Unlock()
}

// generatorFor resolves the session's effective lyric generator:
// session choice, then configured default, then the built-in default.
func (b *Builder) generatorFor(s *session.Session) LyricsGenerator {
	b.mu.Lock()
	def := b.defaultGen
	b.mu.Unlock()
	for _, name := range []string{s.LyricsGenerator, def, DefaultGeneratorName} {
		if g, ok := GeneratorByName(name); ok {
			return g
		}
	}
	return registry[0]
}

// GeneratorName reports the effective lyric generator for a session.
func (b *Builder) GeneratorName(s *session.Session) string {
	return b.generatorFor(s).Name()
}

// buildLyrics returns lyrics for the next track when any are ready and
// keeps background writes running so upcoming tracks get fresh words.
// It never blocks: the first vocal track in a new steering context
// falls back to the engine's own planner while the first write runs,
// and when a later write has not finished in time the previous lyrics
// are reused once more.
func (b *Builder) buildLyrics(s *session.Session, r Rendered, lang Language, seconds int) string {
	if !b.helperUsable() {
		return ""
	}
	gen := b.generatorFor(s)
	key := "l|" + gen.Name() + "|" + r.Caption + "|" + s.LyricsTheme + "|" + lang.Name + "|" + lang.Code
	b.mu.Lock()
	var out string
	if q := b.lyrReady[key]; len(q) > 0 {
		out = q[0]
		b.lyrReady[key] = q[1:]
		b.lyrLast[key] = out
		if h := hookLine(out); h != "" {
			hooks := append(b.lyrHooks[key], h)
			if len(hooks) > maxAvoidHooks {
				hooks = hooks[len(hooks)-maxAvoidHooks:]
			}
			b.lyrHooks[key] = hooks
		}
	} else {
		out = b.lyrLast[key]
	}
	needFill := len(b.lyrReady[key]) < lyricsReadyTarget
	req := LyricsRequest{
		Style:        r.Caption,
		Theme:        s.LyricsTheme,
		Seconds:      seconds,
		Language:     lang.Code,
		LanguageName: lang.Name,
		AvoidHooks:   append([]string(nil), b.lyrHooks[key]...),
	}
	b.mu.Unlock()
	if needFill {
		b.fillLyricsAsync(key, gen, req)
	}
	return out
}

// fillLyricsAsync runs one lyric generator call in the background and
// queues its result. At most one write per context is in flight.
func (b *Builder) fillLyricsAsync(key string, gen LyricsGenerator, req LyricsRequest) {
	b.mu.Lock()
	if b.pending[key] {
		b.mu.Unlock()
		return
	}
	b.pending[key] = true
	runCtx := b.runCtx
	b.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(runCtx, gen.Timeout())
		defer cancel()
		out, err := gen.Generate(ctx, b.ollama, req)
		out = strings.TrimSpace(out)
		if len(out) > maxLyricsBytes {
			out = out[:maxLyricsBytes]
		}
		// One critical section clears pending AND stores the result, so
		// no buildLyrics call can slip between them and start a
		// duplicate write for the same context.
		b.mu.Lock()
		delete(b.pending, key)
		if err == nil && out != "" {
			if len(b.lyrReady) > 64 {
				// The steering context changed many times; drop stale
				// queues wholesale, like the generic cache does.
				b.lyrReady = map[string][]string{}
				b.lyrLast = map[string]string{}
				b.lyrHooks = map[string][]string{}
			}
			b.lyrReady[key] = append(b.lyrReady[key], out)
		}
		b.mu.Unlock()
		if err == nil && out == "" {
			err = fmt.Errorf("%s wrote empty lyrics", gen.Name())
		}
		if err != nil {
			b.noteFailure(err)
			return
		}
		b.noteSuccess()
		lines, words := 0, 0
		for _, l := range strings.Split(out, "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "[") {
				continue
			}
			lines++
			words += len(strings.Fields(l))
		}
		b.log.Info("lyrics ready", "event", "lyrics_ready",
			"generator", gen.Name(), "sung_lines", lines, "words", words)
	}()
}

// hookLine extracts a song's hook: the first sung line of the first
// chorus, or the first sung line at all. The builder remembers it so
// consecutive tracks don't share a chorus.
func hookLine(lyrics string) string {
	var first string
	inChorus := false
	for _, line := range strings.Split(lyrics, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			inChorus = strings.Contains(strings.ToLower(line), "chorus")
			continue
		}
		if first == "" {
			first = line
		}
		if inChorus {
			return line
		}
	}
	return first
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
	return b.usable && time.Now().After(b.restAfter)
}

// noteFailure records a failed helper call and rests the helper after
// too many in a row.
func (b *Builder) noteFailure(err error) {
	b.log.Debug("helper call failed", "event", "ollama_fallback", "error", err.Error())
	b.mu.Lock()
	b.failures++
	rest := b.failures >= maxOllamaFailures && time.Now().After(b.restAfter)
	if rest {
		b.failures = 0
		b.restAfter = time.Now().Add(helperRest)
	}
	b.mu.Unlock()
	if rest {
		b.log.Info("helper model resting after repeated failures", "event", "ollama_resting",
			"minutes", helperRest.Minutes())
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

	Production []string `json:"production"`
	// Reduce lists things to dial DOWN but keep (the soft "less"
	// path); Negatives lists what the music must completely avoid.
	Reduce    []string `json:"reduce"`
	Negatives []string `json:"negatives"`
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
		"production":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"reduce":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"negatives":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
}

const refineSystem = `You refine steering for a music-generation model.
Given the current structured description and one user adjustment, output a
JSON update: only fields the adjustment affects, everything else empty.
Weights are 1-3. "reduce" lists things to dial DOWN but keep (use it for
"less"/"fewer" requests). "negatives" lists what the music must
completely AVOID (use it only for "no"/"without"/"remove" requests;
never for a mere "less"). Never put a negated thing in a positive
field. bpm 0 means unchanged.`

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
		out, err := b.ollama.ChatWith(ctx, system, user, ChatOpts{
			Format: specUpdateSchema, KeepAliveSeconds: scribeKeepAlive,
		})
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
	u.Reduce = clampList(u.Reduce)
	u.Negatives = clampList(u.Negatives)
	clean := map[string]int{}
	for k, w := range u.Instruments {
		k = sanitizeLine(strings.ToLower(k), 40)
		if k == "" || len(clean) >= 8 {
			continue
		}
		switch {
		case w <= 0:
			// A zero-or-negative weight dials the instrument down; only
			// an explicit negative eliminates it.
			if len(u.Reduce) < 8 {
				u.Reduce = append(u.Reduce, k)
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
	for _, n := range u.Reduce {
		applyLessen(spec, n)
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
	// The language is deliberately not merged: it is the one spec field
	// with a control of its own, and a background guess installing one
	// is invisible and outlives every list the listener edits.
	return !before.Equal(spec)
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
