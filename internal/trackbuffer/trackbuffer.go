// Package trackbuffer is the store of everything the generator makes -
// the tub. Plans the planner has written but the renderer has not
// turned into audio yet, and the rendered songs themselves, live as
// files under one directory: plans as JSON, songs as MP3 with a JSON
// sidecar, so a full store costs disk, not memory, and survives
// restarts.
//
// A song is not removed when a player takes it: taking marks it
// consumed and leaves it on disk for whatever other player has not
// caught up yet, until the store trims the oldest taken songs to keep
// its size bounded. The level - how many songs nobody has taken - is
// what the generator fills against.
package trackbuffer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"iar/internal/audio"
	"iar/internal/engine"
	"iar/internal/export"
)

// Store is the on-disk store. Safe for concurrent use: the file system
// is the state, and an in-memory index of the songs on disk answers
// every listing and count without a directory scan.
type Store struct {
	dir     string
	quality int
	log     *slog.Logger

	mu sync.Mutex
	// songs is every rendered song on disk by base name, read once and
	// kept current by every write. Bulk deletions that walk the
	// directory drop the index instead, and the next reader rebuilds it.
	songs  map[string]*trackMeta
	loaded bool
}

// New returns a store rooted at dir (created on first use). quality is
// the MP3 VBR quality for rendered songs (0 best..9 smallest).
func New(dir string, quality int, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{dir: dir, quality: quality, log: log}
}

// Dir returns the store's root directory.
func (s *Store) Dir() string { return s.dir }

// Context returns the steering-context key the store's content belongs
// to (empty when the store is fresh or from an older version).
func (s *Store) Context() string {
	raw, err := os.ReadFile(filepath.Join(s.dir, "context"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// RenderVersion identifies the render path that produced the songs in
// a store. Bump it whenever a fix changes what the renderer produces:
// songs already on disk were made by the old path and are dropped on
// the next start, while their plans - which the renderer reads, not
// writes - are kept and simply rendered again.
//
//	2: the deferred DiT silently discarded the planner's audio codes,
//	   so every phased render was conditioned on silence and came out
//	   with a 5 Hz comb across the whole song.
const RenderVersion = 2

// RenderVersionOf returns the render path that wrote this store's
// songs; 0 for a store from before the marker existed.
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

// SetRenderVersion records the render path that wrote this store.
func (s *Store) SetRenderVersion(v int) {
	s.writeMarker("render-version", strconv.Itoa(v))
}

// SetContext records the steering-context key the store now serves.
func (s *Store) SetContext(key string) {
	s.writeMarker("context", key)
}

// Cursor returns where a named player got to: the base name of the
// last song it took, "" when it has never taken one. A player that
// restarts continues after it rather than from the oldest song kept.
func (s *Store) Cursor(name string) string {
	raw, err := os.ReadFile(filepath.Join(s.dir, "cursor-"+name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// SetCursor records where a named player got to.
func (s *Store) SetCursor(name, base string) {
	s.writeMarker("cursor-"+name, base)
}

// Rung returns the rung of the batch ladder the generator had reached
// when it last closed one, so a restart carries on from there rather
// than climbing the ladder again over a store that is already deep. 0
// for a fresh store, one started over, or one from before the marker
// existed: the ladder's first rung.
func (s *Store) Rung() int {
	raw, err := os.ReadFile(filepath.Join(s.dir, "rung"))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// SetRung records the rung of the batch ladder the generator is on.
func (s *Store) SetRung(n int) {
	s.writeMarker("rung", strconv.Itoa(n))
}

// writeMarker writes one small state file atomically.
func (s *Store) writeMarker(name, value string) {
	if os.MkdirAll(s.dir, 0o755) != nil {
		return
	}
	tmp := filepath.Join(s.dir, "."+name+".tmp")
	if os.WriteFile(tmp, []byte(value+"\n"), 0o644) == nil {
		os.Rename(tmp, filepath.Join(s.dir, name))
	}
}

// DropTracks removes every rendered song, of any epoch, leaving the
// plans alone: the audio is unusable but the work that produced it is
// still good.
func (s *Store) DropTracks() (dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()
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

// DropAll removes every plan and rendered song regardless of epoch
// (the store belonged to a different steering context, or is being
// started over). The ladder's rung goes with them: whatever fills the
// store next starts from the first rung.
func (s *Store) DropAll() (dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()
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
	os.Remove(filepath.Join(s.dir, "rung"))
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
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()
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

// nameRe is the exact shape of a store base name; anything else -
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
	// ID is the song's own identity, minted when its audio is rendered
	// and kept for its whole life. Empty for songs rendered before
	// songs carried one.
	ID      string        `json:"id,omitempty"`
	Prompt  string        `json:"prompt"`
	Lyrics  string        `json:"lyrics,omitempty"`
	Seconds float64       `json:"seconds"`
	Seed    string        `json:"seed,omitempty"`
	GenTime time.Duration `json:"gen_time,omitempty"`
	Spec    engine.Spec   `json:"spec"`
	Created time.Time     `json:"created"`
	// Title/Subtitle are the song's display names, written with its
	// words and stored here with the audio, so a song on disk always
	// knows what it is called.
	Title    string `json:"title,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	// Hash is the SHA-256 (hex) of the MP3 as written. The file goes
	// out byte for byte and is never rewritten, so a copy of it hashes
	// to this wherever it turns up.
	Hash string `json:"hash,omitempty"`
	// Taken is when a player first took the song; zero while nobody
	// has. A taken song stays on disk for the other players.
	Taken time.Time `json:"taken,omitempty"`
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

// PutTrack encodes a rendered track to MP3 with a metadata sidecar,
// returning the file's hash. The song's name is part of that sidecar:
// it was decided when the words were written, so a song on disk is
// never nameless and nothing has to come back and name it.
func (s *Store) PutTrack(ctx context.Context, epoch, seq int, t *engine.Track) (string, error) {
	if err := os.MkdirAll(s.tracksDir(), 0o755); err != nil {
		return "", err
	}
	base := name(epoch, seq)
	path := filepath.Join(s.tracksDir(), base)
	meta := trackMeta{
		ID:       t.ID,
		Prompt:   t.Prompt,
		Lyrics:   t.Lyrics,
		Seconds:  float64(len(t.Samples)) / float64(audio.SampleRate*audio.Channels),
		Seed:     t.Seed,
		GenTime:  t.GenTime,
		Spec:     t.Spec,
		Created:  time.Now(),
		Title:    t.Title,
		Subtitle: t.Subtitle,
	}
	if err := export.EncodeMP3(ctx, t.Samples, path+".mp3", export.MP3Options{
		Quality: s.quality,
		Title:   t.Title,
		Artist:  "Infinite AI Radio",
		Comment: "buffered track",
	}); err != nil {
		return "", err
	}
	hash, err := fileHash(path + ".mp3")
	if err != nil {
		os.Remove(path + ".mp3")
		return "", err
	}
	meta.Hash = hash
	if err := s.writeMeta(base, &meta); err != nil {
		os.Remove(path + ".mp3")
		return "", err
	}
	s.mu.Lock()
	s.load()
	s.songs[base] = &meta
	s.mu.Unlock()
	return hash, nil
}

// writeMeta writes a song's sidecar atomically.
func (s *Store) writeMeta(base string, m *trackMeta) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	path := filepath.Join(s.tracksDir(), base+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// fileHash is the SHA-256 of a file's bytes, hex-encoded.
func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// load reads every song's sidecar into the index, once. Callers hold
// s.mu.
func (s *Store) load() {
	if s.loaded {
		return
	}
	s.songs = map[string]*trackMeta{}
	s.loaded = true
	entries, err := os.ReadDir(s.tracksDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".json")
		if _, _, ok := parseName(base); !ok {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.tracksDir(), e.Name()))
		if err != nil {
			continue
		}
		var m trackMeta
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.tracksDir(), base+".mp3")); err != nil {
			continue // half a song; Sweep collects it
		}
		s.songs[base] = &m
	}
}

// invalidate drops the index after a change that walked the directory.
// Callers hold s.mu.
func (s *Store) invalidate() {
	s.loaded = false
	s.songs = nil
}

// TrackPath returns the MP3 of a rendered song still on disk, for
// serving byte for byte. The base must parse as this epoch's naming;
// anything else is refused. The caller opens the file straight away:
// a trim can delete it, and an open descriptor is the only thing that
// keeps the bytes readable past that.
func (s *Store) TrackPath(epoch int, base string) (string, bool) {
	ep, _, ok := parseName(base)
	if !ok || ep != epoch {
		return "", false
	}
	s.mu.Lock()
	s.load()
	_, known := s.songs[base]
	s.mu.Unlock()
	if !known {
		return "", false
	}
	mp3 := filepath.Join(s.tracksDir(), base+".mp3")
	if _, err := os.Stat(mp3); err != nil {
		return "", false
	}
	return mp3, true
}

// Take marks a song as taken by a player, leaving it on disk. The mark
// is written into the sidecar so a restart still knows the level.
// Taking a song twice is nothing. Reports whether the song is here.
func (s *Store) Take(epoch int, base string) bool {
	ep, _, ok := parseName(base)
	if !ok || ep != epoch {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	m, known := s.songs[base]
	if !known {
		return false
	}
	if !m.Taken.IsZero() {
		return true
	}
	m.Taken = time.Now()
	if err := s.writeMeta(base, m); err != nil {
		s.log.Warn("taken mark not written", "event", "buffer_take_failed", "file", base, "error", err.Error())
	}
	return true
}

// Trim deletes the oldest taken songs of the epoch beyond keep, so the
// songs kept for players that have not caught up never outgrow the
// store. Untaken songs are never trimmed. Returns how many were removed.
func (s *Store) Trim(epoch, keep int) (dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	var taken []string
	for base, m := range s.songs {
		if ep, _, _ := parseName(base); ep == epoch && !m.Taken.IsZero() {
			taken = append(taken, base)
		}
	}
	sort.Strings(taken)
	for len(taken) > keep {
		base := taken[0]
		taken = taken[1:]
		os.Remove(filepath.Join(s.tracksDir(), base+".json"))
		os.Remove(filepath.Join(s.tracksDir(), base+".mp3"))
		delete(s.songs, base)
		dropped++
	}
	return dropped
}

// DropTrack removes one song (its audio turned out unplayable).
func (s *Store) DropTrack(epoch int, base string) {
	if ep, _, ok := parseName(base); !ok || ep != epoch {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	os.Remove(filepath.Join(s.tracksDir(), base+".json"))
	os.Remove(filepath.Join(s.tracksDir(), base+".mp3"))
	delete(s.songs, base)
}

// Next returns the first song of the epoch after the named one, in the
// order they were made ("" for the oldest song kept). A player walks
// the store with it, taken songs included: a player that has not
// caught up plays what the others already have before it takes
// anything new. A cursor from another epoch counts as none.
func (s *Store) Next(epoch int, after string) (Entry, bool) {
	if ep, _, ok := parseName(after); !ok || ep != epoch {
		after = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	best := ""
	for base := range s.songs {
		if ep, _, _ := parseName(base); ep != epoch || base <= after {
			continue
		}
		if best == "" || base < best {
			best = base
		}
	}
	if best == "" {
		return Entry{}, false
	}
	return s.entryLocked(best), true
}

// Build reads the build stamp of the binary that last owned the
// store; SetBuild records this binary's. A store made by another
// commit is treated as suspect wholesale - a newer build may have
// fixed the very bugs its songs were rendered with.
func (s *Store) Build() string {
	raw, err := os.ReadFile(filepath.Join(s.dir, "build"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// SetBuild records the running binary's build stamp.
func (s *Store) SetBuild(v string) {
	s.writeMarker("build", v)
}

// DiskEpoch reports the highest epoch present among stored plans and
// songs, so a fresh process - whose in-memory epoch starts at zero -
// can adopt the store a previous run left behind instead of treating
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

// SetTitle writes a new name into a rendered song's metadata (a
// listener renaming it), so listings and later runs show it, reporting
// whether it wrote. A song trimmed since it was listed is not an error.
func (s *Store) SetTitle(epoch int, base, title, subtitle string) bool {
	if ep, _, ok := parseName(base); !ok || ep != epoch {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	m, known := s.songs[base]
	if !known {
		return false
	}
	if _, err := os.Stat(filepath.Join(s.tracksDir(), base+".mp3")); err != nil {
		return false
	}
	m.Title, m.Subtitle = title, subtitle
	if err := s.writeMeta(base, m); err != nil {
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

// TrackStats reports how many rendered songs of the epoch are on disk,
// taken or not, and their total audio seconds.
func (s *Store) TrackStats(epoch int) (count int, seconds float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	for base, m := range s.songs {
		if ep, _, _ := parseName(base); ep == epoch {
			count++
			seconds += m.Seconds
		}
	}
	return count, seconds
}

// Level reports the songs of the epoch nobody has taken yet, and their
// audio seconds: what the generator fills against.
func (s *Store) Level(epoch int) (count int, seconds float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	for base, m := range s.songs {
		if ep, _, _ := parseName(base); ep == epoch && m.Taken.IsZero() {
			count++
			seconds += m.Seconds
		}
	}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()
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

// Entry summarizes one rendered song on disk, without touching its
// audio.
type Entry struct {
	Base string
	// ID is the song's own identity (see trackMeta.ID); empty for songs
	// rendered before songs carried one.
	ID       string
	Prompt   string
	Lyrics   string
	Seconds  float64
	Spec     engine.Spec
	Title    string
	Subtitle string
	// Hash is the SHA-256 of the MP3 on disk (see trackMeta.Hash).
	Hash string
	// Taken reports that some player has taken the song already.
	Taken bool
}

// entryLocked builds the listing row for one indexed song. Callers
// hold s.mu.
func (s *Store) entryLocked(base string) Entry {
	m := s.songs[base]
	return Entry{
		Base:     base,
		ID:       m.ID,
		Prompt:   m.Prompt,
		Lyrics:   m.Lyrics,
		Seconds:  m.Seconds,
		Spec:     m.Spec,
		Title:    m.Title,
		Subtitle: m.Subtitle,
		Hash:     m.Hash,
		Taken:    !m.Taken.IsZero(),
	}
}

// List returns the epoch's rendered songs in the order they were made,
// taken ones included, metadata only.
func (s *Store) List(epoch int) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	var out []Entry
	for base := range s.songs {
		if ep, _, _ := parseName(base); ep == epoch {
			out = append(out, s.entryLocked(base))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Base < out[j].Base })
	return out
}

// Peek decodes one rendered song by its base name without taking it.
// The base must parse as this epoch's naming; anything else is refused.
func (s *Store) Peek(ctx context.Context, epoch int, base string) (*engine.Track, bool) {
	ep, _, ok := parseName(base)
	if !ok || ep != epoch {
		return nil, false
	}
	s.mu.Lock()
	s.load()
	m, known := s.songs[base]
	var meta trackMeta
	if known {
		meta = *m
	}
	s.mu.Unlock()
	if !known {
		return nil, false
	}
	samples, err := export.DecodePCM(ctx, filepath.Join(s.tracksDir(), base+".mp3"))
	if err != nil || len(samples) == 0 {
		return nil, false
	}
	return &engine.Track{
		ID:       meta.ID,
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
