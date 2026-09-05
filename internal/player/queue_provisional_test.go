package player

import (
	"context"
	"testing"

	"iar/internal/engine"
	"iar/internal/engine/enginetest"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/trackbuffer"
)

// A client that already holds a song's real name must be able to tell
// it apart from a stand-in read off the prompt. Without that, the
// phone adopts whatever the listing last said and the song renames
// itself under a listener who is only listening to it.
func TestQueueListingSaysWhichNamesAreStandIns(t *testing.T) {
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
	key := prompting.SongKey(track.Lyrics)
	if err := o.Buffer.PutTrack(context.Background(), 0, 3, key, track); err != nil {
		t.Fatal(err)
	}

	listed := func() QueueTrack {
		t.Helper()
		_, rows := o.QueueTracks()
		for _, r := range rows {
			if r.Kind == "queue" {
				return r
			}
		}
		t.Fatal("the buffered song was not listed")
		return QueueTrack{}
	}

	// Nobody has named it yet: the listing still offers something
	// readable, and says plainly that it is a stand-in.
	row := listed()
	if row.Title == "" {
		t.Fatal("a listed song carried no name at all")
	}
	if !row.TitleProvisional {
		t.Fatalf("the prompt-derived stand-in %q was offered as the song's own name", row.Title)
	}

	// Once the helper has named the song, the same listing offers that
	// name as final.
	b.PrimeTitle(key, "Steel In The Water", "nu-metal, driving")
	o.retitlePass()
	row = listed()
	if row.Title != "Steel In The Water" {
		t.Fatalf("listed name = %q, want the helper's", row.Title)
	}
	if row.TitleProvisional {
		t.Fatal("the song's own name was still marked a stand-in")
	}
}
