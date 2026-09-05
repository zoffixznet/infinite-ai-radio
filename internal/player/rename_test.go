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
	"iar/internal/library"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/trackbuffer"
)

// The helper's name for a song is a guess, and a listener who hears the
// song knows better. Renaming from the live page has to stick: the
// playing copy, everywhere the song is remembered, and - the part that
// is easy to miss - the late naming pass, which would otherwise come
// along twenty seconds later and put the guess back.
func TestRenamingThePlayingSongSticks(t *testing.T) {
	b := prompting.NewBuilder(nil, testLogger())
	o := New(testConfig(), enginetest.NewMock(), b, session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	playing := provisionalTrack("p")
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

	// The helper answers afterwards, as it does minutes late on a busy
	// machine. The listener's name wins.
	b.PrimeTitle(playing.TitleKey, "Steel In The Water", "nu-metal, driving")
	o.retitlePass()
	if playing.Title != "Harbour Lights" {
		t.Fatalf("the late naming pass overwrote a typed name: %q", playing.Title)
	}

	// And the listing the phones read agrees, without the stand-in flag
	// that lets a client swap a name mid-song.
	o.mu.Lock()
	o.queue = append(o.queue, playing)
	o.mu.Unlock()
	_, tracks := o.QueueTracks()
	if len(tracks) == 0 || tracks[0].Title != "Harbour Lights" || tracks[0].TitleProvisional {
		t.Fatalf("queue listing: %+v", tracks)
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
	if err := o.Buffer.PutTrack(context.Background(), 0, 3, prompting.SongKey(lyrics), track); err != nil {
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
	if len(tracks) != 1 || tracks[0].Title != "Harbour Lights" || tracks[0].TitleProvisional {
		t.Fatalf("queue listing: %+v", tracks)
	}
	// A helper answer that lands afterwards does not get to overrule
	// the listener - not in the pass, and not straight into the file.
	b.PrimeTitle(prompting.SongKey(lyrics), "Steel In The Water", "nu-metal, driving")
	o.retitlePass()
	o.Buffer.SetTitleIfUnnamed(0, base, "Steel In The Water", "nu-metal, driving")
	if got := o.Buffer.List(0)[0]; got.Title != "Harbour Lights" {
		t.Fatalf("a late answer overwrote a typed name: %q", got.Title)
	}

	// The name rides out of the buffer with the song.
	fed, _, _, ok := o.Buffer.NextTrack(context.Background(), 0)
	if !ok || fed.Title != "Harbour Lights" {
		t.Fatalf("fed track = %+v ok=%v", fed, ok)
	}
}

// The case a buffered phone actually hits: it downloaded the song an
// hour ago, the machine has long since fed that file (which deletes it
// from disk), and the listener renames the copy they are hearing. The
// id they have is the file's; the song is in memory.
func TestRenamingFollowsASongOutOfTheBuffer(t *testing.T) {
	b := prompting.NewBuilder(nil, testLogger())
	cfg := testConfig()
	cfg.Buffer.Phased = true
	o := New(cfg, enginetest.NewMock(), b, session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 0, testLogger())

	playing := provisionalTrack("p")
	o.rememberFed("0000000003", playing.ID)
	o.mu.Lock()
	o.cur = newTrackSource(playing, "music")
	o.curTrack = playing
	o.mu.Unlock()

	if ack := o.Retitle(bufTrackPrefix+"0000000003", "Harbour Lights"); !strings.Contains(ack, "Harbour Lights") {
		t.Fatalf("rename ack = %q", ack)
	}
	if playing.Title != "Harbour Lights" || playing.TitleProvisional {
		t.Fatalf("the in-memory song was not renamed: %+v", playing)
	}
	// A file the machine never fed, and never had, is refused gently.
	if ack := o.Retitle(bufTrackPrefix+"0000000009", "Nowhere"); !strings.Contains(ack, "no longer here") {
		t.Fatalf("unknown file ack = %q", ack)
	}
}

// Renaming reaches the banked copy kept for instant starts, so the
// stand-in cannot come back in a later run.
func TestRenamingReachesTheBankedCopy(t *testing.T) {
	b := prompting.NewBuilder(nil, testLogger())
	o := New(testConfig(), enginetest.NewMock(), b, session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	lib := library.New(t.TempDir(), 100, testLogger())
	o.Library = lib

	key := "test-vibe"
	banked := provisionalTrack("b")
	banked.Samples = make([]int16, 9600)
	libID, err := lib.Put(key, banked)
	if err != nil {
		t.Fatal(err)
	}
	// Playing it as filler: the listener's rename must land in the file
	// it came from.
	if ack := o.Retitle(libFillerPrefix+key+"/"+libID, "Harbour Lights"); !strings.Contains(ack, "Harbour Lights") {
		t.Fatalf("rename ack = %q", ack)
	}
	entries := lib.Entries(key)
	if len(entries) != 1 || entries[0].Title != "Harbour Lights" {
		t.Fatalf("banked entry: %+v", entries)
	}
	if entries[0].Subtitle != banked.Subtitle {
		t.Fatalf("rename dropped the genre line: %q", entries[0].Subtitle)
	}

	// And through the in-memory path, for a song banked while it played.
	live := provisionalTrack("l")
	live.Samples = make([]int16, 9600)
	o.mu.Lock()
	o.cur = newTrackSource(live, "music")
	o.curTrack = live
	o.mu.Unlock()
	id2, err := lib.Put(key, live)
	if err != nil {
		t.Fatal(err)
	}
	o.rememberBank(live.ID, bankRef{key: key, id: id2})
	o.Retitle("", "Ash And Anchor")
	for _, e := range lib.Entries(key) {
		if e.ID == id2 && e.Title != "Ash And Anchor" {
			t.Fatalf("banked copy of the playing song: %+v", e)
		}
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
	playing := provisionalTrack("p")
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
