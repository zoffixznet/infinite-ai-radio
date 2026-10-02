package remote

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The listing a phone polls every ten seconds carries only what places
// a song and names it: no description, which is most of a listing's
// bytes and is read once from the song's own JSON. It goes out under
// a tag naming the store's state, a client sending the tag back is
// told "unchanged" with no body, a change to the store changes the
// tag, and a client that takes gzip gets the listing compressed.
func TestListingIsSlimTaggedAndCompressed(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()

	get := func(headers map[string]string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", h.srv.URL+"/api/queue", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := admin.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	read := func(resp *http.Response) string {
		t.Helper()
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(body)
	}

	resp := get(nil)
	body := read(resp)
	if resp.StatusCode != 200 {
		t.Fatalf("/api/queue = %d %s", resp.StatusCode, body)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, `"7-0-`) {
		t.Fatalf("listing ETag = %q, want one naming epoch 7 and version 0", etag)
	}
	if resp.Header.Get("Vary") != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", resp.Header.Get("Vary"))
	}
	var rows struct {
		Tracks []map[string]any `json:"tracks"`
	}
	if err := json.Unmarshal([]byte(body), &rows); err != nil || len(rows.Tracks) != 3 {
		t.Fatalf("listing = %s (%v)", body, err)
	}
	for _, row := range rows.Tracks {
		if _, has := row["prompt"]; has {
			t.Errorf("listing row carries a description: %v", row)
		}
		for _, key := range []string{"id", "title", "duration_s", "url"} {
			if _, has := row[key]; !has {
				t.Errorf("listing row lacks %s: %v", key, row)
			}
		}
	}
	if _, has := rows.Tracks[1]["hash"]; !has {
		t.Errorf("the listed song with a hash lost it: %v", rows.Tracks[1])
	}

	// The description is at the song's own route.
	_, song := admin.get("/queue/t-1.json")
	var detail struct {
		Prompt   string  `json:"prompt"`
		Title    string  `json:"title"`
		Subtitle string  `json:"subtitle"`
		Hash     string  `json:"hash"`
		Duration float64 `json:"duration_s"`
		Lyrics   string  `json:"lyrics"`
	}
	if err := json.Unmarshal([]byte(song), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Prompt != "dark techno, driving" || detail.Title != "Dark Techno" || detail.Subtitle != "driving" ||
		detail.Hash != "hash-of-t-1" || detail.Duration != 2 || detail.Lyrics == "" {
		t.Errorf("song json = %+v, want description, names, hash, length and words", detail)
	}

	// The tag sent back: unchanged, no body, the tag repeated.
	resp = get(map[string]string{"If-None-Match": etag})
	body = read(resp)
	if resp.StatusCode != http.StatusNotModified || body != "" || resp.Header.Get("ETag") != etag {
		t.Fatalf("with the tag sent back: %d, %d bytes, ETag %q; want 304, nothing, the same tag",
			resp.StatusCode, len(body), resp.Header.Get("ETag"))
	}
	// A cache may weaken the tag; it still names the listing.
	if resp = get(map[string]string{"If-None-Match": "W/" + etag}); resp.StatusCode != http.StatusNotModified {
		t.Errorf("a weakened tag got %d, want 304", resp.StatusCode)
	}
	read(resp)

	// The store changed: the old tag is stale, the listing comes whole.
	h.ctl.bump()
	resp = get(map[string]string{"If-None-Match": etag})
	body = read(resp)
	if resp.StatusCode != 200 || !strings.Contains(body, `"tracks"`) {
		t.Fatalf("after a change the old tag got %d %q, want the listing", resp.StatusCode, body)
	}
	next := resp.Header.Get("ETag")
	if next == etag || !strings.HasPrefix(next, `"7-1-`) {
		t.Errorf("after a change the tag is %q, want a new one at version 1 (was %q)", next, etag)
	}

	// Compressed on request. Asking explicitly keeps the Go client
	// from unwrapping it on the way in.
	resp = get(map[string]string{"Accept-Encoding": "gzip"})
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", resp.Header.Get("Content-Encoding"))
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(gz)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(plain, &rows); err != nil || len(rows.Tracks) != 3 {
		t.Fatalf("gunzipped listing = %s (%v)", plain, err)
	}
	// Not when the client declines it.
	resp = get(map[string]string{"Accept-Encoding": "gzip;q=0, identity"})
	if resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("a client refusing gzip got Content-Encoding %q", resp.Header.Get("Content-Encoding"))
	}
	read(resp)
	// A 304 for a gzip-taking client is still a 304.
	resp = get(map[string]string{"Accept-Encoding": "gzip", "If-None-Match": next})
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("gzip client with the current tag got %d, want 304", resp.StatusCode)
	}
	read(resp)
}
