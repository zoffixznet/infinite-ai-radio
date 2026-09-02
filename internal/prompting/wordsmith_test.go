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
