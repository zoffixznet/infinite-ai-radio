package remote

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"iar/internal/player"
)

// postRaw posts a body as the page does for an upload: the bytes
// themselves, with the API header and no form encoding.
func (c *client) postRaw(path, contentType string, body []byte) (*http.Response, string) {
	c.h.t.Helper()
	req, _ := http.NewRequest("POST", c.h.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(csrfHeader, "1")
	resp, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(out)
}

// A song still on disk goes out as the file itself, byte for byte,
// with ranges honoured; one that is not is encoded as before.
func TestAStoredSongIsServedByteForByte(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	path := filepath.Join(t.TempDir(), "t-1.mp3")
	data := bytes.Repeat([]byte("mp3-bytes-"), 1000)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	h.ctl.mu.Lock()
	h.ctl.storedT1 = path
	h.ctl.mu.Unlock()

	resp, body := admin.get("/queue/t-1.mp3")
	if resp.StatusCode != 200 || body != string(data) {
		t.Fatalf("/queue/t-1.mp3 = %d, %d bytes (want the file's %d)", resp.StatusCode, len(body), len(data))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	// Downloading is taking.
	h.ctl.mu.Lock()
	takes := append([]string(nil), h.ctl.takes...)
	h.ctl.mu.Unlock()
	if len(takes) != 1 || takes[0] != "t-1" {
		t.Fatalf("takes after a download = %v, want t-1", takes)
	}

	req, _ := http.NewRequest("GET", h.srv.URL+"/queue/t-1.mp3", nil)
	req.Header.Set("Range", "bytes=10-19")
	part, err := admin.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(part.Body)
	part.Body.Close()
	if part.StatusCode != 206 || string(got) != string(data[10:20]) {
		t.Fatalf("range = %d %q", part.StatusCode, got)
	}

	// t-2 is only in memory: encoded on the way out, as before.
	resp, body = admin.get("/queue/t-2.mp3")
	if resp.StatusCode != 200 || !(strings.HasPrefix(body, "ID3") || body[0] == 0xff) {
		t.Fatalf("/queue/t-2.mp3 = %d, starts %q", resp.StatusCode, body[:min(4, len(body))])
	}
}

// The listing carries each song's hash; a save of a song the radio no
// longer holds asks for the device's copy; and the copy is taken on
// its own route, tag and hash in the query, the file as the body.
func TestSavingFromTheDevicesCopy(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()

	_, body := admin.get("/api/queue")
	var q struct {
		Tracks []struct {
			ID   string `json:"id"`
			Hash string `json:"hash"`
		} `json:"tracks"`
	}
	if err := json.Unmarshal([]byte(body), &q); err != nil {
		t.Fatal(err)
	}
	hashed := false
	for _, row := range q.Tracks {
		if row.ID == "t-1" && row.Hash == "hash-of-t-1" {
			hashed = true
		}
	}
	if !hashed {
		t.Fatalf("listing rows = %+v, want t-1 with its hash", q.Tracks)
	}

	resp, body := admin.postAPI("/save", url.Values{"which": {"t-gone"}, "tag": {"gym"}})
	var ans struct {
		OK     bool   `json:"ok"`
		Ack    string `json:"ack"`
		Saved  bool   `json:"saved"`
		Upload bool   `json:"upload"`
	}
	if err := json.Unmarshal([]byte(body), &ans); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !ans.OK || ans.Saved || !ans.Upload || ans.Ack != player.AckSendCopy {
		t.Fatalf("/save of a gone song = %d %s", resp.StatusCode, body)
	}

	copyBytes := bytes.Repeat([]byte("mp3-"), 500)
	resp, body = admin.postRaw("/save/upload?hash=hash-of-t-1&tag=gym", "audio/mpeg", copyBytes)
	ans.Saved, ans.Upload = false, false
	if err := json.Unmarshal([]byte(body), &ans); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !ans.OK || !ans.Saved || ans.Upload {
		t.Fatalf("/save/upload = %d %s", resp.StatusCode, body)
	}
	h.ctl.mu.Lock()
	ups := append([]upload(nil), h.ctl.uploads...)
	h.ctl.mu.Unlock()
	if len(ups) != 1 || ups[0].hash != "hash-of-t-1" || ups[0].tag != "gym" || ups[0].size != len(copyBytes) {
		t.Fatalf("uploads = %+v", ups)
	}

	// The route is guarded like /save: no login, no upload.
	anon := h.client()
	if resp, _ := anon.postRaw("/save/upload?tag=gym", "audio/mpeg", copyBytes); resp.StatusCode != 401 {
		t.Fatalf("anonymous /save/upload = %d", resp.StatusCode)
	}
}

// A save that reaches the radio as it shuts down goes out as the
// service being away rather than as a final no: the page keeps the
// save queued and delivers it once the radio is back. Every other
// answer goes out as before.
func TestASaveDuringShutdownIsAnsweredAsTheRadioBeingAway(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	var ans struct {
		OK    bool   `json:"ok"`
		Ack   string `json:"ack"`
		Saved bool   `json:"saved"`
	}
	resp, body := admin.postAPI("/save", url.Values{"which": {"t-closing"}, "tag": {"gym"}})
	if err := json.Unmarshal([]byte(body), &ans); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || ans.Saved || ans.Ack != player.AckClosing {
		t.Fatalf("/save during shutdown = %d %s, want 503 carrying the answer", resp.StatusCode, body)
	}
	resp, body = admin.postRaw("/save/upload?hash=hash-closing&tag=gym", "audio/mpeg", []byte("mp3-mp3-"))
	ans.Saved = false
	if err := json.Unmarshal([]byte(body), &ans); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || ans.Saved || ans.Ack != player.AckClosing {
		t.Fatalf("/save/upload during shutdown = %d %s, want 503 carrying the answer", resp.StatusCode, body)
	}
	if resp, _ := admin.postAPI("/save", url.Values{"which": {"t-1"}, "tag": {"gym"}}); resp.StatusCode != 200 {
		t.Fatalf("/save of a song the radio can take = %d", resp.StatusCode)
	}
}

// Every answer that means the track is in the snippets or on its way
// there greys the page's save control: written, being written, or
// waiting its turn behind another save. The rest do not.
func TestSavedAckKnowsEveryKeptAnswer(t *testing.T) {
	kept := []string{
		"saving this track to gym/20260924-101500-song.mp3",
		"saving this track to gym/20260924-101500-song.mp3, behind 1 other save",
		"saving this track to gym/20260924-101500-song.mp3, behind 3 other saves",
		"already saved: that track is in your snippets",
		"already saving: that track is on its way to your snippets",
		"track saved: gym/20260924-101500-song.mp3",
	}
	for _, ack := range kept {
		if !savedAck(ack) {
			t.Errorf("%q does not count as saved", ack)
		}
	}
	notKept := []string{
		"",
		player.AckSendCopy,
		"nothing to save yet: no generated track is playing",
		"no previous track to save yet",
		"that track is no longer available to save",
		"saving the track failed: no space left on device",
		player.AckClosing,
		"that copy is not a song this radio made",
	}
	for _, ack := range notKept {
		if savedAck(ack) {
			t.Errorf("%q counts as saved", ack)
		}
	}
}
