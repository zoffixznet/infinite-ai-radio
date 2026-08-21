package remote

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"bgm/internal/player"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- tailnet detection ---

func addr(cidr string) net.Addr {
	ip, ipnet, _ := net.ParseCIDR(cidr)
	ipnet.IP = ip
	return ipnet
}

func TestTailnetAddrDetection(t *testing.T) {
	cases := []struct {
		name  string
		addrs []net.Addr
		want  string
		ok    bool
	}{
		{"typical machine with tailnet",
			[]net.Addr{addr("127.0.0.1/8"), addr("192.168.1.20/24"), addr("100.101.102.103/32")},
			"100.101.102.103", true},
		{"no tailnet",
			[]net.Addr{addr("127.0.0.1/8"), addr("10.0.0.5/8"), addr("172.16.3.4/12")},
			"", false},
		{"100.x outside the CGNAT range is not a tailnet",
			[]net.Addr{addr("100.1.2.3/8")},
			"", false},
		{"range edges",
			[]net.Addr{addr("100.64.0.1/10")},
			"100.64.0.1", true},
		{"upper edge outside",
			[]net.Addr{addr("100.128.0.1/10")},
			"", false},
		{"empty list", nil, "", false},
	}
	for _, tc := range cases {
		got, ok := tailnetAddrIn(tc.addrs)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got %q/%v want %q/%v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestBindAddrsOverride(t *testing.T) {
	addrs, ip := BindAddrs("0.0.0.0", 9999)
	if len(addrs) != 1 || addrs[0] != "0.0.0.0:9999" || ip != "" {
		t.Fatalf("override binding = %v / %q", addrs, ip)
	}
	addrs, _ = BindAddrs("", 9999)
	if addrs[0] != "127.0.0.1:9999" {
		t.Fatalf("default binding must start with localhost: %v", addrs)
	}
}

// --- fan-out ---

func TestFanoutSlowClientDoesNotStallOthers(t *testing.T) {
	s := NewStreamer(testLog())
	_, fast, cancelFast := s.Subscribe()
	defer cancelFast()
	_, slow, cancelSlow := s.Subscribe()
	defer cancelSlow()

	// Fill well past the slow client's queue without reading it.
	chunk := make([]byte, 512)
	chunk[0] = 0xFF
	chunk[1] = 0xFB
	var fastGot int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range fast {
			fastGot++
		}
	}()
	for i := 0; i < clientChanSlots*2; i++ {
		s.broadcast(chunk)
	}
	if got := s.Listeners(); got != 1 {
		t.Fatalf("slow client not dropped: %d listeners", got)
	}
	cancelFast()
	<-done
	if fastGot < clientChanSlots {
		t.Fatalf("fast client starved: got %d chunks", fastGot)
	}
	// The slow client's channel must be closed (drained reader sees EOF).
	if _, ok := <-slow; ok {
		for range slow {
		}
	}
}

func TestFanoutPreBufferAlignsToFrame(t *testing.T) {
	s := NewStreamer(testLog())
	// Garbage, then a frame boundary mid-buffer.
	s.broadcast([]byte{0x00, 0x11, 0x22})
	s.broadcast([]byte{0x33, 0xFF, 0xFB, 0x90, 0x00, 0x01})
	pre, _, cancel := s.Subscribe()
	defer cancel()
	if len(pre) == 0 || pre[0] != 0xFF || pre[1]&0xE0 != 0xE0 {
		t.Fatalf("pre-buffer not frame aligned: % x", pre)
	}
}

func TestFanoutDisconnectCleanup(t *testing.T) {
	s := NewStreamer(testLog())
	var cancels []func()
	for i := 0; i < 5; i++ {
		_, _, c := s.Subscribe()
		cancels = append(cancels, c)
	}
	if s.Listeners() != 5 {
		t.Fatalf("listeners = %d", s.Listeners())
	}
	for _, c := range cancels {
		c()
		c() // double-cancel must be safe
	}
	if s.Listeners() != 0 {
		t.Fatalf("listeners after cancel = %d", s.Listeners())
	}
}

func TestStreamerWriteNeverBlocks(t *testing.T) {
	s := NewStreamer(testLog())
	// No encoder running: writes must still return instantly.
	buf := make([]byte, 19200)
	doneCh := make(chan struct{})
	go func() {
		for i := 0; i < feedSlots*3; i++ {
			s.Write(buf)
		}
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked without an encoder")
	}
}

// --- handlers ---

// fakeCtl records control calls.
type fakeCtl struct {
	mu     sync.Mutex
	steers []string
	news   []string
	saves  []string
	notes  []string
}

func (f *fakeCtl) Steer(text string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steers = append(f.steers, text)
	return "steering with: " + text
}

func (f *fakeCtl) NewSession(prompt string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.news = append(f.news, prompt)
	return "new session: " + prompt
}

func (f *fakeCtl) SaveSnippet(which string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves = append(f.saves, which)
	return "saving this track to /tmp/x.mp3"
}

func (f *fakeCtl) Status() player.Status {
	return player.Status{State: "playing", Source: "test prompt", Session: "s1", Volume: 70}
}

func (f *fakeCtl) Announce(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, text)
}

func testServer(t *testing.T, token string) (*httptest.Server, *fakeCtl) {
	t.Helper()
	ctl := &fakeCtl{}
	s := &Server{cfg: Config{Token: token}, ctl: ctl, streamer: NewStreamer(testLog()), log: testLog()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handlePage)
	mux.HandleFunc("GET /state", s.handleState)
	mux.HandleFunc("POST /steer", s.handleSteer)
	mux.HandleFunc("POST /new", s.handleNew)
	mux.HandleFunc("POST /save", s.handleSave)
	srv := httptest.NewServer(s.gate(mux))
	t.Cleanup(srv.Close)
	return srv, ctl
}

func postForm(t *testing.T, u string, vals url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := http.PostForm(u, vals)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func TestHandlersRoundTrip(t *testing.T) {
	srv, ctl := testServer(t, "")

	resp, body := postForm(t, srv.URL+"/steer", url.Values{"text": {"calmer"}})
	if resp.StatusCode != 200 || !strings.Contains(body, "steering with: calmer") {
		t.Fatalf("steer: %d %s", resp.StatusCode, body)
	}
	resp, body = postForm(t, srv.URL+"/new", url.Values{"prompt": {"dark techno"}})
	if resp.StatusCode != 200 || !strings.Contains(body, "new session: dark techno") {
		t.Fatalf("new: %d %s", resp.StatusCode, body)
	}
	resp, body = postForm(t, srv.URL+"/save", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "/tmp/x.mp3") {
		t.Fatalf("save: %d %s", resp.StatusCode, body)
	}
	if len(ctl.steers) != 1 || len(ctl.news) != 1 || len(ctl.saves) != 1 {
		t.Fatalf("controller calls: %+v", ctl)
	}
	if len(ctl.notes) != 3 {
		t.Fatalf("remote actions must be announced to the local UI: %v", ctl.notes)
	}

	// Empty inputs are rejected.
	resp, _ = postForm(t, srv.URL+"/steer", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty steer accepted: %d", resp.StatusCode)
	}

	// State JSON.
	r2, err := http.Get(srv.URL + "/state")
	if err != nil {
		t.Fatal(err)
	}
	var st stateJSON
	json.NewDecoder(r2.Body).Decode(&st)
	r2.Body.Close()
	if st.Source != "test prompt" || st.State != "playing" || st.Volume != 70 {
		t.Fatalf("state = %+v", st)
	}

	// Page contains every control.
	r3, _ := http.Get(srv.URL + "/")
	page, _ := io.ReadAll(r3.Body)
	r3.Body.Close()
	for _, want := range []string{"stream.mp3", `id="steer"`, `id="fresh"`, `id="save"`, `id="text"`, `id="now"`, "viewport"} {
		if !strings.Contains(string(page), want) {
			t.Fatalf("page missing %q", want)
		}
	}
}

func TestTokenGate(t *testing.T) {
	srv, _ := testServer(t, "sesame")

	resp, _ := http.Get(srv.URL + "/state")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token accepted: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, _ = http.Get(srv.URL + "/state?token=wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token accepted: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, _ = http.Get(srv.URL + "/state?token=sesame")
	if resp.StatusCode != 200 {
		t.Fatalf("query token rejected: %d", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/state", nil)
	req.Header.Set("Authorization", "Bearer sesame")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("bearer token rejected: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestStreamerEncodesRealMP3(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewStreamer(testLog())
	if err := s.Start(ctx); err != nil {
		t.Skipf("ffmpeg unavailable: %v", err)
	}
	_, ch, cancelSub := s.Subscribe()
	defer cancelSub()
	// Feed one second of a tone.
	pcm := make([]byte, 192000)
	for i := 0; i < len(pcm); i += 4 {
		pcm[i] = byte(i)
		pcm[i+1] = byte(i >> 6)
	}
	for i := 0; i < len(pcm); i += 19200 {
		s.Write(pcm[i : i+19200])
	}
	var got []byte
	deadline := time.After(5 * time.Second)
	for len(got) < 8000 {
		select {
		case chunk := <-ch:
			got = append(got, chunk...)
		case <-deadline:
			t.Fatalf("only %d MP3 bytes arrived", len(got))
		}
	}
	if idx := strings.Index(string(got), "\xff"); idx < 0 {
		t.Fatal("no MP3 sync byte in output")
	}
}
