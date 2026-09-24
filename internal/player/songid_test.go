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
	"iar/internal/library"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/songbook"
	"iar/internal/trackbuffer"
)

// The buffer and the library both run the encoder, so these need it.
func skipWithoutFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
}

// idOrchestrator is a phased radio with a disk buffer and a library and
// nothing running, so a test can walk one song through its life by hand.
func idOrchestrator(t *testing.T) (*Orchestrator, *session.Session) {
	t.Helper()
	cfg := testConfig()
	cfg.Buffer.Phased = true
	sess := session.New()
	o := New(cfg, enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), sess, &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 9, testLogger())
	o.Library = library.New(t.TempDir(), 100, 9, testLogger())
	return o, sess
}

// renderSong puts a finished song on disk the way a render does: with
// the id it will keep for the rest of its life, and a record of it in
// the songbook. Returns the file's hash.
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

// listing returns the phone's view of the radio as id -> kind, failing
// the test if any song is offered under two rows.
func listing(t *testing.T, o *Orchestrator) map[string]string {
	t.Helper()
	_, rows := o.QueueTracks()
	seen := map[string]string{}
	for _, r := range rows {
		if prev, dup := seen[r.ID]; dup {
			t.Fatalf("%s is listed twice (as %s and as %s)", r.ID, prev, r.Kind)
		}
		seen[r.ID] = r.Kind
	}
	return seen
}

// bankedAs reports whether the library holds a song under its own id.
func bankedAs(o *Orchestrator, sess *session.Session, id string) bool {
	for _, e := range o.Library.Entries(library.Key(sess)) {
		if e.TrackID == id {
			return true
		}
	}
	return false
}

// The duplicate a phone downloaded: one song, listed under a new name at
// each stop of its life - a file name on disk, a fresh id in the play
// queue, a library id as filler - and taken again under every one of
// them. A phone that had flushed and refilled held 34 songs of a radio
// with 18.
func TestASongIsListedOnceUnderOneIDFromDiskToTheLibrary(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, sess := idOrchestrator(t)
	ids := []string{"t-1790000000000-0001", "t-1790000000000-0002"}
	for i, id := range ids {
		renderSong(t, o, i+1, id)
	}

	onDisk := listing(t, o)
	for _, id := range ids {
		if onDisk[id] == "" {
			t.Fatalf("%s is not listed under its own id while it waits on disk: %v", id, onDisk)
		}
	}

	// The feeder takes both into the play queue and banks them.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.feedLoop(ctx)
	waitFor(t, 20*time.Second, "both songs fed and banked", func() bool {
		o.mu.Lock()
		n := len(o.queue)
		o.mu.Unlock()
		return n == len(ids) && bankedAs(o, sess, ids[0]) && bankedAs(o, sess, ids[1])
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
	for id := range fed {
		if strings.HasPrefix(id, bufTrackPrefix) || strings.HasPrefix(id, libFillerPrefix) {
			t.Fatalf("a song went out under a name that belongs to one stop of its life: %v", fed)
		}
	}

	// The speakers play them, the way the mixer does: out of the queue.
	// Banked, they are now offered as filler - under the ids they had all
	// along, which a phone that downloaded them already holds.
	o.mu.Lock()
	o.prevTrack, o.curTrack = o.queue[0], o.queue[1]
	o.queue = nil
	o.mu.Unlock()
	played := listing(t, o)
	for _, id := range ids {
		if played[id] != "library" {
			t.Fatalf("%s, played and banked, is not offered as filler under its own id: %v", id, played)
		}
	}
	for id := range played {
		if strings.HasPrefix(id, libFillerPrefix) {
			t.Fatalf("a banked song went out under a library name of its own: %v", played)
		}
	}
}

// Songs banked with no id of their own - starter tracks, and songs banked
// before songs carried one - are still offered under the library's name
// for them, and still save through it. That name has a slash in it, and
// the audio comes out of the library rather than the stream.
func TestAnOldBankedSongIsOfferedAndSavedUnderItsLibraryName(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, sess := idOrchestrator(t)
	o.SnippetsDir = t.TempDir()
	key := library.Key(sess)
	starter := &engine.Track{Prompt: "a starter track", Lyrics: "[Verse]\nfrom the setup run",
		Samples: make([]int16, audio.SampleRate*audio.Channels/2)}
	fileID, err := o.Library.Put(context.Background(), key, starter)
	if err != nil {
		t.Fatal(err)
	}
	name := libFillerPrefix + key + "/" + fileID
	if rows := listing(t, o); rows[name] != "library" {
		t.Fatalf("the starter track is not offered as %q: %v", name, rows)
	}
	if ack := o.SaveSnippet(name, "starter"); !strings.Contains(ack, "saving this track to starter/") {
		t.Fatalf("save ack = %q", ack)
	}
	waitFor(t, 20*time.Second, "the snippet on disk", func() bool {
		m, _ := filepath.Glob(filepath.Join(o.SnippetsDir, "starter", "*.mp3"))
		return len(m) == 1
	})
}

// A fresh radio offers yesterday's songs as filler under their own ids,
// and knows nothing in memory about where they were banked. A phone
// plays one and its listener renames it: the library still has it.
func TestRenamingABankedSongAfterARestart(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, sess := idOrchestrator(t)
	const id = "t-1790000000000-0006"
	yesterday := &engine.Track{ID: id, Prompt: "a song from yesterday",
		Samples: make([]int16, audio.SampleRate*audio.Channels/2)}
	if _, err := o.Library.Put(context.Background(), library.Key(sess), yesterday); err != nil {
		t.Fatal(err)
	}
	if rows := listing(t, o); rows[id] != "library" {
		t.Fatalf("yesterday's song is not offered under its own id: %v", rows)
	}
	if ack := o.Retitle(id, "Harbour Lights"); !strings.Contains(ack, "Harbour Lights") {
		t.Fatalf("rename ack = %q", ack)
	}
	es := o.Library.Entries(library.Key(sess))
	if len(es) != 1 || es[0].Title != "Harbour Lights" {
		t.Fatalf("the banked song was not renamed: %+v", es)
	}
}

// File names are reused: once a buffer empties, the count starts over,
// so a phone can hold an old song under a file name that a brand-new
// song now occupies. That name must go on meaning the old song - saving
// or renaming it must never reach the new one sitting in its file.
func TestAReusedFileNameNeverNamesANewSong(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, sess := idOrchestrator(t)
	const legacy = bufTrackPrefix + "e00000000-00000003"
	old := &engine.Track{ID: legacy, Prompt: "the old song",
		Samples: make([]int16, audio.SampleRate*audio.Channels/2)}
	if _, err := o.Library.Put(context.Background(), library.Key(sess), old); err != nil {
		t.Fatal(err)
	}
	renderSong(t, o, 3, "t-1790000000000-0008") // lands in e00000000-00000003

	got, ok := o.TrackData(legacy)
	if !ok || got.Prompt != "the old song" {
		t.Fatalf("the old file name resolved to %q (ok=%v)", got.Prompt, ok)
	}
	if ack := o.Retitle(legacy, "Renamed"); !strings.Contains(ack, "Renamed") {
		t.Fatalf("rename ack = %q", ack)
	}
	if title := o.Buffer.List(0)[0].Title; title == "Renamed" {
		t.Fatal("renaming the old song renamed the new one sitting in its file")
	}
}

// Saving the song a phone is still playing, long after the speakers
// played it: the audio has been in the library the whole time, and the
// radio used to answer "no longer available" because it only looked in
// memory. It must work after a restart too, when nothing remembers where
// the song was banked.
func TestSavingASongTheRadioHasPlayedPast(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, sess := idOrchestrator(t)
	o.SnippetsDir = t.TempDir()
	const id = "t-1790000000000-0003"
	renderSong(t, o, 1, id)

	ctx, cancel := context.WithCancel(context.Background())
	go o.feedLoop(ctx)
	waitFor(t, 20*time.Second, "the song fed and banked", func() bool { return bankedAs(o, sess, id) })
	cancel()

	// The radio plays it and moves on: nothing in memory holds it now.
	o.mu.Lock()
	o.queue = nil
	o.curTrack, o.prevTrack, o.lastGood = nil, nil, nil
	o.mu.Unlock()
	if _, ok := o.TrackData(id); !ok {
		t.Fatal("a song the radio has played past is out of reach, though the library holds it")
	}

	// A restart: the remembered locations are gone, the library is not.
	o.mu.Lock()
	o.bankRefs = map[string]bankRef{}
	o.mu.Unlock()
	got, ok := o.TrackData(id)
	if !ok {
		t.Fatal("after a restart the played-past song cannot be found")
	}
	if got.ID != id {
		t.Fatalf("the song came back as %q, not the id it was asked for", got.ID)
	}

	if ack := o.SaveSnippet(id, "later"); !strings.Contains(ack, "saving this track to later/") {
		t.Fatalf("save ack = %q", ack)
	}
	waitFor(t, 20*time.Second, "the snippet on disk", func() bool {
		m, _ := filepath.Glob(filepath.Join(o.SnippetsDir, "later", "*.mp3"))
		return len(m) == 1
	})
}

// A phone that downloaded ahead of the speakers renames the song it is
// playing, which is still a file waiting on disk - listed under the
// song's own id now, not its file name.
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

// An instant start plays a banked song, and it is the same song it was:
// a phone that already has it does not take it again under a new name.
func TestAnInstantStartKeepsTheBankedSongsID(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, sess := idOrchestrator(t)
	const id = "t-1790000000000-0005"
	banked := &engine.Track{ID: id, Prompt: "an old favourite",
		Samples: make([]int16, audio.SampleRate*audio.Channels/2)}
	if _, err := o.Library.Put(context.Background(), library.Key(sess), banked); err != nil {
		t.Fatal(err)
	}
	o.seedFromLibrary()

	o.mu.Lock()
	n := len(o.queue)
	var got string
	if n > 0 {
		got = o.queue[0].ID
	}
	o.mu.Unlock()
	if n != 1 || got != id {
		t.Fatalf("the instant start queued %d song(s), first as %q, want %q", n, got, id)
	}
	if rows := listing(t, o); len(rows) != 1 {
		t.Fatalf("one banked song is offered as %d rows: %v", len(rows), rows)
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
