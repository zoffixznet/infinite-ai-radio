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
	"unicode"
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

// slugRe and fileRe validate path components coming from clients. File
// names may carry any script's letters (a Tagalog or Russian title
// keeps its own words), but only letters, digits and [._-], never a
// path separator.
var (
	slugRe = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
	fileRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}._-]*\.mp3$`)
	// langRe is the optional language segment before the extension
	// ("...-title.tl.mp3"): a short lowercase engine language tag.
	langRe  = regexp.MustCompile(`\.([a-z]{2,3})\.mp3$`)
	langTag = regexp.MustCompile(`^[a-z]{2,3}$`)
	dashRun = regexp.MustCompile(`-+`)
)

// ValidRef reports whether a tag/file pair names a plausible chunk
// (directory-safe components only).
func ValidRef(tag, file string) bool {
	return slugRe.MatchString(tag) && fileRe.MatchString(file) && !strings.Contains(file, "..")
}

// TitleSlug turns a track title into the file-name part of a chunk:
// letters and digits of any script survive (lowercased), everything
// else joins as single dashes.
func TitleSlug(title string) string {
	var b strings.Builder
	runes := 0
	for _, r := range strings.ToLower(title) {
		if runes >= 48 {
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
		runes++
	}
	slug := strings.Trim(dashRun.ReplaceAllString(b.String(), "-"), "-")
	if slug == "" {
		return "track"
	}
	return slug
}

// FileName builds a chunk file name: timestamp, a slug of the track's
// title, and the sung language's tag when one is known - so what the
// interface shows is what the disk says, findable by name and by
// language.
func FileName(title, lang string, now time.Time) string {
	if !langTag.MatchString(lang) {
		lang = ""
	}
	if lang != "" {
		return fmt.Sprintf("%s-%s.%s.mp3", now.Format("20060102-150405"), TitleSlug(title), lang)
	}
	return fmt.Sprintf("%s-%s.mp3", now.Format("20060102-150405"), TitleSlug(title))
}

// Path returns where a chunk for tag, title and language goes under dir.
func Path(dir, tag, title, lang string, now time.Time) string {
	return filepath.Join(dir, Slug(tag), FileName(title, lang, now))
}

// Language reports the language tag encoded in a chunk file name, empty
// when none is.
func Language(file string) string {
	if m := langRe.FindStringSubmatch(file); m != nil {
		return m[1]
	}
	return ""
}

// LyricsSidecar is the text file that carries a chunk's full lyrics,
// next to the MP3 with the same base name.
func LyricsSidecar(path string) string {
	return strings.TrimSuffix(path, ".mp3") + ".txt"
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
	// Language is the sung language's engine tag, read from the file
	// name; empty for instrumental tracks and saves from before the
	// language was recorded.
	Language string `json:"language,omitempty"`
	// Lyrics is the full lyric sheet from the sidecar text file; empty
	// when none was saved.
	Lyrics string `json:"lyrics,omitempty"`
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
				ch.Language = Language(f.Name())
				if raw, err := os.ReadFile(LyricsSidecar(path)); err == nil {
					ch.Lyrics = string(raw)
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

// tsPrefixRe matches the timestamp prefix every saved file starts with.
var tsPrefixRe = regexp.MustCompile(`^\d{8}-\d{6}-`)

// Delete removes a chunk and its lyrics sidecar.
func (c *Catalog) Delete(tag, file string) error {
	path, ok := c.Resolve(tag, file)
	if !ok {
		return errors.New("no such saved track")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	// The sidecar is best-effort on the way out too.
	os.Remove(LyricsSidecar(path))
	return nil
}

// Move relocates a chunk (and its sidecar) into the directory for
// newTag - free text; the directory is created on first use, which is
// how new tag groups come to exist. Returns the slug it landed in.
func (c *Catalog) Move(tag, file, newTag string) (string, error) {
	path, ok := c.Resolve(tag, file)
	if !ok {
		return "", errors.New("no such saved track")
	}
	slug := Slug(newTag)
	if slug == tag {
		return slug, nil
	}
	destDir := filepath.Join(c.dir, slug)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(destDir, file)
	if _, err := os.Stat(dest); err == nil {
		return "", errors.New("a track with this file name already exists under that tag")
	}
	if err := os.Rename(path, dest); err != nil {
		return "", err
	}
	if _, err := os.Stat(LyricsSidecar(path)); err == nil {
		os.Rename(LyricsSidecar(path), LyricsSidecar(dest))
	}
	return slug, nil
}

// Retitle renames a chunk's files to match a new title, keeping the
// timestamp prefix and the language segment. It moves only the file
// names; rewriting the MP3's own title tag is the caller's job (it
// needs an encoder). Returns the new file name.
func (c *Catalog) Retitle(tag, file, title string) (string, error) {
	path, ok := c.Resolve(tag, file)
	if !ok {
		return "", errors.New("no such saved track")
	}
	suffix := ".mp3"
	if lang := Language(file); lang != "" {
		suffix = "." + lang + ".mp3"
	}
	newFile := tsPrefixRe.FindString(file) + TitleSlug(title) + suffix
	if newFile == file {
		return file, nil
	}
	dest := filepath.Join(filepath.Dir(path), newFile)
	if _, err := os.Stat(dest); err == nil {
		return "", errors.New("a track with this name already exists under that tag")
	}
	if err := os.Rename(path, dest); err != nil {
		return "", err
	}
	if _, err := os.Stat(LyricsSidecar(path)); err == nil {
		os.Rename(LyricsSidecar(path), LyricsSidecar(dest))
	}
	return newFile, nil
}
