// Package library caches generated tracks on disk so a launch can start
// playing within seconds while fresh generation warms up. Tracks are
// stored per vibe key (preset name or prompt slug) as WAV plus a small
// metadata file, with a rolling size cap across the whole library.
package library

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"iar/internal/audio"
	"iar/internal/engine"
	"iar/internal/session"
)

// meta is the sidecar metadata stored with each banked track.
type meta struct {
	Prompt   string `json:"prompt"`
	Lyrics   string `json:"lyrics"`
	Title    string `json:"title,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	// Language is the language the track was sung in, in the listener's
	// own wording. Empty for instrumentals, and for tracks banked
	// before it was recorded.
	Language string    `json:"language,omitempty"`
	Created  time.Time `json:"created"`
}

// Library is a size-capped on-disk track cache. A nil *Library is valid
// and does nothing.
type Library struct {
	dir      string
	maxBytes int64
	log      *slog.Logger
}

// New returns a library rooted at dir with a total size cap of maxMB.
// maxMB <= 0 disables the library (returns nil).
func New(dir string, maxMB int, log *slog.Logger) *Library {
	if maxMB <= 0 {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &Library{dir: dir, maxBytes: int64(maxMB) * 1024 * 1024, log: log}
}

// Key derives the library key for a session: the preset name when the
// session came from one, otherwise a slug of its base prompt.
func Key(s *session.Session) string {
	if s.Preset != "" {
		return s.Preset
	}
	slug := session.SanitizeName(s.BasePrompt)
	if len(slug) > 48 {
		slug = slug[:48]
	}
	if slug == "" || slug == "unnamed" {
		return "default"
	}
	return slug
}

// Put banks a track under key and enforces the size cap.
func (l *Library) Put(key string, t *engine.Track) error {
	if l == nil || t == nil || len(t.Samples) == 0 {
		return nil
	}
	dir := filepath.Join(l.dir, session.SanitizeName(key))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	id := time.Now().UTC().Format("20060102-150405") + fmt.Sprintf("-%04d", rand.IntN(10000))
	wavTmp := filepath.Join(dir, "."+id+".wav.tmp")
	if err := os.WriteFile(wavTmp, audio.EncodeWAV(t.Samples), 0o644); err != nil {
		return err
	}
	m, err := json.Marshal(meta{
		Prompt: t.Prompt, Lyrics: t.Lyrics, Title: t.Title, Subtitle: t.Subtitle,
		Language: t.Spec.VocalLanguageName, Created: time.Now(),
	})
	if err != nil {
		os.Remove(wavTmp)
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), m, 0o644); err != nil {
		os.Remove(wavTmp)
		return err
	}
	if err := os.Rename(wavTmp, filepath.Join(dir, id+".wav")); err != nil {
		return err
	}
	l.log.Info("track banked", "event", "library_put", "key", key, "id", id)
	l.evict()
	return nil
}

// Pick loads a random banked track for key, returning its file id so
// callers can tell it apart from the same track offered as filler. ok is
// false when none exist.
func (l *Library) Pick(key string) (*engine.Track, string, bool) {
	if l == nil {
		return nil, "", false
	}
	dir := filepath.Join(l.dir, session.SanitizeName(key))
	ids := l.ids(dir)
	if len(ids) == 0 {
		return nil, "", false
	}
	id := ids[rand.IntN(len(ids))]
	data, err := os.ReadFile(filepath.Join(dir, id+".wav"))
	if err != nil {
		return nil, "", false
	}
	w, err := audio.DecodeWAV(data)
	if err != nil {
		l.log.Warn("banked track unreadable, removing", "event", "library_corrupt", "id", id)
		os.Remove(filepath.Join(dir, id+".wav"))
		os.Remove(filepath.Join(dir, id+".json"))
		return nil, "", false
	}
	samples, err := w.ToInternal()
	if err != nil {
		return nil, "", false
	}
	var m meta
	if raw, err := os.ReadFile(filepath.Join(dir, id+".json")); err == nil {
		json.Unmarshal(raw, &m)
	}
	l.log.Info("track loaded from library", "event", "library_pick", "key", key, "id", id)
	return &engine.Track{
		Samples:     samples,
		Spec:        engine.Spec{VocalLanguageName: m.Language},
		Prompt:      m.Prompt,
		Lyrics:      m.Lyrics,
		Title:       m.Title,
		Subtitle:    m.Subtitle,
		FromLibrary: true,
	}, id, true
}

// Entry describes one banked track without loading its audio.
type Entry struct {
	// ID is the track's file id inside its key directory.
	ID string
	// Prompt is the prompt that produced the track.
	Prompt string
	// Title and Subtitle are the short display names, when banked.
	Title    string
	Subtitle string
	// Seconds is the track's play time, derived from the file size.
	Seconds float64
	// Language is the language the track was sung in, in the listener's
	// own wording. Empty for instrumentals and for tracks banked before
	// it was recorded, which is indistinguishable from "unknown".
	Language string
	// Lyrics is what the track sings, empty for instrumentals.
	Lyrics string
}

// Entries lists the banked tracks under key, newest first.
func (l *Library) Entries(key string) []Entry {
	if l == nil {
		return nil
	}
	dir := filepath.Join(l.dir, session.SanitizeName(key))
	ids := l.ids(dir)
	sort.Sort(sort.Reverse(sort.StringSlice(ids))) // ids start with a timestamp
	var out []Entry
	for _, id := range ids {
		fi, err := os.Stat(filepath.Join(dir, id+".wav"))
		if err != nil {
			continue
		}
		e := Entry{ID: id}
		if raw, err := os.ReadFile(filepath.Join(dir, id+".json")); err == nil {
			var m meta
			json.Unmarshal(raw, &m)
			e.Prompt = m.Prompt
			e.Title = m.Title
			e.Subtitle = m.Subtitle
			e.Language = m.Language
			if m.Lyrics != engine.InstrumentalLyrics {
				e.Lyrics = m.Lyrics
			}
		}
		if bytes := fi.Size() - 44; bytes > 0 {
			e.Seconds = float64(bytes) / (audio.SampleRate * audio.Channels * 2)
		}
		out = append(out, e)
	}
	return out
}

// Load reads one banked track by key and id. ok is false when the track
// is gone (eviction races are expected and harmless).
func (l *Library) Load(key, id string) (*engine.Track, bool) {
	if l == nil {
		return nil, false
	}
	dir := filepath.Join(l.dir, session.SanitizeName(key))
	data, err := os.ReadFile(filepath.Join(dir, session.SanitizeName(id)+".wav"))
	if err != nil {
		return nil, false
	}
	w, err := audio.DecodeWAV(data)
	if err != nil {
		return nil, false
	}
	samples, err := w.ToInternal()
	if err != nil {
		return nil, false
	}
	var m meta
	if raw, err := os.ReadFile(filepath.Join(dir, session.SanitizeName(id)+".json")); err == nil {
		json.Unmarshal(raw, &m)
	}
	return &engine.Track{
		Samples: samples, Spec: engine.Spec{VocalLanguageName: m.Language},
		Prompt: m.Prompt, Lyrics: m.Lyrics, Title: m.Title, Subtitle: m.Subtitle,
		FromLibrary: true,
	}, true
}

// Count reports how many tracks are banked under key.
func (l *Library) Count(key string) int {
	if l == nil {
		return 0
	}
	return len(l.ids(filepath.Join(l.dir, session.SanitizeName(key))))
}

// ids lists banked track ids in a key directory.
func (l *Library) ids(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".wav"); ok && !strings.HasPrefix(name, ".") {
			out = append(out, name)
		}
	}
	return out
}

// evict trims the library under its size cap. Eviction is fair across
// vibes: it always removes the oldest track of the key holding the MOST
// tracks, so one long listening session cannot silently wipe out every
// other vibe's instant-start tracks.
func (l *Library) evict() {
	type file struct {
		wav, json string
		size      int64
		mod       time.Time
	}
	byKey := map[string][]file{}
	var total int64
	keyDirs, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	for _, kd := range keyDirs {
		if !kd.IsDir() {
			continue
		}
		dir := filepath.Join(l.dir, kd.Name())
		for _, id := range l.ids(dir) {
			wav := filepath.Join(dir, id+".wav")
			fi, err := os.Stat(wav)
			if err != nil {
				continue
			}
			byKey[kd.Name()] = append(byKey[kd.Name()],
				file{wav: wav, json: filepath.Join(dir, id+".json"), size: fi.Size(), mod: fi.ModTime()})
			total += fi.Size()
		}
	}
	for key := range byKey {
		files := byKey[key]
		sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
		byKey[key] = files
	}
	for total > l.maxBytes {
		// The key with the most tracks loses its oldest one; ties go to
		// the key with the oldest candidate.
		victim := ""
		for key, files := range byKey {
			if len(files) == 0 {
				continue
			}
			switch {
			case victim == "":
				victim = key
			case len(files) > len(byKey[victim]):
				victim = key
			case len(files) == len(byKey[victim]) && files[0].mod.Before(byKey[victim][0].mod):
				victim = key
			}
		}
		if victim == "" {
			return
		}
		f := byKey[victim][0]
		byKey[victim] = byKey[victim][1:]
		os.Remove(f.wav)
		os.Remove(f.json)
		total -= f.size
		l.log.Info("library track evicted", "event", "library_evict", "path", f.wav)
	}
}
