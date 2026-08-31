package remote

import (
	"context"
	"mime"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"iar/internal/accounts"
	"iar/internal/export"
	"iar/internal/prompting"
	"iar/internal/snippets"
)

// chunkJSON is one saved chunk as the page sees it.
type chunkJSON struct {
	snippets.Chunk
	// URL is the authenticated, range-capable file route.
	URL string `json:"url"`
	// LanguageName is the sung language in words ("Tagalog"); empty
	// when the file name carries no language tag.
	LanguageName string `json:"language_name,omitempty"`
}

// chunksJSON is the listing payload: every chunk, newest first, plus the
// tags that have at least one chunk.
type chunksJSON struct {
	Chunks []chunkJSON `json:"chunks"`
	Tags   []string    `json:"tags"`
}

func (s *Server) handleChunks(w http.ResponseWriter, r *http.Request, u accounts.User) {
	list, err := s.catalog.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := chunksJSON{Chunks: []chunkJSON{}, Tags: []string{}}
	seen := map[string]bool{}
	for _, ch := range list {
		out.Chunks = append(out.Chunks, chunkJSON{
			Chunk:        ch,
			URL:          "/chunks/" + url.PathEscape(ch.Tag) + "/" + url.PathEscape(ch.File),
			LanguageName: prompting.LanguageName(ch.Language),
		})
		if !seen[ch.Tag] {
			seen[ch.Tag] = true
			out.Tags = append(out.Tags, ch.Tag)
		}
	}
	sort.Strings(out.Tags)
	writeJSON(w, http.StatusOK, out)
}

// handleChunkFile serves one saved MP3 with range support, so seeking
// works in the browser.
func (s *Server) handleChunkFile(w http.ResponseWriter, r *http.Request, u accounts.User) {
	path, ok := s.catalog.Resolve(r.PathValue("tag"), r.PathValue("file"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if r.URL.Query().Get("dl") == "1" {
		// A download keeps the on-disk name, which is the title in its
		// own script; FormatMediaType handles the non-ASCII encoding.
		w.Header().Set("Content-Disposition",
			mime.FormatMediaType("attachment", map[string]string{"filename": fi.Name()}))
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

// chunkRef reads and validates the tag/file pair of a chunk request.
func chunkRef(w http.ResponseWriter, r *http.Request) (tag, file string, ok bool) {
	tag, file = textField(r, "tag"), textField(r, "file")
	if !snippets.ValidRef(tag, file) {
		http.Error(w, "no such saved track", http.StatusBadRequest)
		return "", "", false
	}
	return tag, file, true
}

// handleChunkRename gives a saved track a new title: the MP3's own
// title tag is rewritten (stream copy, no re-encode) and the files are
// renamed to the new title's slug, keeping timestamp and language.
func (s *Server) handleChunkRename(w http.ResponseWriter, r *http.Request, u accounts.User) {
	tag, file, ok := chunkRef(w, r)
	if !ok {
		return
	}
	title := textFieldN(r, "title", 120)
	if title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	path, ok := s.catalog.Resolve(tag, file)
	if !ok {
		http.Error(w, "no such saved track", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := export.RetitleMP3(ctx, path, title); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	newFile, err := s.catalog.Retitle(tag, file, title)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.log.Info("saved track renamed", "event", "remote_chunk_rename", "by", u.Email,
		"tag", tag, "file", file, "new_file", newFile, "title", title)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tag": tag, "file": newFile})
}

// handleChunkMove moves one or more saved tracks into a tag group -
// free text; a new group's directory is created on first use. Items
// arrive as repeated "item" fields of the form "tag/file".
func (s *Server) handleChunkMove(w http.ResponseWriter, r *http.Request, u accounts.User) {
	to := textField(r, "to")
	if to == "" {
		http.Error(w, "a destination tag is required", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	items := r.PostForm["item"]
	if len(items) == 0 {
		http.Error(w, "nothing to move", http.StatusBadRequest)
		return
	}
	moved, failed := 0, []string{}
	slug := ""
	for _, item := range items {
		tag, file, found := strings.Cut(item, "/")
		if !found || !snippets.ValidRef(tag, file) {
			failed = append(failed, item)
			continue
		}
		dest, err := s.catalog.Move(tag, file, to)
		if err != nil {
			failed = append(failed, item)
			continue
		}
		slug = dest
		moved++
	}
	s.log.Info("saved tracks moved", "event", "remote_chunk_move", "by", u.Email,
		"to", slug, "moved", moved, "failed", len(failed))
	writeJSON(w, http.StatusOK, map[string]any{"ok": len(failed) == 0, "moved": moved, "tag": slug, "failed": failed})
}

// handleChunkDelete removes one saved track (and its lyrics file).
func (s *Server) handleChunkDelete(w http.ResponseWriter, r *http.Request, u accounts.User) {
	tag, file, ok := chunkRef(w, r)
	if !ok {
		return
	}
	if err := s.catalog.Delete(tag, file); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.log.Info("saved track deleted", "event", "remote_chunk_delete", "by", u.Email, "tag", tag, "file", file)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
