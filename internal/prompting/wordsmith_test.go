package prompting

import (
	"context"
	"testing"
	"time"

	"iar/internal/session"
)

// wordsmithSession is a vocal session using the one-call lyric writer,
// so the fake server's plain reply is the whole lyric sheet.
func wordsmithSession() *session.Session {
	s := session.New()
	s.Vocal = true
	s.BasePrompt = "nu-metal, aggressive, heavy groove"
	s.LyricsGenerator = "smoothbrain"
	return s
}

// StockLyrics writes the batch's words while the caller owns the free
// graphics card, and each song is named in the same breath - that is
// the whole point of the wordsmith phase.
func TestStockLyricsWritesAheadAndNamesEachSong(t *testing.T) {
	f := &fakeOllama{
		reply:     "[Verse]\nsteel in the water\n\n[Chorus]\nhold the line",
		jsonReply: `{"title":"Steel In The Water","subtitle":"nu-metal, driving"}`,
	}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	s := wordsmithSession()

	wrote := b.StockLyrics(context.Background(), s, 2, nil)
	if wrote != 2 {
		t.Fatalf("StockLyrics wrote %d sheets, want 2", wrote)
	}

	// The name is part of the sheet by the time the sheet counts as
	// written - not a request left in flight for someone to pick up
	// later.
	if spec := b.BuildSpec(context.Background(), s, 150); spec.Lyrics != f.reply ||
		spec.Title != "Steel In The Water" || spec.Subtitle != "nu-metal, driving" {
		t.Fatalf("BuildSpec: lyrics %q title %q subtitle %q",
			firstLine(spec.Lyrics), spec.Title, spec.Subtitle)
	}
}

// firstLine keeps a failure message short.
func firstLine(s string) string {
	if i := len(s); i > 40 {
		return s[:40]
	}
	return s
}

// A stocked sheet carries the language it was written in, and BuildSpec
// adopts it - the language decision moved to write time.
func TestStockedSheetBringsItsLanguage(t *testing.T) {
	f := &fakeOllama{reply: "[Couplet]\nl'acier dans l'eau"}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	b.SetLanguages([]string{"French"})
	s := wordsmithSession()
	s.SungLanguages = []string{"French"}

	if wrote := b.StockLyrics(context.Background(), s, 1, nil); wrote != 1 {
		t.Fatalf("wrote %d", wrote)
	}
	spec := b.BuildSpec(context.Background(), s, 150)
	if spec.Lyrics != f.reply || spec.VocalLanguageName != "French" {
		t.Fatalf("stocked language lost: lang=%q lyrics=%q", spec.VocalLanguageName, spec.Lyrics[:20])
	}
}

// A sheet stocked in a language the listener has since switched off is
// stale steering context: BuildSpec must drop it, not sing it.
func TestStaleLanguageSheetIsDropped(t *testing.T) {
	f := &fakeOllama{reply: "[Couplet]\nl'acier dans l'eau"}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	b.SetLanguages([]string{"French", "Spanish"})
	s := wordsmithSession()
	s.SungLanguages = []string{"French"}

	if wrote := b.StockLyrics(context.Background(), s, 1, nil); wrote != 1 {
		t.Fatalf("wrote %d", wrote)
	}
	// The listener turns French off; the stocked French sheet is stale.
	s.SungLanguages = []string{"Spanish"}
	f.reply = "[Verso]\nacero en el agua"
	spec := b.BuildSpec(context.Background(), s, 150)
	if spec.Lyrics == "[Couplet]\nl'acier dans l'eau" {
		t.Fatal("a switched-off language's sheet was consumed")
	}
}

// The regression this guards against, measured on a live machine: 16 of
// 16 rendered songs singing one identical sheet, because deep planning
// outran a slow helper and the reuse fallback had no limit. A sheet may
// bridge one hiccup; after that the batch falls to sample mode, where
// the engine invents different words for every song.
func TestASheetIsSungAtMostTwice(t *testing.T) {
	f := &fakeOllama{
		reply:    "[Verse]\nsteel in the water\n\n[Chorus]\nhold the line",
		maxFills: 1, // the helper writes exactly one sheet, then drought
	}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	s := wordsmithSession()

	if wrote := b.StockLyrics(context.Background(), s, 1, nil); wrote != 1 {
		t.Fatalf("wrote %d", wrote)
	}

	first := b.BuildSpec(context.Background(), s, 150)
	if first.Lyrics != f.reply {
		t.Fatalf("first song should sing the stocked sheet")
	}
	second := b.BuildSpec(context.Background(), s, 150)
	if second.Lyrics != f.reply {
		t.Fatalf("one reuse bridges the hiccup: %q", second.Lyrics)
	}
	third := b.BuildSpec(context.Background(), s, 150)
	if third.Lyrics == f.reply {
		t.Fatal("a third song must not sing the same sheet again")
	}
	if third.SampleQuery == "" {
		t.Fatal("past the reuse cap the engine invents the words (sample mode)")
	}
}

// Each stocked song carries its own rich caption, and the spec is
// conditioned on it. One terse tag list shared by every render is a
// large part of why a day of radio blurred together; the caption is
// what the music generator actually reads.
func TestStockedSongCarriesItsOwnCaption(t *testing.T) {
	f := &fakeOllama{replies: []string{
		"[Verse]\nsteel in the water",
		"An aggressive, high-energy nu-metal track driven by down-tuned guitars and a taut, punchy groove.",
	}}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	s := wordsmithSession()

	if wrote := b.StockLyrics(context.Background(), s, 1, nil); wrote != 1 {
		t.Fatalf("wrote %d", wrote)
	}
	spec := b.BuildSpec(context.Background(), s, 150)
	if spec.Lyrics != "[Verse]\nsteel in the water" {
		t.Fatalf("lyrics = %q", spec.Lyrics)
	}
	if spec.Prompt != "An aggressive, high-energy nu-metal track driven by down-tuned guitars and a taut, punchy groove." {
		t.Fatalf("the spec was not conditioned on the song's own caption: %q", spec.Prompt)
	}
}

// A failed caption call falls back to the steering caption and never
// blocks the sheet.
func TestCaptionFailureFallsBackToSteeringCaption(t *testing.T) {
	f := &fakeOllama{replies: []string{"[Verse]\nsteel in the water"}, maxFills: 1}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	s := wordsmithSession()

	if wrote := b.StockLyrics(context.Background(), s, 1, nil); wrote != 1 {
		t.Fatalf("wrote %d", wrote)
	}
	spec := b.BuildSpec(context.Background(), s, 150)
	if spec.Lyrics == "" || spec.Prompt == "" {
		t.Fatalf("sheet or caption lost: %+v", spec)
	}
	if spec.Prompt != Render(s).Caption {
		t.Fatalf("caption fallback should be the steering caption, got %q", spec.Prompt)
	}
}

// The planner must know when the writer has nothing ready, so it can
// stop and hand the card back instead of planning songs without words.
func TestAwaitingLyricsTracksTheSupply(t *testing.T) {
	f := &fakeOllama{reply: "[Verse]\nsteel in the water"}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	b.SetPhased(true)
	b.SetEngineBusy(true) // mid-cycle: no background fills may run
	s := wordsmithSession()

	if !b.AwaitingLyrics(s) {
		t.Fatal("an empty shelf must read as awaiting")
	}
	b.SetEngineBusy(false)
	if wrote := b.StockLyrics(context.Background(), s, 1, nil); wrote != 1 {
		t.Fatalf("wrote %d", wrote)
	}
	b.SetEngineBusy(true)
	if b.AwaitingLyrics(s) {
		t.Fatal("a stocked sheet must satisfy the planner")
	}
	b.BuildSpec(context.Background(), s, 150) // consumes the sheet (use 1)
	if b.AwaitingLyrics(s) {
		t.Fatal("one reuse remains; not yet awaiting")
	}
	b.BuildSpec(context.Background(), s, 150) // the one allowed reuse
	if !b.AwaitingLyrics(s) {
		t.Fatal("supply spent; the planner must wait for the wordsmith")
	}
}

// While a phased engine holds the card, consuming lyrics must not kick
// a background CPU write - the machine's owner is using that CPU, and
// the words come from wordsmith rounds on the free card instead.
func TestNoBackgroundLyricWritesWhileEngineBusy(t *testing.T) {
	f := &fakeOllama{reply: "[Verse]\nsteel in the water"}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	b.SetPhased(true)
	if wrote := b.StockLyrics(context.Background(), wordsmithSession(), 1, nil); wrote != 1 {
		t.Fatalf("wrote %d", wrote)
	}
	b.SetEngineBusy(true)
	// Let the in-flight naming, caption and eviction calls settle so
	// the counter only moves if BuildSpec itself starts a write.
	settle := f.fills.Load()
	for {
		time.Sleep(120 * time.Millisecond)
		if now := f.fills.Load(); now == settle {
			break
		} else {
			settle = now
		}
	}
	before := f.fills.Load()
	b.BuildSpec(context.Background(), wordsmithSession(), 150)
	time.Sleep(150 * time.Millisecond)
	if got := f.fills.Load(); got != before {
		t.Fatalf("a fill ran on the busy card: %d -> %d", before, got)
	}
	b.SetEngineBusy(false)
	b.BuildSpec(context.Background(), wordsmithSession(), 150)
	deadline := time.Now().Add(3 * time.Second)
	for f.fills.Load() == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.fills.Load() == before {
		t.Fatal("the freed card should resume background writing")
	}
}

// The helper can answer the words and then fail on the name - a timeout
// on a crowded card, or an unreadable reply. The sheet is still written
// (the words are the expensive part) and the song goes on to take the
// deterministic name from its own description. What must not happen is
// a silent failure that also stops the writer: the helper is plainly
// working, so it keeps its turn.
func TestASheetSurvivesANameThatFails(t *testing.T) {
	f := &fakeOllama{
		reply:    "[Verse]\nsteel in the water\n\n[Chorus]\nhold the line",
		failJSON: true, // every schema-constrained call - the naming ones
	}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	s := wordsmithSession()

	if wrote := b.StockLyrics(context.Background(), s, 1, nil); wrote != 1 {
		t.Fatalf("a failed name lost the sheet: wrote %d", wrote)
	}
	if n := f.jsonCalls.Load(); n < 2 {
		t.Fatalf("naming was tried %d times; it gets one retry while the model is warm", n)
	}
	if !b.helperUsable() {
		t.Fatal("a failed name put the whole helper to rest")
	}
	spec := b.BuildSpec(context.Background(), s, 150)
	if spec.Lyrics != f.reply {
		t.Fatalf("the sheet was not used: %q", firstLine(spec.Lyrics))
	}
	if spec.Title != "" {
		t.Fatalf("a failed name produced one anyway: %q", spec.Title)
	}
}

// An instrumental has no words to be named from, but it does have the
// description written for it in the same round - and that is written on
// the same free card, so the name comes from there rather than from
// nowhere.
func TestInstrumentalsAreNamedFromTheirDescription(t *testing.T) {
	f := &fakeOllama{
		reply:     "Slow-burning synth arpeggios over a patient kick, widening into a hazy chorus of pads.",
		jsonReply: `{"title":"Patient Kick","subtitle":"synthwave, hazy"}`,
	}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	s := session.New()
	s.BasePrompt = "synthwave, hazy"
	s.Vocal = false

	if wrote := b.StockInstrumentalCaptions(context.Background(), s, 1, nil); wrote != 1 {
		t.Fatalf("wrote %d descriptions", wrote)
	}
	spec := b.BuildSpec(context.Background(), s, 150)
	if spec.Prompt != f.reply {
		t.Fatalf("the description was not used: %q", firstLine(spec.Prompt))
	}
	if spec.Title != "Patient Kick" || spec.Subtitle != "synthwave, hazy" {
		t.Fatalf("instrumental name = %q / %q", spec.Title, spec.Subtitle)
	}
}
