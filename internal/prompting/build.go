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
// its own; an optional Ollama helper writes lyrics, describes and names
// each song, and proposes structured spec refinements. Building a spec
// never waits for it: refinements land in the background and are used
// only when they are already there, and the words - with the
// description and the name written from them in the same round - come
// off a shelf stocked ahead, in the window where the helper has the
// graphics card to itself.
type Builder struct {
	ollama *Ollama
	log    *slog.Logger

	mu sync.Mutex
	// pending marks the lyric-context keys with a background write in
	// flight, so two callers never start the same one twice.
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
	// instReady holds per-song descriptions, and the names written from
	// them, for instrumental tracks,
	// keyed like the lyric shelf. An instrumental has no words to
	// describe it, so without these every instrumental of a session
	// reaches the engine under the identical terse tag list while
	// every vocal song arrives with a description of its own.
	instReady map[string][]StockedCaption
	lyrReady  map[string][]StockedLyrics
	lyrLast   map[string]StockedLyrics
	// phased records whether generation is phased: only then does a
	// busy engine suppress background lyric writing (see buildLyrics).
	phased bool
	// lyrLastUses counts how many songs the lyrLast sheet has been
	// given to, so reuse stays a hiccup-bridge and never a habit.
	lyrLastUses map[string]int
	lyrHooks    map[string][]string
	usable      bool // set after a successful background probe
	failures    int  // consecutive helper failures
	// restAfter is when the helper may be consulted again. A run of
	// failures rests the helper rather than retiring it: the graphics
	// card is shared, and a model that timed out while something else
	// had the card is busy, not broken. restEarnedIdle marks a rest
	// whose failures happened on a free card - that one is the model's
	// own fault and survives the card being handed back.
	restAfter      time.Time
	restEarnedIdle bool
	runCtx         context.Context
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
// slow, not broken; treating it as broken stands the lyric writer down
// for the rest of the run.
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
		ollama:      ollama,
		log:         log,
		pending:     map[string]bool{},
		instReady:   map[string][]StockedCaption{},
		lyrReady:    map[string][]StockedLyrics{},
		lyrLast:     map[string]StockedLyrics{},
		lyrLastUses: map[string]int{},
		lyrHooks:    map[string][]string{},
		runCtx:      context.Background(),
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
		if d := b.takeInstrumentalCaption(s, r); d.Text != "" {
			spec.Prompt = d.Text
			spec.Title, spec.Subtitle = d.Title, d.Subtitle
		}
		return spec
	}
	// A language steered in by hand ("sing in french") pins the session
	// to it; otherwise one of the switched-on languages is drawn at
	// random. A language a preset happens to carry is only a default:
	// it loses to a configured catalogue, or the list the listener is
	// editing would quietly do nothing. With neither, the engine sings
	// in whatever language it likes.
	if st, ok := b.buildLyrics(s, r); ok {
		spec.Lyrics = st.Text
		spec.VocalLanguage = st.Lang.Code
		spec.VocalLanguageName = st.Lang.Name
		spec.Title, spec.Subtitle = st.Title, st.Subtitle
		if st.Caption != "" {
			spec.Prompt = st.Caption
		}
		return spec
	}
	lang := b.chooseLanguage(s, r)
	spec.VocalLanguage = lang.Code
	spec.VocalLanguageName = lang.Name
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

// maxSheetReuse is how many extra songs the last consumed sheet may be
// given to when no fresh sheet is ready: each sheet is sung at most
// twice in total.
const maxSheetReuse = 1

// maxAvoidHooks caps the remembered hook lines per steering context
// and language. Sized for batch writing: a deep cycle pens dozens of
// songs of one context back to back, and each should steer clear of
// the recent choruses. Wider would cover even longer batches, but the
// whole list rides every lyric prompt, so this is a balance between
// repetition at the tail of a very deep batch and prompt bloat on
// every call; per-song captions and sampling temperature carry the
// rest of the variety.
const maxAvoidHooks = 24

// maxLyricsBytes caps a generator's output defensively.
const maxLyricsBytes = 4000

// captionSystem instructs the helper to describe one specific song for
// the music generator's conditioning. The description must stay inside
// the style the listener steered - it rephrases and enriches, never
// contradicts.
const captionSystem = `You describe songs for a music generator. Given
a STYLE tag list and the song's LYRICS, reply with one vivid sentence
(25-45 words) describing this specific track: its instruments, energy,
texture and mood, echoing the song's imagery. Stay strictly inside the
STYLE - every tag holds; add color, never contradictions. Plain prose,
no quotes, no artist names, no mention of prompts or AI.`

// instrumentalSystem instructs the helper to describe one specific
// instrumental piece. It has no lyrics to work from, so the brief is
// the arrangement itself: what the piece does over its length.
const instrumentalSystem = `You describe instrumental pieces for a music
generator. Given a STYLE tag list, reply with one vivid sentence (25-45
words) describing one specific instrumental track in that style: its
instruments, its groove, and how it moves from its opening through a
middle to its ending. No singing, no vocals, no lyrics - this piece has
none. Stay strictly inside the STYLE - every tag holds; add color,
never contradictions. Plain prose, no quotes, no artist names, no
mention of prompts or AI.`

// instrumentalKey names the shelf slot for a session's instrumental
// descriptions: the steering context they were written for.
func (b *Builder) instrumentalKey(r Rendered) string { return "inst:" + r.Caption }

// StockInstrumentalCaptions writes per-song descriptions ahead for an
// instrumental session, the way StockLyrics writes words for a vocal
// one, and returns how many it wrote. Synchronous: the caller owns the
// timing, and means to spend the freed graphics card on it.
func (b *Builder) StockInstrumentalCaptions(ctx context.Context, s *session.Session, want int, stop func(wrote int) bool) int {
	if s == nil || s.Vocal || !b.helperUsable() {
		return 0
	}
	r := Render(s)
	key := b.instrumentalKey(r)
	wrote := 0
	for ctx.Err() == nil && b.helperUsable() && (stop == nil || !stop(wrote)) {
		b.mu.Lock()
		have := len(b.instReady[key])
		b.mu.Unlock()
		if have >= want {
			break
		}
		callCtx, cancel := context.WithTimeout(ctx, captionTimeout)
		out, err := b.ollama.ChatWith(callCtx, instrumentalSystem, "STYLE: "+r.Caption, ChatOpts{
			Temperature: 0.9, KeepAliveSeconds: scribeKeepAlive,
		})
		cancel()
		line := sanitizeLine(out, 400)
		if err != nil || line == "" {
			b.noteFailure(fmt.Errorf("instrumental description: %w", err))
			return wrote
		}
		// Named from that description in the same breath, on the same
		// free card, for the same reason a song with words is: nothing
		// gets to name it afterwards.
		nameCtx, nameCancel := context.WithTimeout(ctx, songNameBudget)
		title, subtitle := b.titleInstrumental(nameCtx, line)
		nameCancel()
		b.mu.Lock()
		b.instReady[key] = append(b.instReady[key], StockedCaption{Text: line, Title: title, Subtitle: subtitle})
		b.mu.Unlock()
		wrote++
		b.log.Info("instrumental description stocked ahead",
			"event", "instrumental_stocked", "ready", have+1)
	}
	return wrote
}

// AwaitingInstrumentalCaptions reports that an instrumental session has
// no description left to hand out, so planning should pause and let the
// writer refill rather than sending the terse tag list again.
func (b *Builder) AwaitingInstrumentalCaptions(s *session.Session) bool {
	if s == nil || s.Vocal || !b.helperUsable() {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.instReady[b.instrumentalKey(Render(s))]) == 0
}

// takeInstrumentalCaption pops one stocked description, or returns
// empty when the shelf is bare - in which case the terse steering
// caption stands, exactly as it always did.
func (b *Builder) takeInstrumentalCaption(s *session.Session, r Rendered) StockedCaption {
	key := b.instrumentalKey(r)
	b.mu.Lock()
	defer b.mu.Unlock()
	shelf := b.instReady[key]
	if len(shelf) == 0 {
		return StockedCaption{}
	}
	out := shelf[0]
	b.instReady[key] = shelf[1:]
	return out
}

// StockedCaption is one instrumental piece's description and the name
// written from it, both before the piece exists.
type StockedCaption struct {
	Text     string
	Title    string
	Subtitle string
}

// captionTimeout bounds one description call.
const captionTimeout = 90 * time.Second

// songNameBudget bounds one naming call in the window where the helper
// has the graphics card. It sits between the description's budget and
// the lyric writer's: the answer is two short strings, but it is the
// song's only chance at a name, and there is a retry inside it.
const songNameBudget = 90 * time.Second

// captionSong asks the helper to describe one song, synchronously; the
// caller owns the timing. Empty on any failure - the terse steering
// caption is the fallback, never a blocker.
func (b *Builder) captionSong(ctx context.Context, style, lyrics string) string {
	if !b.helperUsable() {
		return ""
	}
	user := "STYLE: " + style + "\nLYRICS:\n" + lyricExcerpt(lyrics, 10)
	out, err := b.ollama.ChatWith(ctx, captionSystem, user, ChatOpts{
		KeepAliveSeconds: scribeKeepAlive,
	})
	if err != nil {
		return ""
	}
	return sanitizeLine(out, 400)
}

// StockLyrics writes lyric sheets ahead for the session's steering
// context until `want` sit ready or the context ends, returning how
// many it wrote. It runs synchronously - the caller owns the timing -
// and is meant for the window when the music engine is hibernated and
// the helper has the whole graphics card: each sheet is written there
// in seconds, and its song is named in the same breath. Languages are
// drawn per sheet exactly as they would be at consumption time.
func (b *Builder) StockLyrics(ctx context.Context, s *session.Session, want int, stop func(wrote int) bool) int {
	if s == nil || !s.Vocal || !b.helperUsable() {
		return 0
	}
	gen := b.generatorFor(s)
	r := Render(s)
	key := b.lyricsKey(gen, s, r)
	acceptable := b.langAcceptable(s, r)
	// Sheets in languages no longer wanted are stale steering context:
	// drop them up front, so they neither count toward the target nor
	// get sung.
	b.mu.Lock()
	kept := b.lyrReady[key][:0]
	for _, st := range b.lyrReady[key] {
		if acceptable(st.Lang) {
			kept = append(kept, st)
		}
	}
	b.lyrReady[key] = kept
	b.mu.Unlock()
	wrote := 0
	for ctx.Err() == nil && b.helperUsable() && (stop == nil || !stop(wrote)) {
		lang := b.chooseLanguage(s, r)
		b.mu.Lock()
		have := len(b.lyrReady[key])
		inFlight := b.pending[key]
		hooks := append([]string(nil), b.lyrHooks[hooksKey(key, lang)]...)
		b.mu.Unlock()
		if have >= want {
			break
		}
		if inFlight {
			// A background fill from the previous cycle is mid-write;
			// running a second call against the same helper buys
			// nothing. Let it land and count it.
			select {
			case <-ctx.Done():
				return wrote
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		req := LyricsRequest{
			Style:        r.Caption,
			Theme:        s.LyricsTheme,
			Language:     lang.Code,
			LanguageName: lang.Name,
			AvoidHooks:   hooks,
		}
		callCtx, cancel := context.WithTimeout(ctx, gen.Timeout())
		out, err := gen.Generate(callCtx, b.ollama, req)
		cancel()
		out = strings.TrimSpace(out)
		if len(out) > maxLyricsBytes {
			out = out[:maxLyricsBytes]
		}
		if err != nil || out == "" {
			if ctx.Err() != nil {
				// The phase's own budget ended mid-call; that is the
				// caller's clock running out, not the helper failing.
				return wrote
			}
			if err == nil {
				err = fmt.Errorf("%s wrote empty lyrics", gen.Name())
			}
			b.noteFailure(err)
			return wrote
		}
		b.noteSuccess()
		st := StockedLyrics{Lang: lang, Text: out}
		// The model is warm from the lyric write: describe the song and
		// name it in the same breath, both before the sheet counts as
		// written. The name has to exist now - this is the only window
		// where the helper has the graphics card.
		capCtx, capCancel := context.WithTimeout(ctx, captionTimeout)
		st.Caption = b.captionSong(capCtx, r.Caption, out)
		capCancel()
		nameCtx, nameCancel := context.WithTimeout(ctx, songNameBudget)
		st.Title, st.Subtitle = b.titleSong(nameCtx, r.Caption, out)
		nameCancel()
		b.mu.Lock()
		b.lyrReady[key] = append(b.lyrReady[key], st)
		b.recordHookLocked(key, st)
		b.mu.Unlock()
		wrote++
		b.log.Info("lyrics stocked ahead", "event", "lyrics_stocked",
			"language", lang.Name, "ready", have+1)
	}
	return wrote
}

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
// StockedLyrics is one written-ahead lyric sheet together with the
// language it was written in - the language is chosen when the words
// are written, so a stocked sheet is always usable as-is.
type StockedLyrics struct {
	Lang Language
	Text string
	// Title and Subtitle are the song's display names, written from
	// these very words in the same round. They travel with the sheet
	// into the spec, the plan and the rendered file, so a song can
	// never reach a listener without a name of its own - and never has
	// to be renamed under one who is already hearing it.
	Title    string
	Subtitle string
	// Caption is a rich one-line description of this specific song,
	// written by the helper from the style and the song's own words.
	// The music generator is conditioned on the caption text, so a
	// unique caption per song is a large share of what makes tracks
	// from one station sound different from each other; every song
	// sharing one terse tag list is a large share of what makes them
	// blur together. Empty falls back to the steering caption.
	Caption string
}

// lyricsKey names a steering context's lyric queue. The language is
// not part of the key: it travels with each stocked sheet instead.
func (b *Builder) lyricsKey(gen LyricsGenerator, s *session.Session, r Rendered) string {
	return "l|" + gen.Name() + "|" + r.Caption + "|" + s.LyricsTheme
}

// chooseLanguage picks the language the next song is sung in: a
// steered-in language pins the session, otherwise one of the
// switched-on languages is drawn at random. A language a preset
// happens to carry is only a default: it loses to a configured
// catalogue, or the list the listener is editing would quietly do
// nothing. With neither, the engine sings in whatever language it
// likes.
// chooseLanguage picks the language for one song: the one steered in by
// hand if there is one, otherwise a draw from the languages this session
// sings in. A session that lists none sings in whatever the engine
// picks, which is what an empty language is.
func (b *Builder) chooseLanguage(s *session.Session, r Rendered) Language {
	if r.LanguagePinned {
		return Language{Code: r.VocalLanguage, Name: LanguageName(r.VocalLanguage)}
	}
	drawn, ok := pickLanguage(ParseLanguages(s.SungLanguages))
	if !ok {
		return Language{}
	}
	return drawn
}

// langAcceptable returns a predicate for whether a stocked sheet's
// language is still wanted: a pinned language must match, a configured
// catalogue must have it switched on, and with neither anything goes.
func (b *Builder) langAcceptable(s *session.Session, r Rendered) func(Language) bool {
	if r.LanguagePinned {
		return func(l Language) bool { return l.Code == r.VocalLanguage }
	}
	enabled := ParseLanguages(s.SungLanguages)
	if len(enabled) == 0 {
		// The session names no language: the listener left the choice
		// to the engine, and sheets written under that choice (an empty
		// language) are exactly what fits.
		return func(l Language) bool { return l == Language{} }
	}
	return func(lang Language) bool {
		for _, l := range enabled {
			if l.Code != "" && lang.Code != "" {
				if l.Code == lang.Code {
					return true
				}
				continue
			}
			if l.Name == lang.Name {
				return true
			}
		}
		return false
	}
}

func (b *Builder) buildLyrics(s *session.Session, r Rendered) (StockedLyrics, bool) {
	if !b.helperUsable() {
		return StockedLyrics{}, false
	}
	gen := b.generatorFor(s)
	key := b.lyricsKey(gen, s, r)
	acceptable := b.langAcceptable(s, r)
	var out StockedLyrics
	got := false
	b.mu.Lock()
	for len(b.lyrReady[key]) > 0 && !got {
		cand := b.lyrReady[key][0]
		b.lyrReady[key] = b.lyrReady[key][1:]
		if acceptable(cand.Lang) {
			out, got = cand, true
		}
		// A sheet in a language no longer wanted is stale steering
		// context; drop it.
	}
	if got {
		b.lyrLast[key] = out
		b.lyrLastUses[key] = 1
		b.recordHookLocked(key, out)
	} else if last := b.lyrLast[key]; last.Text != "" && acceptable(last.Lang) &&
		b.lyrLastUses[key] <= maxSheetReuse {
		// Reuse bridges a helper hiccup - once. A batch that outruns
		// the supply of fresh sheets falls through to sample mode,
		// where the engine invents different words for every song;
		// endless re-renders of one sheet is what made a whole day of
		// radio sound like the same song. The reused sheet also obeys
		// the same language rules a fresh one would.
		b.lyrLastUses[key]++
		out, got = last, true
	}
	needFill := len(b.lyrReady[key]) < lyricsReadyTarget
	phased := b.phased
	b.mu.Unlock()
	// Under phased generation, a busy engine means the card is taken
	// and a background write would land on the CPU - minutes per song
	// on a machine whose owner is using it. Phased cycles get their
	// words from wordsmith rounds on the free card instead, so the
	// fill waits for the card. The fused path never releases the card
	// at all and has always paid the CPU price for its lyrics; it
	// keeps doing so.
	if needFill && !(phased && b.engineBusyNow()) {
		b.fillLyricsAsync(key, gen, s, r)
	}
	return out, got
}

// SetPhased tells the builder whether generation is phased, which is
// what decides if a busy engine should pause background lyric writing.
func (b *Builder) SetPhased(v bool) {
	b.mu.Lock()
	b.phased = v
	b.mu.Unlock()
}

// engineBusyNow reports whether the music engine currently holds the
// graphics card.
func (b *Builder) engineBusyNow() bool {
	return b.ollama != nil && b.ollama.engineBusy.Load()
}

// AwaitingLyrics reports whether the next vocal song would have to fall
// back to engine-invented words: the helper is usable but has no sheet
// stocked and no reuse left. A planner that sees true should stop and
// let a wordsmith round refill the shelf on the free card, rather than
// planning songs the writer never got to.
func (b *Builder) AwaitingLyrics(s *session.Session) bool {
	if s == nil || !s.Vocal || !b.helperUsable() {
		return false
	}
	gen := b.generatorFor(s)
	r := Render(s)
	key := b.lyricsKey(gen, s, r)
	acceptable := b.langAcceptable(s, r)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, st := range b.lyrReady[key] {
		if acceptable(st.Lang) {
			return false
		}
	}
	if last := b.lyrLast[key]; last.Text != "" && acceptable(last.Lang) &&
		b.lyrLastUses[key] <= maxSheetReuse {
		return false
	}
	return true
}

// hooksKey buckets remembered hook lines per language within a
// steering context, so one language's chorus does not evict another's
// and a lyric prompt only ever sees its own language's lines.
func hooksKey(key string, lang Language) string {
	return key + "|" + lang.Code + "|" + lang.Name
}

// recordHookLocked remembers a sheet's hook line (b.mu held). Already
// remembered lines are not re-added, so recording at write time and at
// consumption time cannot double up.
func (b *Builder) recordHookLocked(key string, st StockedLyrics) {
	h := hookLine(st.Text)
	if h == "" {
		return
	}
	hk := hooksKey(key, st.Lang)
	for _, have := range b.lyrHooks[hk] {
		if have == h {
			return
		}
	}
	hooks := append(b.lyrHooks[hk], h)
	if len(hooks) > maxAvoidHooks {
		hooks = hooks[len(hooks)-maxAvoidHooks:]
	}
	b.lyrHooks[hk] = hooks
}

// fillLyricsAsync runs one lyric generator call in the background and
// queues its result. At most one write per context is in flight. The
// language is chosen here, at write time, and the finished words are
// handed straight to the namer - the helper model is already loaded,
// so the song's title costs one more short call on the same card.
func (b *Builder) fillLyricsAsync(key string, gen LyricsGenerator, s *session.Session, r Rendered) {
	b.mu.Lock()
	if b.pending[key] {
		b.mu.Unlock()
		return
	}
	b.pending[key] = true
	runCtx := b.runCtx
	b.mu.Unlock()
	lang := b.chooseLanguage(s, r)
	b.mu.Lock()
	hooks := append([]string(nil), b.lyrHooks[hooksKey(key, lang)]...)
	b.mu.Unlock()
	req := LyricsRequest{
		Style:        r.Caption,
		Theme:        s.LyricsTheme,
		Language:     lang.Code,
		LanguageName: lang.Name,
		AvoidHooks:   hooks,
	}
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
		var caption, title, subtitle string
		if err == nil && out != "" {
			capCtx, capCancel := context.WithTimeout(runCtx, captionTimeout)
			caption = b.captionSong(capCtx, r.Caption, out)
			capCancel()
			// The fused path writes while the engine holds the card, so
			// this call runs on the processor: slow, but the name has
			// to come from here or from nowhere.
			nameCtx, nameCancel := context.WithTimeout(runCtx, chatTimeout)
			title, subtitle = b.titleSong(nameCtx, r.Caption, out)
			nameCancel()
		}
		b.mu.Lock()
		delete(b.pending, key)
		if err == nil && out != "" {
			if len(b.lyrReady) > 64 {
				// The steering context changed many times; drop stale
				// queues wholesale.
				b.lyrReady = map[string][]StockedLyrics{}
				b.lyrLast = map[string]StockedLyrics{}
				b.lyrLastUses = map[string]int{}
				b.lyrHooks = map[string][]string{}
			}
			st := StockedLyrics{Lang: lang, Text: out, Caption: caption, Title: title, Subtitle: subtitle}
			b.lyrReady[key] = append(b.lyrReady[key], st)
			b.recordHookLocked(key, st)
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

// AwaitHelper blocks until the background usability probe has
// answered (true) or the budget runs out (false). Short-lived CLI runs
// that want the lyric writer call this before deciding; the radio
// itself never waits on the helper.
func (b *Builder) AwaitHelper(ctx context.Context, budget time.Duration) bool {
	if b.ollama == nil {
		return false
	}
	deadline := time.Now().Add(budget)
	for {
		if b.helperUsable() {
			return true
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
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
		b.restEarnedIdle = !b.engineBusyNow()
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

// SetEngineBusy tells the helper whether the music engine currently
// holds the graphics card. Going idle also ends any rest the helper
// was serving: its timeouts were the crowded card's fault, and the
// card is free now.
func (b *Builder) SetEngineBusy(busy bool) {
	if b.ollama == nil {
		return
	}
	b.ollama.SetEngineBusy(busy)
	if !busy {
		b.mu.Lock()
		if !b.restEarnedIdle {
			b.restAfter = time.Time{}
		}
		b.mu.Unlock()
	}
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
