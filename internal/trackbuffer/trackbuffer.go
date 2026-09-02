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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

// Context returns the steering-context key the buffer's content belongs
// to (empty when the buffer is fresh or from an older version).
func (s *Store) Context() string {
	raw, err := os.ReadFile(filepath.Join(s.dir, "context"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// RenderVersion identifies the render path that produced the songs in
// a buffer. Bump it whenever a fix changes what the renderer produces:
// songs already on disk were made by the old path and are dropped on
// the next start, while their plans - which the renderer reads, not
// writes - are kept and simply rendered again.
//
//	2: the deferred DiT silently discarded the planner's audio codes,
//	   so every phased render was conditioned on silence and came out
//	   with a 5 Hz comb across the whole song.
const RenderVersion = 2

// RenderVersionOf returns the render path that wrote this buffer's
// songs; 0 for a buffer from before the marker existed.
func (s *Store) RenderVersionOf() int {
	raw, err := os.ReadFile(filepath.Join(s.dir, "render-version"))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return n
}

// SetRenderVersion records the render path that wrote this buffer.
func (s *Store) SetRenderVersion(v int) {
	if os.MkdirAll(s.dir, 0o755) != nil {
		return
	}
	tmp := filepath.Join(s.dir, "render-version.tmp")
	if os.WriteFile(tmp, []byte(strconv.Itoa(v)+"\n"), 0o644) == nil {
		os.Rename(tmp, filepath.Join(s.dir, "render-version"))
	}
}

// DropTracks removes every rendered song, of any epoch, leaving the
// plans alone: the audio is unusable but the work that produced it is
// still good.
func (s *Store) DropTracks() (dropped int) {
	entries, err := os.ReadDir(s.tracksDir())
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if os.Remove(filepath.Join(s.tracksDir(), e.Name())) == nil && strings.HasSuffix(e.Name(), ".json") {
			dropped++
		}
	}
	return dropped
}

// Sweep removes debris a crash leaves behind: half-written files from
// an interrupted encode, and songs whose audio and metadata got
// separated. Nothing else ever collects these - the epoch and context
// sweeps both work through parsed base names, and a ".part" or a
// sidecar-less MP3 parses as nothing, so it would sit on disk forever.
// One player runs at a time, so a sweep at startup cannot race a write.
func (s *Store) Sweep() (dropped int, freed int64) {
	for _, dir := range []string{s.plansDir(), s.tracksDir()} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		// Both halves of a stored song must be present: PutTrack writes
		// the audio first, so a crash between the two leaves a large
		// MP3 that no listing will ever look at.
		have := map[string]map[string]bool{}
		for _, e := range entries {
			name := e.Name()
			ext := filepath.Ext(name)
			base := strings.TrimSuffix(name, ext)
			if _, _, ok := parseName(base); !ok {
				continue
			}
			if have[base] == nil {
				have[base] = map[string]bool{}
			}
			have[base][ext] = true
		}
		for _, e := range entries {
			name := e.Name()
			path := filepath.Join(dir, name)
			ext := filepath.Ext(name)
			base := strings.TrimSuffix(name, ext)
			junk := false
			switch {
			case ext == ".part" || ext == ".tmp":
				junk = true // an encode or a write that never finished
			case dir == s.tracksDir() && (ext == ".mp3" || ext == ".json"):
				// A song needs both halves to be playable.
				junk = !have[base][".mp3"] || !have[base][".json"]
			default:
				_, _, ok := parseName(base)
				junk = !ok
			}
			if !junk {
				continue
			}
			size := int64(0)
			if info, err := e.Info(); err == nil {
				size = info.Size()
			}
			if os.Remove(path) == nil {
				dropped++
				freed += size
			}
		}
	}
	return dropped, freed
}

// SetContext records the steering-context key the buffer now serves.
func (s *Store) SetContext(key string) {
	if os.MkdirAll(s.dir, 0o755) != nil {
		return
	}
	tmp := filepath.Join(s.dir, "context.tmp")
	if os.WriteFile(tmp, []byte(key+"\n"), 0o644) == nil {
		os.Rename(tmp, filepath.Join(s.dir, "context"))
	}
}

// DropAll removes every plan and rendered song regardless of epoch
// (the buffer belonged to a different steering context).
func (s *Store) DropAll() (dropped int) {
	for _, dir := range []string{s.plansDir(), s.tracksDir()} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if os.Remove(filepath.Join(dir, e.Name())) == nil {
				dropped++
			}
		}
	}
	return dropped
}

// DedupeSheets removes plans and rendered songs beyond maxPerSheet
// copies of any one lyric sheet, keeping the oldest few of each. A
// backlog written before sheet reuse was capped can hold dozens of
// plans singing identical words - hours of the same song in different
// clothes - and nothing else ever revisits stored plans. Instrumentals
// and sheetless entries are left alone. Returns how many files were
// removed (a plan counts one, a rendered pair counts one).
func (s *Store) DedupeSheets(maxPerSheet int) int {
	removed := 0
	seen := map[string]int{}
	sheet := func(lyrics string) string {
		if lyrics == "" || lyrics == engine.InstrumentalLyrics {
			return ""
		}
		sum := sha256.Sum256([]byte(lyrics))
		return string(sum[:])
	}
	// Plans first, oldest first, so what survives is what plays first.
	for _, dir := range []string{s.plansDir(), s.tracksDir()} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") && !strings.HasSuffix(e.Name(), ".tmp") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			path := filepath.Join(dir, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var lyrics string
			if dir == s.plansDir() {
				var p struct {
					Plan struct{ Lyrics string } `json:"plan"`
				}
				if json.Unmarshal(raw, &p) != nil {
					continue
				}
				lyrics = p.Plan.Lyrics
			} else {
				var m trackMeta
				if json.Unmarshal(raw, &m) != nil {
					continue
				}
				lyrics = m.Lyrics
			}
			key := sheet(lyrics)
			if key == "" {
				continue
			}
			seen[key]++
			if seen[key] <= maxPerSheet {
				continue
			}
			os.Remove(path)
			if dir == s.tracksDir() {
				os.Remove(strings.TrimSuffix(path, ".json") + ".mp3")
			}
			removed++
		}
	}
	if removed > 0 {
		s.log.Info("duplicate lyric sheets swept from the buffer",
			"event", "buffer_sheets_deduped", "removed", removed)
	}
	return removed
}

func (s *Store) plansDir() string  { return filepath.Join(s.dir, "plans") }
func (s *Store) tracksDir() string { return filepath.Join(s.dir, "tracks") }

// name builds the sortable base name for an epoch/sequence pair.
func name(epoch, seq int) string { return fmt.Sprintf("e%08d-%08d", epoch, seq) }

// nameRe is the exact shape of a buffer base name; anything else -
// trailing garbage, path separators, dot segments - is rejected, which
// matters because Peek receives client-supplied names.
var nameRe = regexp.MustCompile(`^e([0-9]{8})-([0-9]{8})$`)

// parseName extracts epoch and sequence from a base name.
func parseName(base string) (epoch, seq int, ok bool) {
	m := nameRe.FindStringSubmatch(base)
	if m == nil {
		return 0, 0, false
	}
	epoch, _ = strconv.Atoi(m[1])
	seq, _ = strconv.Atoi(m[2])
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
	// Title/Subtitle are the resolved display names when they were
	// ready at render time; TitleKey lets later readers pick up a name
	// that resolved afterwards (the helper model can be slow).
	Title    string `json:"title,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	TitleKey string `json:"title_key,omitempty"`
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
// titleKey names the helper's pending title for this song, so a name
// that resolves after rendering is still picked up at feed time.
func (s *Store) PutTrack(ctx context.Context, epoch, seq int, titleKey string, t *engine.Track) error {
	if err := os.MkdirAll(s.tracksDir(), 0o755); err != nil {
		return err
	}
	base := filepath.Join(s.tracksDir(), name(epoch, seq))
	meta := trackMeta{
		Prompt:   t.Prompt,
		Lyrics:   t.Lyrics,
		Seconds:  float64(len(t.Samples)) / float64(audio.SampleRate*audio.Channels),
		Seed:     t.Seed,
		GenTime:  t.GenTime,
		Spec:     t.Spec,
		Created:  time.Now(),
		Title:    t.Title,
		Subtitle: t.Subtitle,
		TitleKey: titleKey,
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

// NextTrack decodes and removes the oldest rendered song of the epoch,
// returning its pending title key alongside. A song that cannot be
// decoded is dropped and the next one tried.
func (s *Store) NextTrack(ctx context.Context, epoch int) (*engine.Track, string, bool) {
	for {
		base, ok := s.oldest(s.tracksDir(), epoch, ".json")
		if !ok {
			return nil, "", false
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
		if ctx.Err() != nil {
			// A cancelled context fails every decode; deleting on that
			// would wipe the whole buffer during shutdown.
			return nil, "", false
		}
		os.Remove(metaPath)
		os.Remove(mp3)
		if err != nil || len(samples) == 0 {
			s.log.Warn("unplayable buffered track dropped", "event", "buffer_track_bad", "file", base,
				"error", fmt.Sprint(err))
			continue
		}
		return &engine.Track{
			Samples:  samples,
			Spec:     meta.Spec,
			Prompt:   meta.Prompt,
			Lyrics:   meta.Lyrics,
			Seed:     meta.Seed,
			GenTime:  meta.GenTime,
			Title:    meta.Title,
			Subtitle: meta.Subtitle,
		}, meta.TitleKey, true
	}
}

// DiskEpoch reports the highest epoch present among stored plans and
// songs, so a fresh process - whose in-memory epoch starts at zero -
// can adopt the buffer a previous run left behind instead of treating
// it as another context's.
func (s *Store) DiskEpoch() (int, bool) {
	best, found := 0, false
	for _, dir := range []string{s.plansDir(), s.tracksDir()} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			base := strings.TrimSuffix(name, filepath.Ext(name))
			if epoch, _, ok := parseName(base); ok {
				if !found || epoch > best {
					best, found = epoch, true
				}
			}
		}
	}
	return best, found
}

// SetTitle writes a late-resolved name into a rendered song's metadata,
// so listings and later runs show it, reporting whether it wrote. A
// song fed or dropped since it was listed is not an error: the write is
// skipped when the audio is already gone, and a re-check afterwards
// removes the metadata again if the audio vanished mid-write (a steer
// wiping the epoch), so no orphan survives the race.
func (s *Store) SetTitle(epoch int, base, title, subtitle string) bool {
	metaPath := filepath.Join(s.tracksDir(), base+".json")
	mp3Path := filepath.Join(s.tracksDir(), base+".mp3")
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return false
	}
	var m trackMeta
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	m.Title, m.Subtitle = title, subtitle
	out, err := json.Marshal(m)
	if err != nil {
		return false
	}
	if _, err := os.Stat(mp3Path); err != nil {
		return false
	}
	tmp := metaPath + ".tmp"
	if os.WriteFile(tmp, out, 0o644) != nil || os.Rename(tmp, metaPath) != nil {
		os.Remove(tmp)
		return false
	}
	if _, err := os.Stat(mp3Path); err != nil {
		os.Remove(metaPath)
		return false
	}
	return true
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

// Entry summarizes one rendered song awaiting play, without touching
// its audio.
type Entry struct {
	Base     string
	Prompt   string
	Lyrics   string
	Seconds  float64
	Spec     engine.Spec
	Title    string
	Subtitle string
	TitleKey string
}

// List returns the epoch's rendered songs in play order, metadata only.
func (s *Store) List(epoch int) []Entry {
	var out []Entry
	s.each(s.tracksDir(), epoch, ".json", func(path string) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var m trackMeta
		if json.Unmarshal(raw, &m) != nil {
			return
		}
		out = append(out, Entry{
			Base:     strings.TrimSuffix(filepath.Base(path), ".json"),
			Prompt:   m.Prompt,
			Lyrics:   m.Lyrics,
			Seconds:  m.Seconds,
			Spec:     m.Spec,
			Title:    m.Title,
			Subtitle: m.Subtitle,
			TitleKey: m.TitleKey,
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Base < out[j].Base })
	return out
}

// Peek decodes one rendered song by its base name without consuming it
// (remote listeners prefetch ahead of the local player). The base must
// parse as this epoch's naming; anything else is refused.
func (s *Store) Peek(ctx context.Context, epoch int, base string) (*engine.Track, bool) {
	ep, _, ok := parseName(base)
	if !ok || ep != epoch {
		return nil, false
	}
	raw, err := os.ReadFile(filepath.Join(s.tracksDir(), base+".json"))
	if err != nil {
		return nil, false
	}
	var meta trackMeta
	if json.Unmarshal(raw, &meta) != nil {
		return nil, false
	}
	samples, err := export.DecodePCM(ctx, filepath.Join(s.tracksDir(), base+".mp3"))
	if err != nil || len(samples) == 0 {
		return nil, false
	}
	return &engine.Track{
		Samples:  samples,
		Spec:     meta.Spec,
		Prompt:   meta.Prompt,
		Lyrics:   meta.Lyrics,
		Seed:     meta.Seed,
		GenTime:  meta.GenTime,
		Title:    meta.Title,
		Subtitle: meta.Subtitle,
	}, true
}
