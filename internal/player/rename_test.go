package player

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"iar/internal/engine"
	"iar/internal/engine/enginetest"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/songbook"
	"iar/internal/trackbuffer"
)

// namedTrack is a song as it now reaches the stream: with the name that
// was written from its own words, which nothing but a listener changes.
func namedTrack(tag string) *engine.Track {
	lyrics := "[Verse]\nsteel in the water " + tag + "\n\n[Chorus]\nhold the line"
	return &engine.Track{
		ID:       "t-" + tag,
		Prompt:   "nu-metal, alternative metal, aggressive, heavy groove",
		Lyrics:   lyrics,
		Title:    "Steel In The Water",
		Subtitle: "alternative metal, aggressive",
	}
}

// The name a song was given is the helper's reading of its words, and a
// listener who hears the song may disagree. Renaming from the live page
// has to stick, in the playing copy and everywhere the song is
// remembered.
func TestRenamingThePlayingSongSticks(t *testing.T) {
	b := prompting.NewBuilder(nil, testLogger())
	o := New(testConfig(), enginetest.NewMock(), b, session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	playing := namedTrack("p")
	o.mu.Lock()
	o.cur = newTrackSource(playing, "music")
	o.curTrack = playing
	o.mu.Unlock()

	if ack := o.Retitle("", "Harbour Lights"); !strings.Contains(ack, "Harbour Lights") {
		t.Fatalf("rename ack = %q", ack)
	}
	if st := o.Status(); st.TrackTitle != "Harbour Lights" {
		t.Fatalf("Status shows %q; every interface polls this", st.TrackTitle)
	}
	// And the song itself, wherever it is read from next.
	if playing.Title != "Harbour Lights" {
		t.Fatalf("the playing song was not renamed: %+v", playing)
	}
}

// Renaming a song nobody has played yet: it is a file on disk, and the
// name has to survive being fed into the stream.
func TestRenamingASongStillOnDisk(t *testing.T) {
	b := prompting.NewBuilder(nil, testLogger())
	cfg := testConfig()
	cfg.Buffer.Phased = true
	o := New(cfg, enginetest.NewMock(), b, session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 0, testLogger())

	lyrics := "[Verse]\nsteel in the water"
	track := &engine.Track{Prompt: "nu-metal, aggressive", Lyrics: lyrics, Samples: make([]int16, 9600)}
	if _, err := o.Buffer.PutTrack(context.Background(), 0, 3, track); err != nil {
		t.Fatal(err)
	}
	base := o.Buffer.List(0)[0].Base

	if ack := o.Retitle(bufTrackPrefix+base, "Harbour Lights"); !strings.Contains(ack, "Harbour Lights") {
		t.Fatalf("rename ack = %q", ack)
	}
	if got := o.Buffer.List(0)[0]; got.Title != "Harbour Lights" {
		t.Fatalf("sidecar title = %q", got.Title)
	}
	_, tracks := o.QueueTracks()
	if len(tracks) != 1 || tracks[0].Title != "Harbour Lights" {
		t.Fatalf("queue listing: %+v", tracks)
	}
	// The name rides out of the buffer with the song.
	fed, ok := o.Buffer.Peek(context.Background(), 0, o.Buffer.List(0)[0].Base)
	if !ok || fed.Title != "Harbour Lights" {
		t.Fatalf("fed track = %+v ok=%v", fed, ok)
	}
}

// A phone downloaded a song while it was a file on disk, listed under
// its file name because it was rendered before songs carried an id of
// their own. The machine has since fed that file, which deletes it from
// disk - but the song keeps the file name as its id in memory, so a
// rename of the copy the listener is hearing still finds it.
func TestRenamingFollowsASongOutOfTheBuffer(t *testing.T) {
	b := prompting.NewBuilder(nil, testLogger())
	cfg := testConfig()
	cfg.Buffer.Phased = true
	o := New(cfg, enginetest.NewMock(), b, session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 0, testLogger())

	playing := namedTrack("p")
	playing.ID = bufTrackPrefix + "e00000000-00000003"
	o.mu.Lock()
	o.cur = newTrackSource(playing, "music")
	o.curTrack = playing
	o.mu.Unlock()

	if ack := o.Retitle(playing.ID, "Harbour Lights"); !strings.Contains(ack, "Harbour Lights") {
		t.Fatalf("rename ack = %q", ack)
	}
	if playing.Title != "Harbour Lights" {
		t.Fatalf("the in-memory song was not renamed: %+v", playing)
	}
	// A file the machine never fed, and never had, is refused gently.
	if ack := o.Retitle(bufTrackPrefix+"e00000000-00000009", "Nowhere"); !strings.Contains(ack, "no longer here") {
		t.Fatalf("unknown file ack = %q", ack)
	}
}

// A song the listener already saved is on disk under its old name.
// Renaming what is playing renames those files too - tag and all -
// because "rename this song" means the copy they kept as well.
func TestRenamingMovesTheSavedCopy(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	b := prompting.NewBuilder(nil, testLogger())
	o := New(testConfig(), enginetest.NewMock(), b, session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	o.SnippetsDir = t.TempDir()
	playing := namedTrack("p")
	playing.Samples = make([]int16, 2*48000*2) // two seconds, stereo
	o.mu.Lock()
	o.cur = newTrackSource(playing, "music")
	o.curTrack = playing
	o.mu.Unlock()

	if ack := o.SaveSnippet("", "drive"); !strings.Contains(ack, "drive/") {
		t.Fatalf("save ack = %q", ack)
	}
	dir := filepath.Join(o.SnippetsDir, "drive")
	waitFor(t, 30*time.Second, "the save to land on disk", func() bool {
		for _, s := range o.Status().SavedTrackIDs {
			if s == playing.ID {
				return true
			}
		}
		return false
	})
	before, _ := os.ReadDir(dir)
	if len(before) == 0 {
		t.Fatalf("nothing saved into %s", dir)
	}

	ack := o.Retitle("", "Harbour Lights")
	if !strings.Contains(ack, "saved copy included") {
		t.Fatalf("rename ack = %q", ack)
	}
	after, _ := os.ReadDir(dir)
	if len(after) != len(before) {
		t.Fatalf("rename changed the file count: %d -> %d", len(before), len(after))
	}
	var found bool
	for _, e := range after {
		if !strings.HasSuffix(e.Name(), ".mp3") {
			continue
		}
		if !strings.Contains(e.Name(), "harbour-lights") {
			t.Fatalf("saved file still called %q", e.Name())
		}
		found = true
	}
	if !found {
		t.Fatalf("no saved song left in %s: %v", dir, after)
	}
	// The lyric sheet moved with it rather than being orphaned.
	for _, e := range after {
		if strings.HasSuffix(e.Name(), ".txt") && !strings.Contains(e.Name(), "harbour-lights") {
			t.Fatalf("lyrics left behind as %q", e.Name())
		}
	}
	// Saving again is still a no-op: the same song, moved, not a new one.
	if ack := o.SaveSnippet("", "drive"); !strings.Contains(ack, "already saved") {
		t.Fatalf("repeat save after rename ack = %q", ack)
	}
}

// A phone plays its own downloaded copies, at its own pace, and can be
// a long way behind the speakers. Renaming what it is hearing must
// still work when this machine has let that song go entirely: the
// audio is gone from here, but the book remembers the song, and the
// name it records is the one a later save from the phone's copy uses.
func TestRenamingASongTheMachineHasFinishedWith(t *testing.T) {
	b := prompting.NewBuilder(nil, testLogger())
	o := New(testConfig(), enginetest.NewMock(), b, session.NewStore(t.TempDir()),
		session.New(), &capturePlayer{}, testLogger())

	gone := namedTrack("old")
	if err := o.Songbook.Record(songbook.Song{ID: gone.ID, Hash: "h", Title: gone.Title, Subtitle: gone.Subtitle}); err != nil {
		t.Fatal(err)
	}

	if ack := o.Retitle(gone.ID, "Harbour Lights"); !strings.Contains(ack, "Harbour Lights") {
		t.Fatalf("rename of a gone song = %q", ack)
	}
	rec, _ := o.Songbook.ByID(gone.ID)
	if rec.Title != "Harbour Lights" || rec.Subtitle != gone.Subtitle {
		t.Fatalf("the book's record: %+v", rec)
	}
	// A song nobody has ever heard of is still refused.
	if ack := o.Retitle("t-nothing", "Nowhere"); !strings.Contains(ack, "no longer here") {
		t.Fatalf("unknown id ack = %q", ack)
	}
}
