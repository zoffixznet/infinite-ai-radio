package prompting

import (
	"context"
	"testing"
	"time"

	"iar/internal/engine"
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

	// The songs were named from their own words, keyed by their words.
	key := SongKey(f.reply)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if title, _, ok := b.TitleForKey(key); ok {
			if title != "Steel In The Water" {
				t.Fatalf("title = %q", title)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stocked song was never named")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// BuildSpec consumes a stocked sheet instead of writing anything.
	spec := b.BuildSpec(context.Background(), s, 150)
	if spec.Lyrics != f.reply {
		t.Fatalf("BuildSpec did not use the stocked sheet: %q", spec.Lyrics[:40])
	}
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
	s.Languages = map[string]bool{"Spanish": false} // French on, Spanish off

	if wrote := b.StockLyrics(context.Background(), s, 1, nil); wrote != 1 {
		t.Fatalf("wrote %d", wrote)
	}
	// The listener turns French off; the stocked French sheet is stale.
	s.Languages = map[string]bool{"French": false, "Spanish": true}
	f.reply = "[Verso]\nacero en el agua"
	spec := b.BuildSpec(context.Background(), s, 150)
	if spec.Lyrics == "[Couplet]\nl'acier dans l'eau" {
		t.Fatal("a switched-off language's sheet was consumed")
	}
}

// SongKey is derived from the words alone: stable, and absent for
// instrumentals.
func TestSongKeyProperties(t *testing.T) {
	a := SongKey("[Verse]\nwords")
	if a == "" || a != SongKey("[Verse]\nwords") {
		t.Fatalf("key not stable: %q", a)
	}
	if b := SongKey("[Verse]\nother words"); b == a {
		t.Fatal("different words, same key")
	}
	if SongKey("") != "" {
		t.Fatal("empty lyrics must have no key")
	}
	if SongKey(engine.InstrumentalLyrics) != "" {
		t.Fatal("instrumentals must have no key")
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
