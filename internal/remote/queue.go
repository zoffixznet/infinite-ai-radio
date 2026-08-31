package remote

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"iar/internal/accounts"
	"iar/internal/export"
)

// queueTrackJSON is one prefetchable track in the queue listing.
type queueTrackJSON struct {
	ID        string  `json:"id"`
	Prompt    string  `json:"prompt"`
	Title     string  `json:"title,omitempty"`
	Subtitle  string  `json:"subtitle,omitempty"`
	DurationS float64 `json:"duration_s"`
	Kind      string  `json:"kind"`
	Lyrics    string  `json:"lyrics,omitempty"`
	// URL is the authenticated, range-capable MP3 route for the track.
	URL string `json:"url"`
}

// queueJSON is the upcoming-tracks listing a prefetching client polls.
type queueJSON struct {
	Epoch  int              `json:"epoch"`
	Tracks []queueTrackJSON `json:"tracks"`
}

func (s *Server) handleQueueList(w http.ResponseWriter, r *http.Request, u accounts.User) {
	epoch, tracks := s.ctl.QueueTracks()
	out := queueJSON{Epoch: epoch, Tracks: []queueTrackJSON{}}
	for _, t := range tracks {
		out.Tracks = append(out.Tracks, queueTrackJSON{
			ID: t.ID, Prompt: t.Prompt, Title: t.Title, Subtitle: t.Subtitle,
			DurationS: t.Seconds, Kind: t.Kind, Lyrics: t.Lyrics,
			URL: "/queue/" + url.PathEscape(t.ID) + ".mp3",
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleQueueTrack serves one queued or library track as MP3 with Range
// support. Encoding happens in the request handler (never on the
// playback path) and results are cached so several phones prefetching
// the same track encode it once.
func (s *Server) handleQueueTrack(w http.ResponseWriter, r *http.Request, u accounts.User) {
	id, ok := strings.CutSuffix(r.PathValue("file"), ".mp3")
	if !ok || id == "" {
		http.NotFound(w, r)
		return
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
	w.Header().Set("Cache-Control", "private, max-age=3600")
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
