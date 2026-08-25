// Package snippets manages the tracks a listener saves while listening:
// the on-disk layout (one directory per tag), tag slugs, migration of
// older flat layouts, and a catalog that lists saved chunks with their
// titles and durations read back from the MP3 files.
package snippets

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Untagged is the tag a save without a tag lands in.
const Untagged = "untagged"

// MaxSlugLength bounds a tag slug.
const MaxSlugLength = 40

// Slug turns a free-text tag into a directory-safe name: every
// character outside [A-Za-z0-9_] becomes "_", lowercase, leading and
// trailing underscores trimmed, at most MaxSlugLength characters, and
// an empty result becomes Untagged. Only [a-z0-9_] can survive, so a
// slug can never escape the snippets directory.
func Slug(tag string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(tag) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), "_")
	if len(s) > MaxSlugLength {
		s = strings.TrimRight(s[:MaxSlugLength], "_")
	}
	if s == "" {
		return Untagged
	}
	return s
}

// slugRe and fileRe validate path components coming from clients.
var (
	slugRe = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
	fileRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.mp3$`)
)

// ValidRef reports whether a tag/file pair names a plausible chunk
// (directory-safe components only).
func ValidRef(tag, file string) bool {
	return slugRe.MatchString(tag) && fileRe.MatchString(file) && !strings.Contains(file, "..")
}

// FileName builds a chunk file name: timestamp plus a slug of the prompt.
func FileName(prompt string, now time.Time) string {
	var b strings.Builder
	for _, r := range strings.ToLower(prompt) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == ',' || r == '.':
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(regexp.MustCompile(`-+`).ReplaceAllString(b.String(), "-"), "-")
	if len(slug) > 60 {
		slug = strings.TrimRight(slug[:60], "-")
	}
	if slug == "" {
		slug = "track"
	}
	return fmt.Sprintf("%s-%s.mp3", now.Format("20060102-150405"), slug)
}

// Path returns where a chunk for tag and prompt goes under dir.
func Path(dir, tag, prompt string, now time.Time) string {
	return filepath.Join(dir, Slug(tag), FileName(prompt, now))
}

// Migrate moves MP3 files lying directly in dir (the layout before tags
// existed) into the Untagged subdirectory. It is idempotent and returns
// how many files moved.
func Migrate(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	moved := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".mp3") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if moved == 0 {
			if err := os.MkdirAll(filepath.Join(dir, Untagged), 0o755); err != nil {
				return 0, err
			}
		}
		if err := os.Rename(filepath.Join(dir, e.Name()), filepath.Join(dir, Untagged, e.Name())); err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}

// Chunk is one saved track.
type Chunk struct {
	// Tag is the slug directory the chunk lives in.
	Tag string `json:"tag"`
	// File is the file name inside the tag directory.
	File string `json:"file"`
	// Title is the track's short name (from the ID3 title; older saves
	// carry the raw prompt there).
	Title string `json:"title"`
	// Subtitle is the genre/mood line (from the ID3 TIT3 frame).
	Subtitle string `json:"subtitle,omitempty"`
	// Seconds is the play time.
	Seconds float64 `json:"seconds"`
	// Saved is when the chunk was written.
	Saved time.Time `json:"saved"`
	// Bytes is the file size.
	Bytes int64 `json:"bytes"`
}

// cacheKey identifies a file version.
type cacheKey struct {
	path string
	mod  time.Time
	size int64
}

// Catalog lists the chunks under a snippets directory, caching the
// per-file tag reads by modification time and size.
type Catalog struct {
	dir string

	mu    sync.Mutex
	cache map[cacheKey]Chunk
}

// NewCatalog returns a catalog over dir.
func NewCatalog(dir string) *Catalog {
	return &Catalog{dir: dir, cache: map[cacheKey]Chunk{}}
}

// Dir returns the snippets directory.
func (c *Catalog) Dir() string { return c.dir }

// List returns every chunk, newest first. A missing directory yields an
// empty list; unreadable files are skipped.
func (c *Catalog) List() ([]Chunk, error) {
	tags, err := os.ReadDir(c.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fresh := map[cacheKey]Chunk{}
	var out []Chunk
	for _, t := range tags {
		if !t.IsDir() || !slugRe.MatchString(t.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(c.dir, t.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !fileRe.MatchString(f.Name()) {
				continue
			}
			path := filepath.Join(c.dir, t.Name(), f.Name())
			fi, err := f.Info()
			if err != nil {
				continue
			}
			key := cacheKey{path: path, mod: fi.ModTime(), size: fi.Size()}
			ch, ok := c.cache[key]
			if !ok {
				ch = Chunk{Tag: t.Name(), File: f.Name(), Saved: fi.ModTime(), Bytes: fi.Size()}
				info, err := ReadInfo(path)
				if err != nil {
					continue
				}
				ch.Title = info.Title
				ch.Subtitle = info.Subtitle
				ch.Seconds = info.Duration.Seconds()
				if ch.Title == "" {
					ch.Title = strings.TrimSuffix(f.Name(), ".mp3")
				}
			}
			fresh[key] = ch
			out = append(out, ch)
		}
	}
	c.cache = fresh
	sort.Slice(out, func(i, j int) bool { return out[i].Saved.After(out[j].Saved) })
	return out, nil
}

// Resolve maps a client-supplied tag/file pair to a path inside the
// snippets directory, refusing anything that is not a plain chunk.
func (c *Catalog) Resolve(tag, file string) (string, bool) {
	if !ValidRef(tag, file) {
		return "", false
	}
	path := filepath.Join(c.dir, tag, file)
	if rel, err := filepath.Rel(c.dir, path); err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	return path, true
}
