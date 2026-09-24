package player

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"iar/internal/audio"
	"iar/internal/engine"
	"iar/internal/engine/enginetest"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/songbook"
	"iar/internal/trackbuffer"
)

// The store runs the encoder, so these need it.
func skipWithoutFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
}

// idOrchestrator is a phased radio with a store and nothing running, so
// a test can walk one song through its life by hand.
func idOrchestrator(t *testing.T) (*Orchestrator, *session.Session) {
	t.Helper()
	cfg := testConfig()
	sess := session.New()
	o := New(cfg, enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), sess, &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 9, testLogger())
	return o, sess
}

// renderSong puts a finished song in the store the way a render does:
// with the id it will keep for the rest of its life, and a record of it
// in the songbook. Returns the file's hash.
func renderSong(t *testing.T, o *Orchestrator, seq int, id string) string {
	t.Helper()
	tr := &engine.Track{
		ID:      id,
		Prompt:  "a song called " + id,
		Lyrics:  "[Verse]\nthe words of " + id,
		Samples: make([]int16, audio.SampleRate*audio.Channels/2),
	}
	hash, err := o.Buffer.PutTrack(context.Background(), 0, seq, tr)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Songbook.Record(songbook.Song{ID: id, Hash: hash, Title: tr.Title, Prompt: tr.Prompt,
		Lyrics: tr.Lyrics, Seconds: tr.Duration().Seconds(), Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return hash
}

// listing returns the phone's view of the radio as id -> "taken" or
// "free", failing the test if any song is offered under two rows.
func listing(t *testing.T, o *Orchestrator) map[string]string {
	t.Helper()
	_, rows := o.QueueTracks()
	seen := map[string]string{}
	for _, r := range rows {
		kind := "free"
		if r.Taken {
			kind = "taken"
		}
		if prev, dup := seen[r.ID]; dup {
			t.Fatalf("%s is listed twice (as %s and as %s)", r.ID, prev, kind)
		}
		seen[r.ID] = kind
	}
	return seen
}

// One song, one id, from the store into the play queue and out the other
// side: a phone that keeps its songs by name must never see the same
// song under a second name at any stop of its life.
func TestASongIsListedOnceUnderOneIDFromTheStoreToPlayed(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	ids := []string{"t-1790000000000-0001", "t-1790000000000-0002"}
	for i, id := range ids {
		renderSong(t, o, i+1, id)
	}

	inStore := listing(t, o)
	for _, id := range ids {
		if inStore[id] != "free" {
			t.Fatalf("%s is not listed as free for the taking while it waits in the store: %v", id, inStore)
		}
	}

	// The feeder takes both into the play queue.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.feedLoop(ctx)
	waitFor(t, 20*time.Second, "both songs fed", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return len(o.queue) == len(ids)
	})
	cancel()

	o.mu.Lock()
	for i, tr := range o.queue {
		if tr.ID != ids[i] {
			o.mu.Unlock()
			t.Fatalf("song %d entered the play queue as %q, not %q", i, tr.ID, ids[i])
		}
	}
	o.mu.Unlock()

	fed := listing(t, o)
	if len(fed) != len(ids) {
		t.Fatalf("two songs are offered as %d rows: %v", len(fed), fed)
	}
	for id, kind := range fed {
		if strings.HasPrefix(id, bufTrackPrefix) {
			t.Fatalf("a song went out under a name that belongs to one stop of its life: %v", fed)
		}
		if kind != "taken" {
			t.Fatalf("%s was fed but is listed as %s: %v", id, kind, fed)
		}
	}

	// The speakers play them, the way the mixer does: out of the queue.
	// Taken, they are still listed - under the ids they had all along,
	// which a phone that downloaded them already holds.
	o.mu.Lock()
	o.prevTrack, o.curTrack = o.queue[0], o.queue[1]
	o.queue = nil
	o.mu.Unlock()
	played := listing(t, o)
	for _, id := range ids {
		if played[id] != "taken" {
			t.Fatalf("%s, played and kept, is not listed under its own id: %v", id, played)
		}
	}
}

// A song rendered in an earlier run and taken there: a fresh radio over
// the same store knows nothing about it in memory, and a phone playing
// it renames it. The store still has it.
func TestRenamingATakenSongAfterARestart(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	const id = "t-1790000000000-0006"
	renderSong(t, o, 1, id)
	o.Buffer.Take(0, "e00000000-00000001")

	// A restart: a new radio over the same store directory.
	again := New(o.cfg, enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	again.Buffer = trackbuffer.New(o.Buffer.Dir(), 9, testLogger())
	if rows := listing(t, again); rows[id] != "taken" {
		t.Fatalf("the taken song is not listed under its own id: %v", rows)
	}
	if ack := again.Retitle(id, "Harbour Lights"); !strings.Contains(ack, "Harbour Lights") {
		t.Fatalf("rename ack = %q", ack)
	}
	if got := again.Buffer.List(0)[0].Title; got != "Harbour Lights" {
		t.Fatalf("the song in the store is still called %q", got)
	}
}

// File names are reused: once a store empties, the count starts over,
// so a phone can hold an old song under a file name that a brand-new
// song now occupies. That name must go on meaning the old song - saving
// or renaming it must never reach the new one sitting in its file.
func TestAReusedFileNameNeverNamesANewSong(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	const legacy = bufTrackPrefix + "e00000000-00000003"
	renderSong(t, o, 3, "t-1790000000000-0008") // lands in e00000000-00000003

	if _, ok := o.TrackData(legacy); ok {
		t.Fatal("the old file name resolved to the new song sitting in its file")
	}
	if ack := o.Retitle(legacy, "Renamed"); !strings.Contains(ack, "no longer here") {
		t.Fatalf("rename ack = %q", ack)
	}
	if title := o.Buffer.List(0)[0].Title; title == "Renamed" {
		t.Fatal("renaming the old song renamed the new one sitting in its file")
	}
}

// Saving the song a phone is still playing, long after the speakers
// played it: the audio has been in the store the whole time, taken but
// kept. It must work after a restart too, when nothing in memory
// remembers the song.
func TestSavingASongTheRadioHasPlayedPast(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	o.SnippetsDir = t.TempDir()
	const id = "t-1790000000000-0003"
	renderSong(t, o, 1, id)

	ctx, cancel := context.WithCancel(context.Background())
	go o.feedLoop(ctx)
	waitFor(t, 20*time.Second, "the song fed", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return len(o.queue) == 1
	})
	cancel()

	// The radio plays it and moves on: nothing in memory holds it now.
	o.mu.Lock()
	o.queue = nil
	o.curTrack, o.prevTrack, o.lastGood = nil, nil, nil
	o.mu.Unlock()
	if _, ok := o.TrackData(id); !ok {
		t.Fatal("a song the radio has played past is out of reach, though the store keeps it")
	}

	// A restart: a new radio over the same store and book.
	again := New(o.cfg, enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	again.Buffer = trackbuffer.New(o.Buffer.Dir(), 9, testLogger())
	again.Songbook = o.Songbook
	again.SnippetsDir = o.SnippetsDir
	got, ok := again.TrackData(id)
	if !ok {
		t.Fatal("after a restart the played-past song cannot be found")
	}
	if got.ID != id {
		t.Fatalf("the song came back as %q, not the id it was asked for", got.ID)
	}

	if ack := again.SaveSnippet(id, "later"); !strings.Contains(ack, "saving this track to later/") {
		t.Fatalf("save ack = %q", ack)
	}
	waitFor(t, 20*time.Second, "the snippet on disk", func() bool {
		m, _ := filepath.Glob(filepath.Join(again.SnippetsDir, "later", "*.mp3"))
		return len(m) == 1
	})
}

// A phone that downloaded ahead of the speakers renames the song it is
// playing, which is still a file in the store - listed under the song's
// own id now, not its file name.
func TestRenamingASongOnDiskByItsOwnID(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	const id = "t-1790000000000-0004"
	renderSong(t, o, 3, id)

	if ack := o.Retitle(id, "Harbour Lights"); !strings.Contains(ack, "Harbour Lights") {
		t.Fatalf("rename ack = %q", ack)
	}
	if got := o.Buffer.List(0)[0].Title; got != "Harbour Lights" {
		t.Fatalf("the file on disk is still called %q", got)
	}
}

// A phone runs ahead of the speakers, so the song it is playing is very
// often the one the machine is just fading in: off the play queue, not
// yet the current song. For the length of the crossfade it used to be in
// no list the radio looked in, and a save or rename of it failed with
// "no longer here" - a few seconds' window that a skip opens exactly
// when a listener is most likely to be reaching for the pencil.
func TestASongBeingFadedInCanBeFoundAndRenamed(t *testing.T) {
	o, _ := idOrchestrator(t)
	fading := &engine.Track{ID: "t-1790000000000-0010", Prompt: "the song fading in",
		Samples: make([]int16, 2*audio.SampleRate*audio.Channels)}
	o.mu.Lock()
	o.queue = []*engine.Track{fading}
	o.mu.Unlock()

	// A full output holds the mixer on the fade's first write: mid-fade,
	// with the song off the queue and not yet current.
	if _, err := o.ring.Write(make([]byte, 2*audio.BytesPerSecond)); err != nil {
		t.Fatal(err)
	}
	var cur source = silenceSource{}
	next := o.chooseNext(cur)
	if next == nil {
		t.Fatal("the mixer chose nothing to fade in")
	}
	faded := make(chan struct{})
	go func() {
		defer close(faded)
		o.crossfade(context.Background(), cur, next, audio.SampleRate/2, audio.SampleRate/10)
	}()
	waitFor(t, 5*time.Second, "the fade under way", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.incoming == fading && len(o.queue) == 0
	})

	if _, ok := o.TrackData(fading.ID); !ok {
		t.Fatal("the song being faded in cannot be found, so it cannot be saved")
	}
	if ack := o.Retitle(fading.ID, "Harbour Lights"); !strings.Contains(ack, "Harbour Lights") {
		t.Fatalf("renaming the song being faded in: ack = %q", ack)
	}

	// Let the fade finish: the song is current, and nothing is incoming.
	drain := make([]byte, audio.BytesPerSecond/10)
	deadline := time.After(10 * time.Second)
	for waiting := true; waiting; {
		select {
		case <-faded:
			waiting = false
		case <-deadline:
			t.Fatal("the fade never finished")
		default:
			o.ring.Read(drain)
			time.Sleep(5 * time.Millisecond)
		}
	}
	o.mu.Lock()
	now, still := o.curTrack, o.incoming
	o.mu.Unlock()
	if now != fading || still != nil {
		t.Fatalf("after the fade: current %v, incoming %v", now, still)
	}
	if fading.Title != "Harbour Lights" {
		t.Fatalf("the rename did not reach the song: %q", fading.Title)
	}
}
