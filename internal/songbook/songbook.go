// Package songbook is the radio's memory of every song it has made:
// the hash of the file as it went out and the name, words and prompt
// it went out with. The audio itself comes and goes - the buffer keeps
// it for a while, a phone keeps it longer - but the record outlives
// both, so a copy that comes back from anywhere is recognised and saved
// with what the radio knows about it, however long ago it was made.
//
// The book is one append-only file of JSON lines, the newest line for a
// song being the truth about it, read whole into memory on open and
// rewritten compact when the stale lines outnumber the live ones.
package songbook

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Song is what the radio remembers about one song it made.
type Song struct {
	// ID is the song's own identity, the one it was rendered under.
	ID string `json:"id"`
	// Hash is the SHA-256 (hex) of the MP3 exactly as the radio serves
	// it. A file that hashes to this is this song, wherever it comes
	// back from.
	Hash     string `json:"hash,omitempty"`
	Title    string `json:"title,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	Lyrics   string `json:"lyrics,omitempty"`
	// Language is the tag the vocals were sung in.
	Language string    `json:"language,omitempty"`
	Seconds  float64   `json:"seconds,omitempty"`
	Created  time.Time `json:"created"`
	// Saved is the snippet path the song was saved to; empty until it is.
	Saved string `json:"saved,omitempty"`
}

// MaxSongs bounds the book: past it the oldest records are forgotten.
// At the radio's pace that is several months of songs, far past what
// any device still holds a copy of.
const MaxSongs = 10000

// maxSaves bounds the remembered order of saves (the status line asks
// for the last few dozen).
const maxSaves = 256

// Book is the open songbook. Safe for concurrent use.
type Book struct {
	mu     sync.Mutex
	path   string
	log    *slog.Logger
	byID   map[string]*Song
	byHash map[string]*Song
	// order lists ids oldest first, so the cap forgets the right end.
	order []string
	// saves lists ids in the order they were saved, for the status line.
	saves []string
	// stale counts lines in the file that no longer say anything: a
	// later line replaced them, or the record was forgotten.
	stale int
}

// Open loads the book at path, creating it on first write. An empty
// path keeps the book in memory only. A book that cannot be read is
// reported and started empty rather than refused: the radio keeps
// playing, it just cannot recognise older copies.
func Open(path string, log *slog.Logger) *Book {
	if log == nil {
		log = slog.Default()
	}
	b := &Book{path: path, log: log, byID: map[string]*Song{}, byHash: map[string]*Song{}}
	if path == "" {
		return b
	}
	if err := b.load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("songbook unreadable; starting empty", "event", "songbook_unreadable",
			"path", path, "error", err.Error())
	}
	if b.stale > len(b.order) {
		if err := b.rewrite(); err != nil {
			log.Warn("songbook not compacted", "event", "songbook_compact_failed", "error", err.Error())
		}
	}
	return b
}

// Path returns the file the book lives in ("" for a memory-only book).
func (b *Book) Path() string { return b.path }

// Len reports how many songs the book remembers.
func (b *Book) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.order)
}

// Record remembers a song, replacing anything held under its id.
func (b *Book) Record(s Song) error {
	if s.ID == "" {
		return errors.New("a song needs an id to be recorded")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.put(s)
	return b.append(s)
}

// ByID returns the record for a song id.
func (b *Book) ByID(id string) (Song, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.byID[id]
	if !ok {
		return Song{}, false
	}
	return *s, true
}

// ByHash returns the record whose file hashes to hash.
func (b *Book) ByHash(hash string) (Song, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.byHash[hash]
	if !ok || hash == "" {
		return Song{}, false
	}
	return *s, true
}

// Retitle records a listener's new name for a song. Reports whether the
// song was known.
func (b *Book) Retitle(id, title string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.byID[id]
	if !ok {
		return false
	}
	if s.Title == title {
		return true
	}
	rec := *s
	rec.Title = title
	b.put(rec)
	b.write(rec)
	return true
}

// MarkSaved records where a song's saved copy landed. A song the book
// never saw made - a starter track from setup, say - gets a record of
// the save alone, so saving it twice is still refused.
func (b *Book) MarkSaved(id, path string) {
	if id == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := Song{ID: id, Created: time.Now()}
	if s, ok := b.byID[id]; ok {
		rec = *s
	}
	rec.Saved = path
	b.put(rec)
	b.write(rec)
}

// SavedPath returns where a song's saved copy landed, "" if it never was.
func (b *Book) SavedPath(id string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.byID[id]; ok {
		return s.Saved
	}
	return ""
}

// RecentlySaved lists the ids of the last n songs saved, oldest first.
func (b *Book) RecentlySaved(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > len(b.saves) {
		n = len(b.saves)
	}
	return append([]string(nil), b.saves[len(b.saves)-n:]...)
}

// put installs a record in the indexes, replacing the one under its id.
// Callers hold b.mu.
func (b *Book) put(s Song) {
	if old, known := b.byID[s.ID]; known {
		b.stale++
		if old.Hash != "" && old.Hash != s.Hash {
			delete(b.byHash, old.Hash)
		}
		if old.Saved != "" && s.Saved == "" {
			s.Saved = old.Saved // a re-render never un-saves a song
		}
		if old.Saved == "" && s.Saved != "" {
			b.noteSave(s.ID)
		}
		*old = s
		if s.Hash != "" {
			b.byHash[s.Hash] = old
		}
		return
	}
	rec := &s
	b.byID[s.ID] = rec
	if s.Hash != "" {
		b.byHash[s.Hash] = rec
	}
	if s.Saved != "" {
		b.noteSave(s.ID)
	}
	b.order = append(b.order, s.ID)
	for len(b.order) > MaxSongs {
		b.forget(b.order[0])
	}
}

// noteSave appends to the save order, bounded. Callers hold b.mu.
func (b *Book) noteSave(id string) {
	b.saves = append(b.saves, id)
	if len(b.saves) > maxSaves {
		b.saves = b.saves[len(b.saves)-maxSaves:]
	}
}

// forget drops the oldest record. Callers hold b.mu.
func (b *Book) forget(id string) {
	b.order = b.order[1:]
	if s, ok := b.byID[id]; ok {
		delete(b.byID, id)
		if s.Hash != "" && b.byHash[s.Hash] == s {
			delete(b.byHash, s.Hash)
		}
	}
	b.stale++
}

// write appends a record, reporting a failure rather than returning
// it: the callers are answering a listener, and the record in memory
// is already right. Callers hold b.mu.
func (b *Book) write(s Song) {
	if err := b.append(s); err != nil {
		b.log.Warn("songbook not written", "event", "songbook_write_failed", "error", err.Error())
	}
}

// append writes one record to the end of the file. Callers hold b.mu.
func (b *Book) append(s Song) error {
	if b.path == "" {
		return nil
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(b.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// load reads the file into the indexes. Only Open calls it, before the
// book is shared.
func (b *Book) load() error {
	f, err := os.Open(b.path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var s Song
		if json.Unmarshal(line, &s) != nil || s.ID == "" {
			b.stale++ // a torn last line from a crash mid-write
			continue
		}
		b.put(s)
	}
	return sc.Err()
}

// rewrite writes the live records back as one compact file. Only Open
// calls it, before the book is shared.
func (b *Book) rewrite() error {
	if b.path == "" {
		return nil
	}
	tmp := b.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, id := range b.order {
		raw, err := json.Marshal(b.byID[id])
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		w.Write(append(raw, '\n'))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, b.path); err != nil {
		os.Remove(tmp)
		return err
	}
	b.stale = 0
	return nil
}
