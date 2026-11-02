package remote

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// logSink keeps the server's log lines, under a lock, since the server
// writes them from its own goroutines.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// lines is every line logged with the event.
func (l *logSink) lines(event string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, ln := range strings.Split(l.buf.String(), "\n") {
		if strings.Contains(ln, "event="+event) {
			out = append(out, ln)
		}
	}
	return out
}

// The phone notes each command the car or the system sends, and the
// note is a line in the radio's log and nothing else: written with the
// account that sent it and its text cut to two hundred letters, under
// any login at all, thirty a minute an account with the rest dropped
// without a word, and refused without a login.
func TestRemoteNoteIsALineInTheLog(t *testing.T) {
	h := newHarness(t, nil)
	sink := &logSink{}
	h.s.log = slog.New(slog.NewTextHandler(sink, nil))
	admin := h.admin()
	listener := h.activate(h.invite(admin, "listener@example.com", nil), "listen-pass-1")

	long := strings.Repeat("é", 250)
	resp, body := listener.postAPI("/note", url.Values{"text": {long}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("a listener's note: %d %s", resp.StatusCode, body)
	}
	got := sink.lines("remote_note")
	if len(got) != 1 {
		t.Fatalf("one note wrote %d lines: %q", len(got), got)
	}
	cut := strings.Repeat("é", 200)
	if !strings.Contains(got[0], "user=listener@example.com") || !strings.Contains(got[0], cut) || strings.Contains(got[0], cut+"é") {
		t.Errorf("the note was logged as %q; want the account and the text cut to 200 letters", got[0])
	}
	if len(h.ctl.notes) != 0 {
		t.Errorf("the note went through the player: %q", h.ctl.notes)
	}

	resp, _ = listener.postAPI("/note", url.Values{"text": {""}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an empty note: %d; want 400", resp.StatusCode)
	}

	// The thirtieth line in the minute is written, the thirty-first
	// dropped, and both are answered as if written; another account
	// is not held back by it.
	for i := 2; i <= 31; i++ {
		resp, body = listener.postAPI("/note", url.Values{"text": {"seekto (live) to 70.0s from 60.0s: button-forward"}})
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"ok":true`) {
			t.Fatalf("note %d: %d %s", i, resp.StatusCode, body)
		}
	}
	if got := sink.lines("remote_note"); len(got) != 30 {
		t.Errorf("31 notes in a minute wrote %d lines; want 30", len(got))
	}
	resp, _ = admin.postAPI("/note", url.Values{"text": {"play (live)"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the admin's note: %d", resp.StatusCode)
	}
	if got := sink.lines("remote_note"); len(got) != 31 || !strings.Contains(got[30], "user=admin@example.com") {
		t.Errorf("another account's note after the first's cap: %d lines, last %q; want 31 with the admin's last", len(got), got[len(got)-1])
	}

	anon := h.client()
	resp, _ = anon.postAPI("/note", url.Values{"text": {"play (live)"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a note without a login: %d; want 401", resp.StatusCode)
	}
	if got := sink.lines("remote_note"); len(got) != 31 {
		t.Errorf("a note without a login was written: %d lines", len(got))
	}
}
