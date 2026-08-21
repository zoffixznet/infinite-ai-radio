package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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

// Delete removes a saved session file if it exists.
func (st *Store) Delete(name string) {
	os.Remove(st.path(SanitizeName(name)))
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
