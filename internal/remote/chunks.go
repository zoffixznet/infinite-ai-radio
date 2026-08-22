package remote

import (
	"net/http"
	"net/url"
	"os"
	"sort"

	"iar/internal/accounts"
	"iar/internal/snippets"
)

// chunkJSON is one saved chunk as the page sees it.
type chunkJSON struct {
	snippets.Chunk
	// URL is the authenticated, range-capable file route.
	URL string `json:"url"`
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
		out.Chunks = append(out.Chunks, chunkJSON{Chunk: ch, URL: "/chunks/" + url.PathEscape(ch.Tag) + "/" + url.PathEscape(ch.File)})
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
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}
