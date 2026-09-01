// Package trackbuffer is the on-disk buffer of phased generation: the
// plans the planner has written but the renderer has not turned into
// audio yet, and the rendered songs waiting to be played. Everything
// lives as files under one directory - plans as JSON, songs as MP3
// with a JSON sidecar - so a full buffer costs disk, not memory, and
// survives restarts. Only the track being handed to playback is ever
// decoded into RAM.
package trackbuffer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"iar/internal/audio"
	"iar/internal/engine"
	"iar/internal/export"
)

// Store is the on-disk buffer. Methods are safe for one producer and
// one consumer goroutine (the scheduler and the playback feeder); the
// file system is the state, so there is nothing to lock beyond what
// rename gives us.
type Store struct {
	dir     string
	quality int
	log     *slog.Logger
}

// New returns a store rooted at dir (created on first use). quality is
// the MP3 VBR quality for rendered songs (0 best..9 smallest).
func New(dir string, quality int, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{dir: dir, quality: quality, log: log}
}

// Dir returns the buffer's root directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) plansDir() string  { return filepath.Join(s.dir, "plans") }
func (s *Store) tracksDir() string { return filepath.Join(s.dir, "tracks") }

// name builds the sortable base name for an epoch/sequence pair.
func name(epoch, seq int) string { return fmt.Sprintf("e%08d-%08d", epoch, seq) }

// parseName extracts epoch and sequence from a base name.
func parseName(base string) (epoch, seq int, ok bool) {
	if _, err := fmt.Sscanf(base, "e%08d-%08d", &epoch, &seq); err != nil {
		return 0, 0, false
	}
	return epoch, seq, true
}

// storedPlan is a plan file's content.
type storedPlan struct {
	Plan    engine.Plan `json:"plan"`
	Created time.Time   `json:"created"`
}

// trackMeta is the JSON sidecar of a rendered song.
type trackMeta struct {
	Prompt  string        `json:"prompt"`
	Lyrics  string        `json:"lyrics,omitempty"`
	Seconds float64       `json:"seconds"`
	Seed    string        `json:"seed,omitempty"`
	GenTime time.Duration `json:"gen_time,omitempty"`
	Spec    engine.Spec   `json:"spec"`
	Created time.Time     `json:"created"`
}

// PutPlan stores one plan under epoch/seq.
func (s *Store) PutPlan(epoch, seq int, p *engine.Plan) error {
	if err := os.MkdirAll(s.plansDir(), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(storedPlan{Plan: *p, Created: time.Now()})
	if err != nil {
		return err
	}
	path := filepath.Join(s.plansDir(), name(epoch, seq)+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// NextPlan returns the oldest plan of the epoch without removing it;
// the caller removes it with DropPlan once the render succeeded.
func (s *Store) NextPlan(epoch int) (*engine.Plan, int, bool) {
	base, ok := s.oldest(s.plansDir(), epoch, ".json")
	if !ok {
		return nil, 0, false
	}
	raw, err := os.ReadFile(filepath.Join(s.plansDir(), base+".json"))
	if err != nil {
		return nil, 0, false
	}
	var sp storedPlan
	if err := json.Unmarshal(raw, &sp); err != nil {
		s.log.Warn("unreadable plan dropped", "event", "buffer_plan_bad", "file", base, "error", err.Error())
		os.Remove(filepath.Join(s.plansDir(), base+".json"))
		return nil, 0, false
	}
	_, seq, _ := parseName(base)
	return &sp.Plan, seq, true
}

// DropPlan removes one plan (after a successful render, or a steer).
func (s *Store) DropPlan(epoch, seq int) {
	os.Remove(filepath.Join(s.plansDir(), name(epoch, seq)+".json"))
}

// PutTrack encodes a rendered track to MP3 with a metadata sidecar.
func (s *Store) PutTrack(ctx context.Context, epoch, seq int, t *engine.Track) error {
	if err := os.MkdirAll(s.tracksDir(), 0o755); err != nil {
		return err
	}
	base := filepath.Join(s.tracksDir(), name(epoch, seq))
	meta := trackMeta{
		Prompt:  t.Prompt,
		Lyrics:  t.Lyrics,
		Seconds: float64(len(t.Samples)) / float64(audio.SampleRate*audio.Channels),
		Seed:    t.Seed,
		GenTime: t.GenTime,
		Spec:    t.Spec,
		Created: time.Now(),
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := export.EncodeMP3(ctx, t.Samples, base+".mp3", export.MP3Options{
		Quality: s.quality,
		Title:   t.Title,
		Artist:  "Infinite AI Radio",
		Comment: "buffered track",
	}); err != nil {
		return err
	}
	tmp := base + ".json.tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		os.Remove(base + ".mp3")
		return err
	}
	return os.Rename(tmp, base+".json")
}

// NextTrack decodes and removes the oldest rendered song of the epoch.
// A song that cannot be decoded is dropped and the next one tried.
func (s *Store) NextTrack(ctx context.Context, epoch int) (*engine.Track, bool) {
	for {
		base, ok := s.oldest(s.tracksDir(), epoch, ".json")
		if !ok {
			return nil, false
		}
		mp3 := filepath.Join(s.tracksDir(), base+".mp3")
		metaPath := filepath.Join(s.tracksDir(), base+".json")
		var meta trackMeta
		raw, err := os.ReadFile(metaPath)
		if err == nil {
			err = json.Unmarshal(raw, &meta)
		}
		var samples []int16
		if err == nil {
			samples, err = export.DecodePCM(ctx, mp3)
		}
		os.Remove(metaPath)
		os.Remove(mp3)
		if err != nil || len(samples) == 0 {
			s.log.Warn("unplayable buffered track dropped", "event", "buffer_track_bad", "file", base,
				"error", fmt.Sprint(err))
			continue
		}
		return &engine.Track{
			Samples: samples,
			Spec:    meta.Spec,
			Prompt:  meta.Prompt,
			Lyrics:  meta.Lyrics,
			Seed:    meta.Seed,
			GenTime: meta.GenTime,
		}, true
	}
}

// PlanStats reports how many plans of the epoch await rendering and
// the audio seconds they will produce.
func (s *Store) PlanStats(epoch int) (count int, seconds float64) {
	s.each(s.plansDir(), epoch, ".json", func(path string) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var sp storedPlan
		if json.Unmarshal(raw, &sp) != nil {
			return
		}
		count++
		seconds += sp.Plan.Seconds
	})
	return count, seconds
}

// TrackStats reports how many rendered songs of the epoch await play
// and their total audio seconds.
func (s *Store) TrackStats(epoch int) (count int, seconds float64) {
	s.each(s.tracksDir(), epoch, ".json", func(path string) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var m trackMeta
		if json.Unmarshal(raw, &m) != nil {
			return
		}
		count++
		seconds += m.Seconds
	})
	return count, seconds
}

// MaxSeq returns the highest sequence number present for the epoch, so
// a restarted player continues numbering instead of colliding.
func (s *Store) MaxSeq(epoch int) int {
	max := 0
	for _, dir := range []string{s.plansDir(), s.tracksDir()} {
		s.each(dir, epoch, ".json", func(path string) {
			if _, seq, ok := parseName(strings.TrimSuffix(filepath.Base(path), ".json")); ok && seq > max {
				max = seq
			}
		})
	}
	return max
}

// DropOtherEpochs removes every plan and rendered song that does not
// belong to the given epoch: a steer makes the old context's work
// worthless, and keeping it would play stale music.
func (s *Store) DropOtherEpochs(epoch int) (dropped int) {
	for _, dir := range []string{s.plansDir(), s.tracksDir()} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			base := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			ep, _, ok := parseName(base)
			if !ok || ep == epoch {
				continue
			}
			if os.Remove(filepath.Join(dir, e.Name())) == nil {
				dropped++
			}
		}
	}
	return dropped
}

// oldest returns the lexically-first base name with the wanted epoch.
func (s *Store) oldest(dir string, epoch int, ext string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("buffer dir unreadable", "event", "buffer_dir_bad", "dir", dir, "error", err.Error())
		}
		return "", false
	}
	var names []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ext) || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ext)
		if ep, _, ok := parseName(base); ok && ep == epoch {
			names = append(names, base)
		}
	}
	if len(names) == 0 {
		return "", false
	}
	sort.Strings(names)
	return names[0], true
}

// each calls fn for every file of the epoch with the extension.
func (s *Store) each(dir string, epoch int, ext string, fn func(path string)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ext) || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		if ep, _, ok := parseName(strings.TrimSuffix(e.Name(), ext)); ok && ep == epoch {
			fn(filepath.Join(dir, e.Name()))
		}
	}
}
