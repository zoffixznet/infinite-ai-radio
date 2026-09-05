package player

import (
	"context"
	"strings"
	"testing"

	"iar/internal/engine"
	"iar/internal/engine/enginetest"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/trackbuffer"
)

// A song is named where its words are written - on the free graphics
// card, before it is planned, rendered or heard - and that name travels
// with it: into the plan, onto the disk beside the audio, out to the
// listing every phone reads. The bug this replaces: the name was asked
// for in the background and applied wherever the song happened to be
// when the answer landed, which on a deep batch was hours later, so
// songs played for hours under "Nu-metal" and then renamed themselves
// mid-play.
func TestASongCarriesItsNameFromItsWordsToTheListener(t *testing.T) {
	cfg := testConfig()
	cfg.Buffer.Phased = true
	o := New(cfg, enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 0, testLogger())

	// What the renderer hands back: the spec carries the name the
	// wordsmith wrote with the words.
	track := &engine.Track{
		Prompt:  "nu-metal, aggressive, heavy groove",
		Lyrics:  "[Verse]\napoy sa dibdib",
		Samples: make([]int16, 9600),
		Spec: engine.Spec{
			Prompt:   "nu-metal, aggressive, heavy groove",
			Lyrics:   "[Verse]\napoy sa dibdib",
			Title:    "Apoy Sa Dibdib",
			Subtitle: "nu-metal, driving",
		},
	}
	o.nameTrack(track)
	if track.Title != "Apoy Sa Dibdib" || track.Subtitle != "nu-metal, driving" {
		t.Fatalf("the name written with the words did not reach the track: %+v", track)
	}

	// It goes to disk with the audio, so the listing has it and a
	// restart still has it.
	if err := o.Buffer.PutTrack(context.Background(), 0, 1, track); err != nil {
		t.Fatal(err)
	}
	if got := o.Buffer.List(0); len(got) != 1 || got[0].Title != "Apoy Sa Dibdib" {
		t.Fatalf("sidecar: %+v", got)
	}
	_, listed := o.QueueTracks()
	if len(listed) != 1 || listed[0].Title != "Apoy Sa Dibdib" {
		t.Fatalf("queue listing: %+v", listed)
	}

	// And feeding it into the stream does not touch the name.
	fed, _, ok := o.Buffer.NextTrack(context.Background(), 0)
	if !ok {
		t.Fatal("nothing fed")
	}
	o.nameTrack(fed)
	if fed.Title != "Apoy Sa Dibdib" || fed.Subtitle != "nu-metal, driving" {
		t.Fatalf("feeding renamed the song: %+v", fed)
	}
}

// A song whose words the engine invented has no written name to carry -
// the helper never saw those words, and by then the graphics card is
// the engine's. It takes the deterministic name from its own
// description, once, and keeps it.
func TestAnUnwordedSongTakesItsNameOnceAndKeepsIt(t *testing.T) {
	o := New(testConfig(), enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	track := &engine.Track{
		Prompt: "desert rock, wide open, dusty",
		Spec:   engine.Spec{Prompt: "desert rock, wide open, dusty"},
	}
	o.nameTrack(track)
	first := track.Title
	if first == "" {
		t.Fatal("a song reached the stream with no name at all")
	}
	if !strings.Contains(strings.ToLower(first), "desert") {
		t.Fatalf("the fallback name ignores the song's own description: %q", first)
	}
	o.nameTrack(track)
	if track.Title != first {
		t.Fatalf("the name changed on a second pass: %q -> %q", first, track.Title)
	}
}
