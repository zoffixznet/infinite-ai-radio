package player

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"iar/internal/engine"
	"iar/internal/export"
	"iar/internal/prompting"
	"iar/internal/snippets"
	"iar/internal/songbook"
)

// AckSendCopy answers a save of a song the radio made but no longer
// holds: the device asking has a copy, and can send it (SaveUpload).
const AckSendCopy = "the radio no longer has that song; send the copy on this device"

// ackAlreadySaved answers a repeat save: a success, never a second file.
const ackAlreadySaved = "already saved: that track is in your snippets"

// ackAlreadySaving answers a save of a song whose save is queued or
// being written this moment: the same success, and the same one file.
const ackAlreadySaving = "already saving: that track is on its way to your snippets"

// AckClosing answers a save that arrives as the radio shuts down, when
// nobody is left to write it. The radio will be back; the remote sends
// this one out as the service being away, so a phone keeps the save
// queued rather than taking it for a final no.
const AckClosing = "the radio is shutting down; that track was not saved"

// SaveSnippet captures a generated track as an MP3 in the snippets
// folder, under the directory for tag (empty means untagged). which
// selects the track: empty for the currently playing one, "prev" for
// the one before it, or a track id (a buffered phone saves what its
// driver is hearing, not what the speakers play). A song still on disk
// is copied as it is, retagged; one only in memory is encoded from its
// PCM. Either way saving is instant for playback: the write runs in
// the background and completion is reported through Events. Saving an
// already-saved track is a success no-op.
func (o *Orchestrator) SaveSnippet(which, tag string) string {
	var track *engine.Track
	if isTrackID(which) {
		// An already-saved id is a no-op even after its audio is gone.
		if o.Songbook.SavedPath(which) != "" {
			return ackAlreadySaved
		}
		// Still on disk: the saved copy is the file itself, not a second
		// encode of it. The file is held under a name of the save's own
		// first: a save waits its turn behind others, and the store
		// drops files on its own schedule meanwhile.
		if path, ok := o.TrackFile(which); ok {
			if song, known := o.Songbook.ByID(which); known {
				held, release, err := o.holdSource(path)
				if err == nil {
					return o.runSave(tag, &saveJob{
						song:    song,
						release: release,
						write: func(ctx context.Context, dst string, opts export.MP3Options) error {
							return export.CopyMP3(ctx, held, dst, opts)
						},
					})
				}
				// Gone between the look and the hold, or nowhere to
				// hold it: the song's audio may still be in memory, and
				// the device asking may still have its copy.
				o.log.Warn("stored song could not be held for its save", "event", "snippet_hold_failed",
					"id", which, "error", err.Error())
			}
		}
		t, ok := o.TrackData(which)
		if !ok {
			// Gone from here, but made here: the phone still playing
			// it has the bytes, and the book knows what they are.
			if song, known := o.Songbook.ByID(which); known && song.Hash != "" {
				return AckSendCopy
			}
			return "that track is no longer available to save"
		}
		if t.ID == "" {
			t.ID = which // a song rendered before songs carried an id of their own
		}
		track = t
	}
	if track == nil {
		o.mu.Lock()
		switch which {
		case "prev", "previous", "last":
			track = o.prevTrack
		default:
			track = o.curTrack
		}
		o.mu.Unlock()
	}
	if track == nil {
		if which == "prev" {
			return "no previous track to save yet"
		}
		return "nothing to save yet: no generated track is playing"
	}
	if track.ID != "" && o.Songbook.SavedPath(track.ID) != "" {
		return ackAlreadySaved
	}
	// Copy the display fields under the lock: a listener may be
	// renaming this very track. The save then uses one consistent name
	// throughout, whichever side of the rename it caught.
	o.mu.Lock()
	song := songbook.Song{
		ID: track.ID, Title: track.Title, Subtitle: track.Subtitle, Prompt: track.Prompt,
		Lyrics: track.Lyrics, Language: track.Spec.VocalLanguage,
	}
	o.mu.Unlock()
	return o.runSave(tag, &saveJob{
		song: song,
		live: track,
		write: func(ctx context.Context, dst string, opts export.MP3Options) error {
			return export.EncodeMP3(ctx, track.Samples, dst, opts)
		},
	})
}

// saveJob is one save: what the file is called and how it is written.
type saveJob struct {
	// song carries the name, words and language the file is saved with.
	song songbook.Song
	// live is the in-memory track, when there is one, whose title a
	// listener may change while the file is being written.
	live *engine.Track
	// write produces the MP3 at dst under opts' tags.
	write func(ctx context.Context, dst string, opts export.MP3Options) error
	// release lets go of whatever write reads from, once the save is
	// written, refused or dropped. Nil when there is nothing to let go.
	release func()
	// path, shown and opts are the file the save was promised - chosen
	// when it joined the queue, so the acknowledgment names it.
	path, shown string
	opts        export.MP3Options
}

// done lets go of the job's source, if it had one to hold.
func (j *saveJob) done() {
	if j.release != nil {
		j.release()
	}
}

// holdSource keeps a song's stored file readable for a save that will
// wait its turn. The store lets files go on its own schedule - a steer
// or a restart empties it, a trim removes the oldest taken songs - and
// a save queued behind others would otherwise find its source gone when
// its turn came, after the device asking was told the song was on its
// way. A hard link shares the bytes for free under a name of the
// save's own, in the snippets folder, where nothing lists a hidden
// file; when the store and the snippets live on different filesystems
// the bytes are copied instead. Returns the held file and its release.
func (o *Orchestrator) holdSource(src string) (held string, release func(), err error) {
	if err := os.MkdirAll(o.SnippetsDir, 0o755); err != nil {
		return "", nil, err
	}
	tmp, err := os.CreateTemp(o.SnippetsDir, ".hold-*.mp3")
	if err != nil {
		return "", nil, err
	}
	held = tmp.Name()
	tmp.Close()
	release = func() { os.Remove(held) }
	os.Remove(held)
	if os.Link(src, held) == nil {
		return held, release, nil
	}
	if err := copyFile(src, held); err != nil {
		release()
		return "", nil, err
	}
	return held, release, nil
}

// sweepSaveScraps removes what a previous run's saves left in the
// snippets folder when it died mid-way: the files queued saves held
// their songs under, and copies half received from a device. Nothing
// lists them, but each is a song's worth of disk.
func (o *Orchestrator) sweepSaveScraps() {
	for _, pattern := range []string{".hold-*.mp3", ".upload-*.mp3"} {
		scraps, _ := filepath.Glob(filepath.Join(o.SnippetsDir, pattern))
		for _, p := range scraps {
			if os.Remove(p) == nil {
				o.log.Info("scrap of an unfinished save removed", "event", "snippet_scrap_removed", "path", p)
			}
		}
	}
}

// copyFile writes src's bytes to dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// runSave puts a save in the queue and answers straight away. Saves are
// written one after another by a worker that runs while the queue has
// anything in it: a phone back on the network after a tunnel delivers
// the saves it kept in a burst, and each one is kept, in order. A song
// that already has a save on the way gets the same success and no
// second file.
func (o *Orchestrator) runSave(tag string, job *saveJob) string {
	ctx := o.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.saveClosed || ctx.Err() != nil {
		job.done()
		return AckClosing
	}
	// The caller looked in the book before coming here, without the
	// lock; the worker marks a song saved and then, under the lock,
	// stops calling it pending. Looked at again here, one of the two
	// is always true for a song already dealt with - a save arriving
	// as the same song's write ends would otherwise be a second file.
	if job.song.ID != "" && o.Songbook.SavedPath(job.song.ID) != "" {
		job.done()
		return ackAlreadySaved
	}
	if o.savePendingLocked(job.song.ID) {
		job.done()
		return ackAlreadySaving
	}
	job.path, job.shown, job.opts = o.claimTargetLocked(tag, job.song)
	ahead := len(o.saveQueue)
	if o.saveNow != nil {
		ahead++
	}
	o.saveQueue = append(o.saveQueue, job)
	if !o.saveWorker {
		o.saveWorker = true
		o.saveWG.Add(1)
		go o.saveLoop(ctx)
	}
	ack := "saving this track to " + job.shown
	switch ahead {
	case 0:
	case 1:
		ack += ", behind 1 other save"
	default:
		ack += fmt.Sprintf(", behind %d other saves", ahead)
	}
	return ack
}

// savePendingLocked reports whether a save of the song is queued or
// being written - by the worker or as a copy sent from a device. A
// song without an id cannot be told from another, so it never is.
// Callers hold o.mu.
func (o *Orchestrator) savePendingLocked(id string) bool {
	if id == "" {
		return false
	}
	if o.saveUploads[id] || (o.saveNow != nil && o.saveNow.song.ID == id) {
		return true
	}
	for _, j := range o.saveQueue {
		if j.song.ID == id {
			return true
		}
	}
	return false
}

// saveLoop writes queued saves one after another until the queue is
// empty or the radio stops. ctx is the radio's run context: stopping
// the radio stops the write in flight, and whatever still waited is
// dropped and said so in the log rather than left half written.
func (o *Orchestrator) saveLoop(ctx context.Context) {
	defer o.saveWG.Done()
	for {
		o.mu.Lock()
		if o.saveNow != nil {
			delete(o.savePaths, o.saveNow.path)
			o.saveNow = nil
		}
		if o.saveClosed || ctx.Err() != nil || len(o.saveQueue) == 0 {
			dropped := o.saveQueue
			o.saveQueue = nil
			for _, j := range dropped {
				delete(o.savePaths, j.path)
			}
			o.saveWorker = false
			o.mu.Unlock()
			for _, j := range dropped {
				j.done()
				o.log.Warn("save dropped at shutdown", "event", "snippet_dropped", "id", j.song.ID, "path", j.path)
			}
			return
		}
		job := o.saveQueue[0]
		o.saveQueue = o.saveQueue[1:]
		o.saveNow = job
		o.mu.Unlock()
		o.writeSave(ctx, job)
	}
}

// writeSave writes one queued save and reports how it went through
// Events.
func (o *Orchestrator) writeSave(ctx context.Context, job *saveJob) {
	defer job.done()
	if err := os.MkdirAll(filepath.Dir(job.path), 0o755); err != nil {
		o.log.Error("snippet save failed", "event", "snippet_failed", "error", err.Error())
		o.emit("saving the track failed: " + err.Error())
		return
	}
	if err := job.write(ctx, job.path, job.opts); err != nil {
		o.log.Error("snippet save failed", "event", "snippet_failed", "error", err.Error())
		o.emit("saving the track failed: " + err.Error())
		return
	}
	o.finishSave(job.song, job.path)
	o.emit("track saved: " + job.shown)
	// The write takes seconds and a rename takes none: a listener
	// who renamed the song while it was being written would
	// otherwise find the old name on disk forever.
	latest := job.song.Title
	if s, ok := o.Songbook.ByID(job.song.ID); ok {
		latest = s.Title
	} else if job.live != nil {
		o.mu.Lock()
		latest = job.live.Title
		o.mu.Unlock()
	}
	if latest != "" && latest != job.opts.Title {
		if moved, ok := o.retitleSaved(job.song.ID, latest); ok {
			o.emit("saved as: " + moved)
		}
	}
}

// claimTargetLocked chooses a song's saved file and speaks for the name
// in savePaths until the save that took it is done. A saved file is
// named to the second, so two songs with one title saved in the same
// second - or one song and another whose copy a device is sending -
// would otherwise land on one name, the second overwriting the first;
// the later one is numbered instead. Callers hold o.mu.
func (o *Orchestrator) claimTargetLocked(tag string, song songbook.Song) (path, shown string, opts export.MP3Options) {
	path, shown, opts = o.snippetTarget(tag, song)
	path = distinctPath(path, o.savePaths)
	shown = filepath.Join(filepath.Dir(shown), filepath.Base(path))
	o.savePaths[path] = true
	return path, shown, opts
}

// distinctPath numbers a saved file's name ("...-title-2.en.mp3") until
// it names neither a file on disk nor one a pending save has spoken
// for. The number goes before the language segment so the name still
// reads as one of the radio's.
func distinctPath(path string, claimed map[string]bool) string {
	taken := func(p string) bool {
		if claimed[p] {
			return true
		}
		_, err := os.Stat(p)
		return err == nil
	}
	if !taken(path) {
		return path
	}
	file := filepath.Base(path)
	suffix := ".mp3"
	if lang := snippets.Language(file); lang != "" {
		suffix = "." + lang + ".mp3"
	}
	stem := strings.TrimSuffix(file, suffix)
	for n := 2; ; n++ {
		p := filepath.Join(filepath.Dir(path), fmt.Sprintf("%s-%d%s", stem, n, suffix))
		if !taken(p) {
			return p
		}
	}
}

// snippetTarget decides where a song's saved copy goes and what tags it
// carries. The file is named after what the interface shows - the
// title, in its own language's script - plus the sung language's tag,
// so a track heard on the remote is findable on disk by the same name.
// shown is the tag directory and file name for acknowledgments; the
// absolute path stays in the log.
func (o *Orchestrator) snippetTarget(tag string, song songbook.Song) (path, shown string, opts export.MP3Options) {
	slug := snippets.Slug(tag)
	title, subtitle := song.Title, song.Subtitle
	if title == "" {
		title, subtitle = prompting.TrackTitle(song.Prompt)
	}
	path = snippets.Path(o.SnippetsDir, tag, title, song.Language, time.Now())
	shown = filepath.Join(slug, filepath.Base(path))
	opts = export.MP3Options{
		Quality:  o.cfg.MP3Quality,
		Title:    title,
		Subtitle: subtitle,
		Artist:   "Infinite AI Radio",
		Album:    slug,
		Comment:  snippetComment(song.Lyrics),
	}
	return path, shown, opts
}

// finishSave writes the lyric sheet beside a saved file and records the
// save. The full sheet rides along as a text file with the same base
// name (the ID3 comment only holds a truncated copy).
func (o *Orchestrator) finishSave(song songbook.Song, path string) {
	if song.Lyrics != "" && song.Lyrics != engine.InstrumentalLyrics {
		if err := os.WriteFile(snippets.LyricsSidecar(path), []byte(song.Lyrics), 0o644); err != nil {
			o.log.Warn("lyrics sidecar not written", "event", "snippet_lyrics_failed", "error", err.Error())
		}
	}
	o.Songbook.MarkSaved(song.ID, path)
	o.log.Info("snippet saved", "event", "snippet_saved", "path", path, "prompt", song.Prompt,
		"tag", filepath.Base(filepath.Dir(path)))
}

// maxUploadBytes bounds a copy sent back to be saved: the longest song
// the radio makes, at its best quality, is a fraction of this.
const maxUploadBytes = 40 << 20

// SaveUpload saves a song from a copy a device sends back, once the
// copy proves to be one the radio made: its hash is looked up in the
// songbook and the file is saved under the name and words recorded
// there. hash, when given, is what the sender believes the copy is,
// checked before the bytes are read and again after. Nothing that does
// not match is kept. The write is done when this returns.
func (o *Orchestrator) SaveUpload(hash, tag string, body io.Reader) string {
	if hash != "" {
		if _, known := o.Songbook.ByHash(hash); !known {
			return "that copy is not a song this radio made"
		}
	}
	if err := os.MkdirAll(o.SnippetsDir, 0o755); err != nil {
		return "saving the track failed: " + err.Error()
	}
	tmp, err := os.CreateTemp(o.SnippetsDir, ".upload-*.mp3")
	if err != nil {
		return "saving the track failed: " + err.Error()
	}
	defer os.Remove(tmp.Name())
	sum := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(body, maxUploadBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "the copy did not arrive whole: " + err.Error()
	}
	if info, err := os.Stat(tmp.Name()); err == nil && info.Size() > maxUploadBytes {
		return "that copy is too large to be a song this radio made"
	}
	got := hex.EncodeToString(sum.Sum(nil))
	song, known := o.Songbook.ByHash(got)
	if !known {
		o.log.Info("uploaded copy not recognised", "event", "upload_unknown", "hash", got)
		return "that copy is not a song this radio made"
	}
	if hash != "" && got != hash {
		return "the copy sent is not the song it was said to be"
	}
	if song.Saved != "" {
		return ackAlreadySaved
	}
	ctx := o.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	// One file per song: a copy of a song whose save is already on
	// its way from the radio's own store is the same save, not a
	// second one. The song is spoken for while its copy is written so
	// a save from the store arriving meanwhile is told the same. The
	// book is read again under the lock, as runSave does, for the
	// same reason.
	o.mu.Lock()
	if o.saveClosed || ctx.Err() != nil {
		o.mu.Unlock()
		return AckClosing
	}
	if song.ID != "" && o.Songbook.SavedPath(song.ID) != "" {
		o.mu.Unlock()
		return ackAlreadySaved
	}
	if o.savePendingLocked(song.ID) {
		o.mu.Unlock()
		return ackAlreadySaving
	}
	if song.ID != "" {
		o.saveUploads[song.ID] = true
	}
	path, shown, opts := o.claimTargetLocked(tag, song)
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		delete(o.saveUploads, song.ID)
		delete(o.savePaths, path)
		o.mu.Unlock()
	}()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "saving the track failed: " + err.Error()
	}
	wctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := export.CopyMP3(wctx, tmp.Name(), path, opts); err != nil {
		if ctx.Err() != nil {
			// The radio stopped under the write; the copy is still on
			// the device, and the radio will be back.
			return AckClosing
		}
		o.log.Error("snippet save failed", "event", "snippet_failed", "error", err.Error())
		return "saving the track failed: " + err.Error()
	}
	o.finishSave(song, path)
	o.emit("track saved: " + shown)
	return "track saved: " + shown
}

// isTrackID reports whether a save selector is a track id rather than a
// which keyword.
func isTrackID(which string) bool {
	return strings.HasPrefix(which, "t-") || strings.HasPrefix(which, bufTrackPrefix)
}

// maxSavedIDs bounds how many saved ids the status carries.
const maxSavedIDs = 64

// snippetComment carries the lyrics (when real) into the file's tags.
func snippetComment(lyrics string) string {
	if lyrics == "" || lyrics == engine.InstrumentalLyrics {
		return "instrumental"
	}
	if len(lyrics) > 500 {
		return lyrics[:500]
	}
	return lyrics
}

// NewSession clears everything and starts a fresh auto-persisted session
// seeded from a free-text prompt (the in-app `new` command).
func (o *Orchestrator) NewSession(prompt string) string {
	fresh := prompting.SessionFromPrompt(prompt)
	o.adoptLanguages(fresh)
	o.saveSession()
	o.mu.Lock()
	o.sess = fresh
	o.produced = false // nothing of this one has been made yet
	o.lastTweak = time.Time{}
	o.epoch++
	o.queue = nil
	o.lastGood = nil
	o.switchReq = true
	o.steerPending = false
	o.mu.Unlock()
	o.saveSession()
	o.recordCurrent()
	o.kickGen()
	o.expandSeedAsync(fresh)
	o.log.Info("new session from prompt", "event", "session_new_prompt", "prompt", prompt, "vocal", fresh.Vocal)
	ack := "new session: " + fresh.Describe()
	if fresh.Vocal {
		ack += " (with vocals)"
	}
	return ack + o.steerContextNote("")
}
