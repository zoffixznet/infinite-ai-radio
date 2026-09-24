package player

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"iar/internal/export"
)

// holds lists the files queued saves are keeping their sources under.
func holds(t *testing.T, o *Orchestrator) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(o.SnippetsDir, ".hold-*"))
	return m
}

// gatedSave queues a save of a rendered song whose write waits on gate
// before it runs, so a test can hold the worker at a chosen point. The
// save is otherwise what SaveSnippet makes of a song in the store.
func gatedSave(t *testing.T, o *Orchestrator, id, tag string, gate <-chan struct{}) string {
	t.Helper()
	src, ok := o.TrackFile(id)
	if !ok {
		t.Fatalf("%s has no file in the store", id)
	}
	song, _ := o.Songbook.ByID(id)
	held, release, err := o.holdSource(src)
	if err != nil {
		t.Fatal(err)
	}
	return o.runSave(tag, &saveJob{
		song:    song,
		release: release,
		write: func(ctx context.Context, dst string, opts export.MP3Options) error {
			<-gate
			return export.CopyMP3(ctx, held, dst, opts)
		},
	})
}

// workerIdle reports that the save worker has written everything and
// gone.
func workerIdle(o *Orchestrator) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.saveWorker
}

// lockedBuffer is a log sink written from the worker and read by the
// test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A save waits its turn behind another, and while it waits the store
// lets its song go - a restart, a steer, a trim of taken songs. The
// device was told the song was on its way, and it is: the save holds
// the file from the moment it is queued.
func TestAQueuedSaveKeepsItsSongWhenTheStoreLetsItGo(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	o.SnippetsDir = t.TempDir()
	const a, b = "t-1790000000000-0061", "t-1790000000000-0062"
	renderSong(t, o, 1, a)
	renderSong(t, o, 2, b)
	gate := make(chan struct{})
	if ack := gatedSave(t, o, a, "road", gate); !strings.HasPrefix(ack, "saving this track to road/") {
		t.Fatalf("first save ack = %q", ack)
	}
	if ack := o.SaveSnippet(b, "road"); !strings.HasSuffix(ack, ", behind 1 other save") {
		t.Fatalf("second save ack = %q, want it waiting behind the first", ack)
	}
	if dropped := o.Buffer.DropAll(); dropped == 0 {
		t.Fatal("the store had nothing to drop")
	}
	if _, ok := o.TrackFile(b); ok {
		t.Fatal("the store still serves the song it dropped")
	}
	close(gate)
	allSnippets(t, o, "road", a, b)
	waitFor(t, 5*time.Second, "the worker to finish", func() bool { return workerIdle(o) })
	if h := holds(t, o); len(h) != 0 {
		t.Fatalf("holding files left behind: %v", h)
	}
}

// A second save of a song lands exactly as the song's write ends: it
// looked in the book before the save was recorded, and reaches the
// queue after the worker has stopped calling the song pending. One
// file, still.
func TestASaveArrivingAsTheSameSongsWriteEndsIsNotASecondFile(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	o.SnippetsDir = t.TempDir()
	const a = "t-1790000000000-0071"
	renderSong(t, o, 1, a)
	gate := make(chan struct{})
	if ack := gatedSave(t, o, a, "road", gate); !strings.HasPrefix(ack, "saving this track to road/") {
		t.Fatalf("save ack = %q", ack)
	}
	waitFor(t, 5*time.Second, "the worker to take the save", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.saveNow != nil
	})
	// The second save reads the book - nothing saved yet - and then
	// waits for the lock, which the test holds until the write is done
	// and recorded. The worker joins the wait behind it, and the two
	// then take their turns in that order: the second save's look at
	// the queue comes after the worker has emptied it.
	o.mu.Lock()
	acks := make(chan string, 1)
	go func() { acks <- o.SaveSnippet(a, "road") }()
	time.Sleep(50 * time.Millisecond)
	close(gate)
	waitFor(t, 10*time.Second, "the save recorded", func() bool { return o.Songbook.SavedPath(a) != "" })
	time.Sleep(20 * time.Millisecond)
	o.mu.Unlock()
	ack := <-acks
	if !strings.HasPrefix(ack, "already sav") {
		t.Fatalf("a save landing as the same song's write ended = %q, want already saved or already saving", ack)
	}
	waitFor(t, 5*time.Second, "the worker to finish", func() bool { return workerIdle(o) })
	files, _ := filepath.Glob(filepath.Join(o.SnippetsDir, "road", "*.mp3"))
	if len(files) != 1 {
		t.Fatalf("one song became %d files: %v", len(files), files)
	}
	if got := o.Songbook.SavedPath(a); got != files[0] {
		t.Fatalf("the book says the song was saved to %q, the file is %q", got, files[0])
	}
	if h := holds(t, o); len(h) != 0 {
		t.Fatalf("holding files left behind: %v", h)
	}
}

// The radio is closed with saves still waiting. The one being written
// is finished; the rest are dropped, said so in the log, and let go of;
// a save arriving from then on is told the radio is shutting down, from
// the store and from a device's copy alike.
func TestClosingTheRadioDropsTheSavesStillWaitingAndSaysSo(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	o.SnippetsDir = t.TempDir()
	logged := &lockedBuffer{}
	o.log = slog.New(slog.NewTextHandler(logged, nil))
	const a, b, c, d = "t-1790000000000-0081", "t-1790000000000-0082", "t-1790000000000-0083", "t-1790000000000-0084"
	renderSong(t, o, 1, a)
	renderSong(t, o, 2, b)
	hash := renderSong(t, o, 3, c)
	renderSong(t, o, 4, d)
	src, _ := o.TrackFile(c)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	if ack := gatedSave(t, o, a, "road", gate); !strings.HasPrefix(ack, "saving this track to road/") {
		t.Fatalf("first save ack = %q", ack)
	}
	for _, id := range []string{b, c} {
		if ack := o.SaveSnippet(id, "road"); !strings.HasPrefix(ack, "saving this track to road/") {
			t.Fatalf("save of %s ack = %q", id, ack)
		}
	}
	// The first save is in the worker's hands - held at its write -
	// before the close begins; a close that came first would find all
	// three still waiting and drop them all.
	waitFor(t, 5*time.Second, "the worker to take the first save", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.saveNow != nil
	})
	closed := make(chan struct{})
	go func() {
		o.Close()
		close(closed)
	}()
	waitFor(t, 5*time.Second, "the close to begin", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.saveClosed
	})
	// Close is waiting for the write in flight; a save arriving now has
	// nobody to write it.
	if ack := o.SaveSnippet(d, "road"); ack != AckClosing {
		t.Fatalf("a save during the close = %q, want %q", ack, AckClosing)
	}
	close(gate)
	select {
	case <-closed:
	case <-time.After(20 * time.Second):
		t.Fatal("Close did not return once the write in flight was done")
	}
	files, _ := filepath.Glob(filepath.Join(o.SnippetsDir, "road", "*.mp3"))
	if len(files) != 1 {
		t.Fatalf("%d files written across the close, want the one in flight: %v\n%s", len(files), files, logged.String())
	}
	if o.Songbook.SavedPath(a) != files[0] {
		t.Fatalf("the save in flight was not recorded as %q", files[0])
	}
	for _, id := range []string{b, c, d} {
		if p := o.Songbook.SavedPath(id); p != "" {
			t.Fatalf("%s is recorded as saved to %q after being dropped", id, p)
		}
	}
	if n := strings.Count(logged.String(), "event=snippet_dropped"); n != 2 {
		t.Fatalf("%d saves logged as dropped, want 2:\n%s", n, logged.String())
	}
	if h := holds(t, o); len(h) != 0 {
		t.Fatalf("dropped saves left their holding files behind: %v", h)
	}
	if ack := o.SaveSnippet(b, "road"); ack != AckClosing {
		t.Fatalf("a save after the close = %q, want %q", ack, AckClosing)
	}
	if ack := o.SaveUpload(hash, "road", bytes.NewReader(data)); ack != AckClosing {
		t.Fatalf("an uploaded copy after the close = %q, want %q", ack, AckClosing)
	}
	if m, _ := filepath.Glob(filepath.Join(o.SnippetsDir, ".upload-*")); len(m) != 0 {
		t.Fatalf("the refused upload left its temporary file behind: %v", m)
	}
}

// A device's copy of a song is being written when a save of the same
// song arrives from the radio's own store - a second device, or the
// terminal. The store's save is told the song is already on its way,
// and one file results.
func TestAStoreSaveDuringAnUploadOfTheSameSongIsTheSameSave(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	o.SnippetsDir = t.TempDir()
	const c = "t-1790000000000-0091"
	hash := renderSong(t, o, 1, c)
	src, _ := o.TrackFile(c)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	uploaded := make(chan string, 1)
	go func() { uploaded <- o.SaveUpload(hash, "road", bytes.NewReader(data)) }()
	waitFor(t, 5*time.Second, "the upload to speak for the song", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.saveUploads[c]
	})
	// From here until the upload returns, the song is spoken for; the
	// store still has it, so a save from there is what a second device
	// would ask for.
	var acks []string
	var upAck string
hammer:
	for {
		select {
		case upAck = <-uploaded:
			break hammer
		default:
			acks = append(acks, o.SaveSnippet(c, "road"))
		}
	}
	if !strings.HasPrefix(upAck, "track saved: road/") {
		t.Fatalf("upload ack = %q", upAck)
	}
	if len(acks) == 0 {
		t.Fatal("no save from the store landed while the copy was being written")
	}
	for _, ack := range acks {
		if !strings.HasPrefix(ack, "already saving:") && !strings.HasPrefix(ack, "already saved") {
			t.Fatalf("a save from the store while the copy was being written = %q, want already saving or already saved", ack)
		}
	}
	waitFor(t, 5*time.Second, "the worker to finish", func() bool { return workerIdle(o) })
	files, _ := filepath.Glob(filepath.Join(o.SnippetsDir, "road", "*.mp3"))
	if len(files) != 1 {
		t.Fatalf("one song became %d files: %v", len(files), files)
	}
	if got := o.Songbook.SavedPath(c); got != files[0] {
		t.Fatalf("the book says the song was saved to %q, the file is %q", got, files[0])
	}
	if h := holds(t, o); len(h) != 0 {
		t.Fatalf("holding files left behind: %v", h)
	}
}
