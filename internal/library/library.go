// Package library caches generated tracks on disk so a launch can start
// playing within seconds while fresh generation warms up. Tracks are
// stored per vibe key (preset name or prompt slug) as MP3 plus a small
// metadata file, with a rolling size cap across the whole library.
//
// The audio was uncompressed until 2026-09-21, from when this was the
// only store on disk and a banked track had to be readable without
// spawning anything. The rendered buffer has stored MP3 and decoded it
// on the playback path for every song since; that settles the question
// of whether the decode is affordable, and the cap buys about five
// times the music at the same quality the listener already hears.
package library

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"iar/internal/engine"
	"iar/internal/export"
	"iar/internal/session"
)

// meta is the sidecar metadata stored with each banked track.
type meta struct {
	// TrackID is the song's own id from the rest of the radio, so a
	// banked song is still the same song: a phone that downloaded it
	// before it played does not take it again as filler, and a listener
	// saving it after the radio has moved on can still name it. Empty
	// for starter tracks and for songs banked before songs carried one.
	TrackID  string `json:"track_id,omitempty"`
	Prompt   string `json:"prompt"`
	Lyrics   string `json:"lyrics"`
	Title    string `json:"title,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	// Language is the language the track was sung in, in the listener's
	// own wording. Empty for instrumentals, and for tracks banked
	// before it was recorded.
	Language string    `json:"language,omitempty"`
	Created  time.Time `json:"created"`
	// Seconds is the track's play time. A WAV could be measured by
	// dividing its size; a compressed file cannot, so it is recorded
	// here when the track is banked.
	Seconds float64 `json:"seconds,omitempty"`
}

// Library is a size-capped on-disk track cache. A nil *Library is valid
// and does nothing.
type Library struct {
	dir      string
	maxBytes int64
	quality  int
	log      *slog.Logger
}

// New returns a library rooted at dir with a total size cap of maxMB,
// banking at the given libmp3lame VBR quality. maxMB <= 0 disables the
// library (returns nil).
func New(dir string, maxMB, quality int, log *slog.Logger) *Library {
	if maxMB <= 0 {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	l := &Library{dir: dir, maxBytes: int64(maxMB) * 1024 * 1024, quality: quality, log: log}
	l.dropUncompressed()
	return l
}

// dropUncompressed clears out the WAV era. Transcoding the old files
// would be re-encoding music the radio can simply make again, and they
// are the reason the cap held twenty songs instead of a hundred - so
// they go, along with any sidecar left without audio beside it.
func (l *Library) dropUncompressed() {
	keys, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	freed, dropped := int64(0), 0
	for _, k := range keys {
		if !k.IsDir() {
			continue
		}
		dir := filepath.Join(l.dir, k.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		have := map[string]bool{}
		for _, f := range files {
			if name, ok := strings.CutSuffix(f.Name(), ".mp3"); ok {
				have[name] = true
			}
		}
		for _, f := range files {
			name := f.Name()
			stale := strings.HasSuffix(name, ".wav") || strings.HasSuffix(name, ".wav.tmp")
			if id, ok := strings.CutSuffix(name, ".json"); ok && !have[id] {
				stale = true
			}
			if !stale {
				continue
			}
			if fi, err := f.Info(); err == nil {
				freed += fi.Size()
			}
			if os.Remove(filepath.Join(dir, name)) == nil {
				dropped++
			}
		}
	}
	if dropped > 0 {
		l.log.Info("uncompressed library tracks cleared", "event", "library_wav_dropped",
			"files", dropped, "freed_mb", freed/(1024*1024))
	}
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
func (l *Library) Put(ctx context.Context, key string, t *engine.Track) (string, error) {
	if l == nil || t == nil || len(t.Samples) == 0 {
		return "", nil
	}
	dir := filepath.Join(l.dir, session.SanitizeName(key))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	id := time.Now().UTC().Format("20060102-150405") + fmt.Sprintf("-%04d", rand.IntN(10000))
	// Encoded beside its final name and renamed into place, so a crash
	// mid-encode leaves a dot-file the next scan ignores rather than a
	// half-written song something later tries to play.
	mp3Tmp := filepath.Join(dir, "."+id+".mp3.tmp")
	if err := export.EncodeMP3(ctx, t.Samples, mp3Tmp, export.MP3Options{
		Quality:  l.quality,
		Title:    t.Title,
		Subtitle: t.Subtitle,
		Artist:   "Infinite AI Radio",
		Comment:  "banked track",
	}); err != nil {
		os.Remove(mp3Tmp)
		return "", err
	}
	m, err := json.Marshal(meta{
		TrackID: t.ID,
		Prompt:  t.Prompt, Lyrics: t.Lyrics, Title: t.Title, Subtitle: t.Subtitle,
		Language: t.Spec.VocalLanguageName, Created: time.Now(),
		Seconds: t.Duration().Seconds(),
	})
	if err != nil {
		os.Remove(mp3Tmp)
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), m, 0o644); err != nil {
		os.Remove(mp3Tmp)
		return "", err
	}
	if err := os.Rename(mp3Tmp, filepath.Join(dir, id+".mp3")); err != nil {
		return "", err
	}
	l.log.Info("track banked", "event", "library_put", "key", key, "id", id)
	l.evict()
	return id, nil
}

// SetTitle writes a new name into a banked track's metadata, so instant
// starts and filler in later runs show the name a listener gave the song
// rather than the one it was banked under. An entry evicted since
// banking is a quiet no-op.
func (l *Library) SetTitle(key, id, title, subtitle string) bool {
	if l == nil {
		return false
	}
	dir := filepath.Join(l.dir, session.SanitizeName(key))
	metaPath := filepath.Join(dir, id+".json")
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return false
	}
	var m meta
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	m.Title, m.Subtitle = title, subtitle
	out, err := json.Marshal(m)
	if err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, id+".mp3")); err != nil {
		return false
	}
	tmp := metaPath + ".tmp"
	if os.WriteFile(tmp, out, 0o644) != nil || os.Rename(tmp, metaPath) != nil {
		os.Remove(tmp)
		return false
	}
	return true
}

// Pick loads a random banked track for key, returning its file id so
// callers can tell it apart from the same track offered as filler. ok is
// false when none exist.
func (l *Library) Pick(ctx context.Context, key string) (*engine.Track, string, bool) {
	if l == nil {
		return nil, "", false
	}
	dir := filepath.Join(l.dir, session.SanitizeName(key))
	ids := l.ids(dir)
	if len(ids) == 0 {
		return nil, "", false
	}
	id := ids[rand.IntN(len(ids))]
	samples, err := export.DecodePCM(ctx, filepath.Join(dir, id+".mp3"))
	if err != nil || len(samples) == 0 {
		// A cancelled context fails every decode; a shutdown must not
		// be read as a corrupt bank and delete the library.
		if ctx.Err() != nil {
			return nil, "", false
		}
		l.log.Warn("banked track unreadable, removing", "event", "library_corrupt", "id", id)
		os.Remove(filepath.Join(dir, id+".mp3"))
		os.Remove(filepath.Join(dir, id+".json"))
		return nil, "", false
	}
	var m meta
	if raw, err := os.ReadFile(filepath.Join(dir, id+".json")); err == nil {
		json.Unmarshal(raw, &m)
	}
	l.log.Info("track loaded from library", "event", "library_pick", "key", key, "id", id)
	return &engine.Track{
		ID:          m.TrackID,
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
	// TrackID is the song's own id from the rest of the radio; empty for
	// starter tracks and songs banked before songs carried one.
	TrackID string
	// Prompt is the prompt that produced the track.
	Prompt string
	// Title and Subtitle are the short display names, when banked.
	Title    string
	Subtitle string
	// Seconds is the track's play time, as recorded when it was banked.
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
		if _, err := os.Stat(filepath.Join(dir, id+".mp3")); err != nil {
			continue
		}
		e := Entry{ID: id}
		if raw, err := os.ReadFile(filepath.Join(dir, id+".json")); err == nil {
			var m meta
			json.Unmarshal(raw, &m)
			e.TrackID = m.TrackID
			e.Prompt = m.Prompt
			e.Title = m.Title
			e.Subtitle = m.Subtitle
			e.Language = m.Language
			e.Seconds = m.Seconds
			if m.Lyrics != engine.InstrumentalLyrics {
				e.Lyrics = m.Lyrics
			}
		}
		out = append(out, e)
	}
	return out
}

// Load reads one banked track by key and id. ok is false when the track
// is gone (eviction races are expected and harmless).
func (l *Library) Load(ctx context.Context, key, id string) (*engine.Track, bool) {
	if l == nil {
		return nil, false
	}
	dir := filepath.Join(l.dir, session.SanitizeName(key))
	samples, err := export.DecodePCM(ctx, filepath.Join(dir, session.SanitizeName(id)+".mp3"))
	if err != nil || len(samples) == 0 {
		return nil, false
	}
	var m meta
	if raw, err := os.ReadFile(filepath.Join(dir, session.SanitizeName(id)+".json")); err == nil {
		json.Unmarshal(raw, &m)
	}
	return &engine.Track{
		ID:      m.TrackID,
		Samples: samples, Spec: engine.Spec{VocalLanguageName: m.Language},
		Prompt: m.Prompt, Lyrics: m.Lyrics, Title: m.Title, Subtitle: m.Subtitle,
		FromLibrary: true,
	}, true
}

// Locate names the key and file a song was banked under, by the song's
// own id, whichever vibe it went into. It is the long way round - every
// sidecar in the library is read - and exists for the question nothing
// faster can answer after a restart: is the song a listener is still
// holding anywhere at all.
func (l *Library) Locate(trackID string) (key, id string, ok bool) {
	if l == nil || trackID == "" {
		return "", "", false
	}
	keys, err := os.ReadDir(l.dir)
	if err != nil {
		return "", "", false
	}
	for _, k := range keys {
		if !k.IsDir() {
			continue
		}
		dir := filepath.Join(l.dir, k.Name())
		for _, id := range l.ids(dir) {
			raw, err := os.ReadFile(filepath.Join(dir, id+".json"))
			if err != nil {
				continue
			}
			var m meta
			if json.Unmarshal(raw, &m) == nil && m.TrackID == trackID {
				return k.Name(), id, true
			}
		}
	}
	return "", "", false
}

// Find loads a banked song by its own id (see Locate).
func (l *Library) Find(ctx context.Context, trackID string) (*engine.Track, bool) {
	key, id, ok := l.Locate(trackID)
	if !ok {
		return nil, false
	}
	return l.Load(ctx, key, id)
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
		if name, ok := strings.CutSuffix(e.Name(), ".mp3"); ok && !strings.HasPrefix(name, ".") {
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
		mp3, json string
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
			mp3 := filepath.Join(dir, id+".mp3")
			fi, err := os.Stat(mp3)
			if err != nil {
				continue
			}
			byKey[kd.Name()] = append(byKey[kd.Name()],
				file{mp3: mp3, json: filepath.Join(dir, id+".json"), size: fi.Size(), mod: fi.ModTime()})
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
		os.Remove(f.mp3)
		os.Remove(f.json)
		total -= f.size
		l.log.Info("library track evicted", "event", "library_evict", "path", f.mp3)
	}
}
