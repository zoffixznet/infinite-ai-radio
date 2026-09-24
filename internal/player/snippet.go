package player

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
		// encode of it.
		if path, ok := o.TrackFile(which); ok {
			if song, known := o.Songbook.ByID(which); known {
				return o.runSave(tag, saveJob{
					song: song,
					write: func(ctx context.Context, dst string, opts export.MP3Options) error {
						return export.CopyMP3(ctx, path, dst, opts)
					},
				})
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
	return o.runSave(tag, saveJob{
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
}

// runSave starts a save in the background and answers straight away.
func (o *Orchestrator) runSave(tag string, job saveJob) string {
	o.mu.Lock()
	busy := o.saving
	if !busy {
		o.saving = true
	}
	o.mu.Unlock()
	if busy {
		return "a snippet is already being saved; try again in a moment"
	}
	path, shown, opts := o.snippetTarget(tag, job.song)
	ctx := o.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		defer func() {
			o.mu.Lock()
			o.saving = false
			o.mu.Unlock()
		}()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			o.log.Error("snippet save failed", "event", "snippet_failed", "error", err.Error())
			o.emit("saving the track failed: " + err.Error())
			return
		}
		if err := job.write(ctx, path, opts); err != nil {
			o.log.Error("snippet save failed", "event", "snippet_failed", "error", err.Error())
			o.emit("saving the track failed: " + err.Error())
			return
		}
		o.finishSave(job.song, path)
		o.emit("track saved: " + shown)
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
		if latest != "" && latest != opts.Title {
			if moved, ok := o.retitleSaved(job.song.ID, latest); ok {
				o.emit("saved as: " + moved)
			}
		}
	}()
	return "saving this track to " + shown
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
	path, shown, opts := o.snippetTarget(tag, song)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "saving the track failed: " + err.Error()
	}
	ctx := o.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := export.CopyMP3(ctx, tmp.Name(), path, opts); err != nil {
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
