package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Store persists sessions as JSON files, one per session, in a directory.
type Store struct {
	dir string
}

// NewStore returns a store rooted at dir (created on first save).
func NewStore(dir string) *Store { return &Store{dir: dir} }

var nameRe = regexp.MustCompile(`[^a-z0-9._-]+`)

// SanitizeName turns free text into a safe session name.
func SanitizeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = nameRe.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-.")
	if name == "" {
		name = "unnamed"
	}
	return name
}

// Save writes the session atomically (temp file + rename). The session is
// stored under its sanitized name; the session itself is not modified.
func (st *Store) Save(s *Session) error {
	if err := os.MkdirAll(st.dir, 0o755); err != nil {
		return err
	}
	name := SanitizeName(s.Name)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	final := st.path(name)
	tmp, err := os.CreateTemp(st.dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), final)
}

// ErrNotFound is returned when a session name is unknown.
var ErrNotFound = errors.New("session not found")

// Load reads a saved session by name.
func (st *Store) Load(name string) (*Session, error) {
	data, err := os.ReadFile(st.path(SanitizeName(name)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("session file %s is corrupt: %w", name, err)
	}
	if s.Name == "" {
		s.Name = SanitizeName(name)
	}
	if s.Mode == "" {
		s.Mode = ModeMusic
	}
	return &s, nil
}

// Delete removes a saved session. ErrNotFound reports a name that had
// no file.
func (st *Store) Delete(name string) error {
	err := os.Remove(st.path(SanitizeName(name)))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return err
}

// Sweep deletes auto-named sessions whose last play is older than
// retention, never the session named keep. It returns the names removed.
// A non-positive retention disables the sweep.
func (st *Store) Sweep(now time.Time, retention time.Duration, keep string) ([]string, error) {
	if retention <= 0 {
		return nil, nil
	}
	sessions, err := st.List()
	if err != nil {
		return nil, err
	}
	keep = SanitizeName(keep)
	var removed []string
	for _, s := range sessions {
		if !s.AutoNamed() || s.Name == keep || now.Sub(s.Played()) <= retention {
			continue
		}
		if err := st.Delete(s.Name); err != nil && !errors.Is(err, ErrNotFound) {
			return removed, err
		}
		removed = append(removed, s.Name)
	}
	return removed, nil
}

// tombstoneFile lists deleted presets, one name per line. It has no
// .json suffix so List never mistakes it for a session.
func (st *Store) tombstoneFile() string { return filepath.Join(st.dir, "deleted-presets") }

// HiddenPresets returns the names of deleted (tombstoned) presets.
func (st *Store) HiddenPresets() map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(st.tombstoneFile())
	if err != nil {
		return out
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if name := SanitizeName(scanner.Text()); name != "" && name != "unnamed" {
			out[name] = true
		}
	}
	return out
}

// ErrPresetDeleted reports a preset hidden by a tombstone.
var ErrPresetDeleted = errors.New("preset was deleted (restore with: iar sessions restore-presets)")

// HidePreset records a tombstone so the preset disappears from every
// listing and cannot be started until RestorePresets.
func (st *Store) HidePreset(name string) error {
	p, err := LookupPreset(name)
	if err != nil {
		return err
	}
	hidden := st.HiddenPresets()
	if hidden[p.Name] {
		return nil
	}
	hidden[p.Name] = true
	return st.writeTombstones(hidden)
}

// RestorePresets removes every tombstone and reports how many there were.
func (st *Store) RestorePresets() (int, error) {
	n := len(st.HiddenPresets())
	err := os.Remove(st.tombstoneFile())
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return n, err
}

func (st *Store) writeTombstones(hidden map[string]bool) error {
	if err := os.MkdirAll(st.dir, 0o755); err != nil {
		return err
	}
	names := make([]string, 0, len(hidden))
	for n := range hidden {
		names = append(names, n)
	}
	sort.Strings(names)
	tmp, err := os.CreateTemp(st.dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strings.Join(names, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), st.tombstoneFile())
}

// Presets returns the built-in presets minus the deleted ones.
func (st *Store) Presets() []*Preset {
	hidden := st.HiddenPresets()
	var out []*Preset
	for _, p := range Presets() {
		if !hidden[p.Name] {
			out = append(out, p)
		}
	}
	return out
}

// LookupPreset finds a preset by name, refusing deleted ones.
func (st *Store) LookupPreset(name string) (*Preset, error) {
	p, err := LookupPreset(name)
	if err != nil {
		return nil, err
	}
	if st.HiddenPresets()[p.Name] {
		return nil, fmt.Errorf("%s: %w", p.Name, ErrPresetDeleted)
	}
	return p, nil
}

// List returns the names of all readable saved sessions, newest first.
// Corrupt files are skipped, never fatal.
func (st *Store) List() ([]*Session, error) {
	entries, err := os.ReadDir(st.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		s, err := st.Load(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue // tolerate corrupt files
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

func (st *Store) path(name string) string {
	return filepath.Join(st.dir, name+".json")
}
