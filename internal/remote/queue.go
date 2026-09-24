package remote

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"iar/internal/accounts"
	"iar/internal/export"
	"iar/internal/player"
)

// queueTrackJSON is one song in the store, as the listing offers it.
// The lyrics are not here - a listing of a hundred songs would carry
// a hundred lyric sheets ten times a minute - but at the song's own
// JSON route, which a client reads once, when it takes the song.
type queueTrackJSON struct {
	ID        string  `json:"id"`
	Prompt    string  `json:"prompt"`
	Title     string  `json:"title,omitempty"`
	Subtitle  string  `json:"subtitle,omitempty"`
	DurationS float64 `json:"duration_s"`
	// Taken reports some player has taken the song already; the store
	// keeps it for the ones that have not caught up.
	Taken bool `json:"taken,omitempty"`
	// URL is the authenticated, range-capable MP3 route for the song.
	URL string `json:"url"`
	// Hash is the SHA-256 of the MP3 the URL serves, when the radio
	// recorded one; the page sends it back to save from its own copy.
	Hash string `json:"hash,omitempty"`
}

// songJSON is one song with everything a client shows about it.
type songJSON struct {
	queueTrackJSON
	Lyrics string `json:"lyrics,omitempty"`
}

// queueJSON is the store's listing a client polls.
type queueJSON struct {
	Epoch  int              `json:"epoch"`
	Tracks []queueTrackJSON `json:"tracks"`
}

func rowJSON(t player.QueueTrack) queueTrackJSON {
	return queueTrackJSON{
		ID: t.ID, Prompt: t.Prompt, Title: t.Title, Subtitle: t.Subtitle,
		DurationS: t.Seconds, Taken: t.Taken,
		URL: "/queue/" + url.PathEscape(t.ID) + ".mp3", Hash: t.Hash,
	}
}

func (s *Server) handleQueueList(w http.ResponseWriter, r *http.Request, u accounts.User) {
	// Every check for new songs carries the seconds skipped with Next
	// since the last one; skipping is faster consumption, and the
	// generator's next batch comes that much sooner.
	if v, err := strconv.ParseFloat(r.URL.Query().Get("skipped"), 64); err == nil && v > 0 && v < 24*60*60 {
		s.ctl.ReportSkipped(v)
	}
	epoch, tracks := s.ctl.QueueTracks()
	out := queueJSON{Epoch: epoch, Tracks: []queueTrackJSON{}}
	for _, t := range tracks {
		out.Tracks = append(out.Tracks, rowJSON(t))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleQueueSong serves one song's details, lyrics included.
func (s *Server) handleQueueSong(w http.ResponseWriter, r *http.Request, id string) {
	row, lyrics, ok := s.ctl.Song(id)
	if !ok {
		http.Error(w, "no such song", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, songJSON{queueTrackJSON: rowJSON(row), Lyrics: lyrics})
}

// handleQueueTrack serves one listed track as MP3 with Range
// support. A song still on disk goes out as the file itself, byte for
// byte, so the hash the radio recorded for it is the hash of what the
// phone holds. One that is only in memory is encoded in the request
// handler (never on the playback path), and cached so several phones
// prefetching the same track encode it once.
func (s *Server) handleQueueTrack(w http.ResponseWriter, r *http.Request, u accounts.User) {
	if id, ok := strings.CutSuffix(r.PathValue("file"), ".json"); ok && id != "" {
		s.handleQueueSong(w, r, id)
		return
	}
	id, ok := strings.CutSuffix(r.PathValue("file"), ".mp3")
	if !ok || id == "" {
		http.NotFound(w, r)
		return
	}
	if path, ok := s.ctl.TrackFile(id); ok {
		// Opened before anything else: a trim can delete the file, and
		// the open descriptor is what keeps the bytes.
		if f, err := os.Open(path); err == nil {
			defer f.Close()
			// Downloading is taking: the song is this client's now, and
			// the store's level drops by one.
			s.ctl.Take(id)
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Header().Set("Cache-Control", "no-store")
			http.ServeContent(w, r, id+".mp3", time.Time{}, f)
			return
		}
	}
	data, err := s.trackMP3.get(id, func() ([]byte, error) {
		track, found := s.ctl.TrackData(id)
		if !found {
			return nil, errTrackGone
		}
		title := track.Title
		if title == "" {
			title = track.Prompt
		}
		return export.EncodeMP3Bytes(r.Context(), track.Samples, export.MP3Options{
			Title: title, Subtitle: track.Subtitle, Artist: ProductName, Album: ProductName,
			Comment: track.Prompt,
		})
	})
	if err != nil {
		// Evicted or steered away: the client drops the entry and moves on.
		http.Error(w, "track no longer available", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	// The phone keeps its own copy of every track it banks, so a second
	// copy in the browser's cache buys nothing - and costs correctness:
	// a flushed device re-requesting a song was handed it straight back
	// out of that cache, with the radio switched off, which made Flush
	// look like it had done nothing at all.
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, id+".mp3", time.Time{}, bytes.NewReader(data))
}

// errTrackGone marks an id that no longer resolves to audio.
var errTrackGone = &trackGoneError{}

type trackGoneError struct{}

func (*trackGoneError) Error() string { return "track gone" }

// mp3Cache keeps recently encoded tracks in memory with single-flight
// encoding per id.
type mp3Cache struct {
	mu       sync.Mutex
	entries  map[string][]byte
	order    []string // oldest first
	inflight map[string]chan struct{}
}

// mp3CacheSlots bounds the cache (tracks are a few MB each).
const mp3CacheSlots = 8

func newMP3Cache() *mp3Cache {
	return &mp3Cache{entries: map[string][]byte{}, inflight: map[string]chan struct{}{}}
}

// get returns the cached bytes for id, encoding them with fill on a
// miss. Concurrent requests for the same id share one encode.
func (c *mp3Cache) get(id string, fill func() ([]byte, error)) ([]byte, error) {
	for {
		c.mu.Lock()
		if data, ok := c.entries[id]; ok {
			c.mu.Unlock()
			return data, nil
		}
		wait, busy := c.inflight[id]
		if !busy {
			done := make(chan struct{})
			c.inflight[id] = done
			c.mu.Unlock()
			data, err := fill()
			c.mu.Lock()
			delete(c.inflight, id)
			close(done)
			if err != nil {
				c.mu.Unlock()
				return nil, err
			}
			c.entries[id] = data
			c.order = append(c.order, id)
			for len(c.order) > mp3CacheSlots {
				delete(c.entries, c.order[0])
				c.order = c.order[1:]
			}
			c.mu.Unlock()
			return data, nil
		}
		c.mu.Unlock()
		<-wait
	}
}
