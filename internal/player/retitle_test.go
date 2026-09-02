package player

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"iar/internal/engine"
	"iar/internal/engine/enginetest"
	"iar/internal/library"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/trackbuffer"
)

// provisionalTrack is a fed song still wearing its prompt-derived
// stand-in name, the way every song starts on a machine where the
// helper answers late.
func provisionalTrack(tag string) *engine.Track {
	lyrics := "[Verse]\nsteel in the water " + tag + "\n\n[Chorus]\nhold the line"
	return &engine.Track{
		ID:               "t-" + tag,
		Prompt:           "nu-metal, alternative metal, aggressive, heavy groove",
		Lyrics:           lyrics,
		Title:            "Nu-metal",
		Subtitle:         "alternative metal, aggressive",
		TitleKey:         prompting.SongKey(lyrics),
		TitleProvisional: true,
	}
}

// The bug this guards against: the helper's answer arrives after the
// song was fed, lands in a cache, and nothing ever reads it again - so
// the song plays as "Nu-metal" forever, along with every song after it.
// A late answer must reach the queued songs, the playing one and the
// previous one, and show up in Status.
func TestLateTitleReachesQueuedAndPlayingTracks(t *testing.T) {
	b := prompting.NewBuilder(nil, testLogger())
	o := New(testConfig(), enginetest.NewMock(), b, session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())

	queued := provisionalTrack("q")
	playing := provisionalTrack("p")
	previous := provisionalTrack("v")
	o.mu.Lock()
	o.queue = append(o.queue, queued)
	o.cur = newTrackSource(playing, "music")
	o.prevTrack = previous
	o.mu.Unlock()

	// No answer yet: the pass changes nothing.
	o.retitlePass()
	if !playing.TitleProvisional || playing.Title != "Nu-metal" {
		t.Fatalf("pass without an answer changed the track: %+v", playing)
	}

	// The helper answers - minutes late, as it does on a busy machine.
	b.PrimeTitle(playing.TitleKey, "Steel In The Water", "nu-metal, driving")
	b.PrimeTitle(queued.TitleKey, "Hold The Line", "nu-metal, anthemic")
	o.retitlePass()

	if playing.Title != "Steel In The Water" || playing.TitleProvisional {
		t.Fatalf("playing track kept its stand-in name: %+v", playing)
	}
	if queued.Title != "Hold The Line" || queued.TitleProvisional {
		t.Fatalf("queued track kept its stand-in name: %+v", queued)
	}
	if !previous.TitleProvisional {
		t.Fatal("a track with no answer must stay provisional")
	}
	if st := o.Status(); st.TrackTitle != "Steel In The Water" {
		t.Fatalf("Status shows %q; the interfaces poll this", st.TrackTitle)
	}

	// The answer for the previous track lands on a later pass.
	b.PrimeTitle(previous.TitleKey, "Ash And Anchor", "nu-metal, slow burn")
	o.retitlePass()
	if previous.Title != "Ash And Anchor" {
		t.Fatalf("previous track kept its stand-in name: %+v", previous)
	}
}

// A name that resolves while the song is still rendered on disk must be
// written into its metadata, so the queue listing and a later run show
// it instead of re-deriving the stand-in.
func TestLateTitlePersistsToTheDiskBuffer(t *testing.T) {
	buf := trackbuffer.New(t.TempDir(), 0, testLogger())
	track := &engine.Track{
		Prompt:  "nu-metal, aggressive",
		Lyrics:  "[Verse]\nsteel in the water",
		Samples: make([]int16, 9600),
	}
	if err := buf.PutTrack(context.Background(), 0, 5, prompting.SongKey(track.Lyrics), track); err != nil {
		t.Fatal(err)
	}
	entries := buf.List(0)
	if len(entries) != 1 || entries[0].Title != "" {
		t.Fatalf("unexpected listing: %+v", entries)
	}

	buf.SetTitle(0, entries[0].Base, "Steel In The Water", "nu-metal, driving")
	entries = buf.List(0)
	if len(entries) != 1 || entries[0].Title != "Steel In The Water" || entries[0].Subtitle != "nu-metal, driving" {
		t.Fatalf("title not persisted: %+v", entries)
	}

	// The persisted name rides the track out of the buffer.
	got, key, ok := buf.NextTrack(context.Background(), 0)
	if !ok || got.Title != "Steel In The Water" || key != prompting.SongKey(track.Lyrics) {
		t.Fatalf("NextTrack = %+v key=%q ok=%v", got, key, ok)
	}

	// Naming a song that was already fed is a quiet no-op.
	buf.SetTitle(0, entries[0].Base, "Too Late", "gone")
	if left := buf.List(0); len(left) != 0 {
		t.Fatalf("SetTitle after feed resurrected metadata: %+v", left)
	}
}

// The retitle pass persists names for songs still on disk.
func TestRetitlePassNamesTheDiskBuffer(t *testing.T) {
	b := prompting.NewBuilder(nil, testLogger())
	cfg := testConfig()
	cfg.Buffer.Phased = true
	o := New(cfg, enginetest.NewMock(), b, session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 0, testLogger())

	track := &engine.Track{
		Prompt:  "nu-metal, aggressive",
		Lyrics:  "[Verse]\nsteel in the water",
		Samples: make([]int16, 9600),
	}
	if err := o.Buffer.PutTrack(context.Background(), 0, 3, prompting.SongKey(track.Lyrics), track); err != nil {
		t.Fatal(err)
	}

	b.PrimeTitle(prompting.SongKey(track.Lyrics), "Steel In The Water", "nu-metal, driving")
	o.retitlePass()

	entries := o.Buffer.List(0)
	if len(entries) != 1 || entries[0].Title != "Steel In The Water" {
		t.Fatalf("disk entry not renamed: %+v", entries)
	}
}

// Instrumentals have no words to name from; their prompt-derived name
// is final and the pass must leave them alone.
func TestRetitleSkipsInstrumentals(t *testing.T) {
	b := prompting.NewBuilder(nil, testLogger())
	o := New(testConfig(), enginetest.NewMock(), b, session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	tr := provisionalTrack("i")
	tr.Lyrics = engine.InstrumentalLyrics
	tr.TitleProvisional = false // feed never marks instrumentals provisional
	o.mu.Lock()
	o.queue = append(o.queue, tr)
	o.mu.Unlock()

	b.PrimeTitle(prompting.SongKey(engine.InstrumentalLyrics), "Should Not Apply", "x")
	o.retitlePass()
	if !strings.HasPrefix(tr.Title, "Nu-metal") {
		t.Fatalf("instrumental was renamed: %+v", tr)
	}
}

// A track banked under its stand-in name gets its library sidecar
// renamed when the real name lands, so instant starts in later runs do
// not resurrect the stand-in.
func TestLateTitleReachesTheBankedCopy(t *testing.T) {
	b := prompting.NewBuilder(nil, testLogger())
	o := New(testConfig(), enginetest.NewMock(), b, session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	o.Library = library.New(t.TempDir(), 100, testLogger())

	tr := provisionalTrack("b")
	tr.Samples = make([]int16, 9600)
	id, err := o.Library.Put("nu-metal", tr)
	if err != nil || id == "" {
		t.Fatalf("Put: id=%q err=%v", id, err)
	}
	o.mu.Lock()
	o.queue = append(o.queue, tr)
	o.bankRefs[tr.ID] = bankRef{key: "nu-metal", id: id}
	o.mu.Unlock()

	b.PrimeTitle(tr.TitleKey, "Steel In The Water", "nu-metal, driving")
	o.retitlePass()

	entries := o.Library.Entries("nu-metal")
	if len(entries) != 1 || entries[0].Title != "Steel In The Water" {
		t.Fatalf("banked sidecar kept the stand-in: %+v", entries)
	}
	o.mu.Lock()
	_, still := o.bankRefs[tr.ID]
	o.mu.Unlock()
	if still {
		t.Fatal("bank ref should be consumed by the rename")
	}
}

// Applying answers is never rationed - a pass with many resolved names
// waiting persists all of them at once, even though it only STARTS a
// couple of new naming calls per pass.
func TestRetitleAppliesAllReadyAnswersInOnePass(t *testing.T) {
	b := prompting.NewBuilder(nil, testLogger())
	cfg := testConfig()
	cfg.Buffer.Phased = true
	o := New(cfg, enginetest.NewMock(), b, session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 0, testLogger())

	for seq := 1; seq <= 5; seq++ {
		track := &engine.Track{
			Prompt:  "nu-metal, aggressive",
			Lyrics:  fmt.Sprintf("[Verse]\nsteel in the water %d", seq),
			Samples: make([]int16, 9600),
		}
		key := prompting.SongKey(track.Lyrics)
		if err := o.Buffer.PutTrack(context.Background(), 0, seq, key, track); err != nil {
			t.Fatal(err)
		}
		b.PrimeTitle(key, "Named Song", "nu-metal")
	}
	o.retitlePass()
	for _, e := range o.Buffer.List(0) {
		if e.Title != "Named Song" {
			t.Fatalf("an already-answered song was left unnamed: %+v", e)
		}
	}
}
