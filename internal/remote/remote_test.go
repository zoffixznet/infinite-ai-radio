package remote

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"iar/internal/accounts"
	"iar/internal/audio"
	"iar/internal/engine"
	"iar/internal/export"
	"iar/internal/player"
	"iar/internal/session"
	"iar/internal/snippets"
)

func init() { accounts.Cost = bcrypt.MinCost }

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

// withFakeInterfaces swaps the interface enumeration for a test.
func withFakeInterfaces(t *testing.T, addrs []net.Addr) {
	t.Helper()
	old := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) { return addrs, nil }
	t.Cleanup(func() { interfaceAddrs = old })
}

func TestResolveBindingAdditive(t *testing.T) {
	withFakeInterfaces(t, []net.Addr{
		addr("127.0.0.1/8"), addr("192.168.1.42/24"), addr("100.101.102.103/32"),
	})

	// Default: localhost plus the detected tailnet address.
	b := ResolveBinding(nil, 9999)
	if len(b.Addrs) != 2 || b.Addrs[0] != "127.0.0.1:9999" || b.Addrs[1] != "100.101.102.103:9999" {
		t.Fatalf("default binding = %v", b.Addrs)
	}
	if b.TailnetIP != "100.101.102.103" || b.Exposed {
		t.Fatalf("default binding meta = %+v", b)
	}

	// An extra LAN entry is ADDITIVE: localhost and tailnet stay bound,
	// the entry is exposed and auto-allowed.
	b = ResolveBinding([]string{"192.168.1.42"}, 9999)
	want := []string{"127.0.0.1:9999", "100.101.102.103:9999", "192.168.1.42:9999"}
	if strings.Join(b.Addrs, " ") != strings.Join(want, " ") {
		t.Fatalf("additive binding = %v; want %v", b.Addrs, want)
	}
	if !b.Exposed {
		t.Fatal("LAN entry must count as exposed")
	}
	found := false
	for _, h := range b.ExtraHosts {
		if h == "192.168.1.42" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bound entry not auto-allowed: %v", b.ExtraHosts)
	}

	// A wildcard collapses the listens (it covers everything) and
	// auto-allows every machine interface address.
	b = ResolveBinding([]string{"0.0.0.0"}, 9999)
	if len(b.Addrs) != 1 || b.Addrs[0] != "0.0.0.0:9999" || !b.Exposed {
		t.Fatalf("wildcard binding = %+v", b)
	}
	allowed := strings.Join(b.ExtraHosts, " ")
	if !strings.Contains(allowed, "192.168.1.42") || !strings.Contains(allowed, "100.101.102.103") {
		t.Fatalf("wildcard must auto-allow machine addresses: %v", b.ExtraHosts)
	}
	if b.TailnetIP != "100.101.102.103" {
		t.Fatalf("tailnet detection lost under wildcard: %+v", b)
	}

	// A loopback-only extra entry stays unexposed.
	b = ResolveBinding([]string{"127.0.0.1", "localhost"}, 9999)
	if b.Exposed {
		t.Fatalf("loopback entries flagged exposed: %+v", b)
	}
}

// --- fan-out ---

func TestFanoutSlowClientResyncsAndStaysSubscribed(t *testing.T) {
	s := NewStreamer(testLog())
	_, fast, cancelFast := s.Subscribe()
	defer cancelFast()
	_, slow, cancelSlow := s.Subscribe()
	defer cancelSlow()

	mkChunk := func(i int) []byte {
		c := make([]byte, 512)
		c[0] = 0xFF
		c[1] = 0xFB
		c[2] = byte(i >> 8)
		c[3] = byte(i)
		return c
	}
	idx := func(c []byte) int { return int(c[2])<<8 | int(c[3]) }
	var fastGot int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range fast {
			fastGot++
		}
	}()
	total := clientChanSlots * 2
	for i := 0; i < total; i++ {
		s.broadcast(mkChunk(i))
	}
	// The slow client is resynced (oldest chunks dropped), never dropped.
	if got := s.Listeners(); got != 2 {
		t.Fatalf("slow client was disconnected: %d listeners", got)
	}
	cancelFast()
	<-done
	if fastGot < clientChanSlots {
		t.Fatalf("fast client starved: got %d chunks", fastGot)
	}
	first := -1
	last := -1
	for {
		select {
		case c := <-slow:
			if first < 0 {
				first = idx(c)
			}
			last = idx(c)
			continue
		default:
		}
		break
	}
	if first <= 0 {
		t.Fatalf("no old chunks were dropped for the slow client (first=%d)", first)
	}
	if last != total-1 {
		t.Fatalf("slow client not at live edge: last=%d want %d", last, total-1)
	}
}

func TestFanoutPreBufferAlignsToFrame(t *testing.T) {
	s := NewStreamer(testLog())
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
		c()
	}
	if s.Listeners() != 0 {
		t.Fatalf("listeners after cancel = %d", s.Listeners())
	}
}

func TestStreamerWriteNeverBlocks(t *testing.T) {
	s := NewStreamer(testLog())
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

func TestStreamerEncodesRealMP3(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewStreamer(testLog())
	if err := s.Start(ctx); err != nil {
		t.Skipf("ffmpeg unavailable: %v", err)
	}
	_, ch, cancelSub := s.Subscribe()
	defer cancelSub()
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

// --- test harness ---

// fakeCtl records control calls.
type fakeCtl struct {
	mu      sync.Mutex
	steers  []string
	skips   int
	loops   int
	news    []string
	saves   [][2]string
	lyrGens []string
	langs   []player.LanguageState
	langSet [][2]string
	langAll [][]string
	notes   []string
	named   []string
	loaded  []string
	deleted []string
}

func (f *fakeCtl) Steer(text string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steers = append(f.steers, text)
	return "steering with: " + text
}

func (f *fakeCtl) LyricsGen(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lyrGens = append(f.lyrGens, name)
	return "lyric writer: " + name
}

func (f *fakeCtl) Languages() []player.LanguageState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]player.LanguageState(nil), f.langs...)
}

func (f *fakeCtl) SetLanguage(name string, on bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := "off"
	if on {
		state = "on"
	}
	f.langSet = append(f.langSet, [2]string{name, state})
	for i, l := range f.langs {
		if l.Name == name {
			f.langs[i].On = on
		}
	}
	return "singing in " + name
}

func (f *fakeCtl) SetLanguages(names []string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.langAll = append(f.langAll, append([]string(nil), names...))
	f.langs = nil
	for _, n := range names {
		f.langs = append(f.langs, player.LanguageState{Name: n, Engine: true, On: true})
	}
	return "languages set"
}

// languagesSet returns the most recent list handed to SetLanguages.
func (f *fakeCtl) languagesSet() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.langAll) == 0 {
		return nil
	}
	return f.langAll[len(f.langAll)-1]
}

func (f *fakeCtl) Skip() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.skips++
	return "skipping to the next track"
}

func (f *fakeCtl) ToggleLoop() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loops++
	return "looping this track until the loop is turned off"
}

func (f *fakeCtl) NewSession(prompt string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.news = append(f.news, prompt)
	return "new session: " + prompt
}

func (f *fakeCtl) SaveSnippet(which, tag string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves = append(f.saves, [2]string{which, tag})
	return "saving this track to " + tag + "/x.mp3"
}

func (f *fakeCtl) Status() player.Status {
	return player.Status{
		State: "playing", Source: "test prompt", Session: "s1", Volume: 70,
		Epoch: 7, BasePrompt: "dark techno", Vocal: true, LyricsGenerator: "scribe",
		SessionDesc: "dark techno +2 tweaks vocals",
		Tweaks: []session.Entry{
			{Time: time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC), Raw: "less guitars", Interpreted: "fewer guitars"},
			{Time: time.Date(2026, 8, 23, 10, 1, 0, 0, time.UTC), Raw: "faster", Interpreted: "faster tempo"},
		},
		TrackID: "t-1", TrackPrompt: "dark techno, driving", Duration: 150 * time.Second,
		TrackTitle: "Dark Techno", TrackSubtitle: "driving", TrackNum: 3,
		TrackSaved: false, PrevTrackID: "t-0", PrevTrackPrompt: "dark techno, opening", PrevTrackSaved: true,
		SavedTrackIDs: []string{"t-0"},
		TrackLanguage: "Bisaya (Cebuano)", Switching: true, Looping: true,
		TrackLyrics: "[Verse]\nnaay usa ka gabii",
	}
}

func (f *fakeCtl) NameSession(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.named = append(f.named, name)
	return "session saved as " + name
}

func (f *fakeCtl) LoadByName(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded = append(f.loaded, name)
	return "loaded " + name
}

func (f *fakeCtl) DeleteSession(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	return "session " + name + " deleted"
}

func (f *fakeCtl) CurrentName() string { return "s1" }

func (f *fakeCtl) Listing() session.Listing {
	now := time.Now()
	mkSess := func(name string, named bool, ago time.Duration) *session.Session {
		s := session.New()
		s.Name, s.Named = name, named
		s.Created, s.Updated, s.LastPlayed = now.Add(-ago-time.Hour), now.Add(-ago), now.Add(-ago)
		return s
	}
	return session.Group([]*session.Session{
		mkSess("gym-grind", true, 3*time.Hour),
		mkSess("s1", true, 0),
		mkSess("session-20260821-100000", false, 26*time.Hour),
	}, session.Presets())
}

// queueTracks are what QueueTracks serves; tests may replace them.
func (f *fakeCtl) QueueTracks() (int, []player.QueueTrack) {
	return 7, []player.QueueTrack{
		{ID: "t-1", Prompt: "dark techno, driving", Title: "Dark Techno", Subtitle: "driving", Seconds: 2, Kind: "queue"},
		{ID: "t-2", Prompt: "dark techno, deeper", Title: "Deep Descent", Subtitle: "deeper", Seconds: 2, Kind: "queue"},
		{ID: "lib:techno/20260823-000000-0001", Prompt: "banked techno", Title: "Banked Techno", Seconds: 2, Kind: "library"},
	}
}

func (f *fakeCtl) TrackData(id string) (*engine.Track, bool) {
	switch id {
	case "t-1", "t-2", "lib:techno/20260823-000000-0001":
	default:
		return nil, false
	}
	samples := make([]int16, 2*audio.SampleRate*audio.Channels)
	for i := 0; i < len(samples); i += 2 {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i/2)/audio.SampleRate))
		samples[i], samples[i+1] = v, v
	}
	return &engine.Track{ID: id, Samples: samples, Prompt: "dark techno, driving"}, true
}

func (f *fakeCtl) Announce(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, text)
}

// fakeMailer captures sent messages.
type fakeMailer struct {
	mu   sync.Mutex
	sent []string // "to|subject|body"
	fail bool
}

func (m *fakeMailer) Configured() bool { return true }
func (m *fakeMailer) Send(ctx context.Context, to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return fmt.Errorf("smtp down")
	}
	m.sent = append(m.sent, to+"|"+subject+"|"+body)
	return nil
}

// harness is a server under httptest with account stores in a temp dir.
type harness struct {
	t      *testing.T
	srv    *httptest.Server
	s      *Server
	ctl    *fakeCtl
	users  *accounts.Store
	mailer *fakeMailer
	dir    string
}

func newHarness(t *testing.T, mailer Mailer) *harness {
	t.Helper()
	dir := t.TempDir()
	users := accounts.NewStore(filepath.Join(dir, "remote", "users.json"))
	sessions := accounts.NewSessions(filepath.Join(dir, "remote", "sessions.json"), time.Hour)
	ctl := &fakeCtl{}
	cfg := Config{Users: users, Sessions: sessions, Mailer: mailer, SnippetsDir: filepath.Join(dir, "snippets")}
	s, err := newServer(cfg, ctl, NewStreamer(testLog()), testLog())
	if err != nil {
		t.Fatal(err)
	}
	s.buildAllowedHosts(nil)
	handler := s.buildHandler()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	// The middleware validates Host:port, so align the config port with
	// the ephemeral httptest port.
	u, _ := url.Parse(srv.URL)
	s.cfg.Port, _ = strconv.Atoi(u.Port())
	h := &harness{t: t, srv: srv, s: s, ctl: ctl, users: users, dir: dir}
	if fm, ok := mailer.(*fakeMailer); ok {
		h.mailer = fm
	}
	return h
}

// client is an authenticated browser-like client (cookie jar, no
// redirects followed, same-origin Origin on posts).
type client struct {
	h    *harness
	http *http.Client
}

func (h *harness) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{h: h, http: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
}

func (c *client) get(path string) (*http.Response, string) {
	c.h.t.Helper()
	resp, err := c.http.Get(c.h.srv.URL + path)
	if err != nil {
		c.h.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

// postForm posts the way a browser form does: with the same-origin
// Origin header and no custom header.
func (c *client) postForm(path string, vals url.Values) (*http.Response, string) {
	c.h.t.Helper()
	req, _ := http.NewRequest("POST", c.h.srv.URL+path, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", c.h.srv.URL)
	resp, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

// postAPI posts the way the page's JavaScript does: custom header, no
// Origin (fetch adds one in browsers; the header alone must suffice).
func (c *client) postAPI(path string, vals url.Values) (*http.Response, string) {
	c.h.t.Helper()
	req, _ := http.NewRequest("POST", c.h.srv.URL+path, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(csrfHeader, "1")
	resp, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

// login authenticates the client.
func (c *client) login(email, password string) *http.Response {
	c.h.t.Helper()
	resp, body := c.postForm("/login", url.Values{"email": {email}, "password": {password}})
	if resp.StatusCode != http.StatusFound {
		c.h.t.Fatalf("login as %s: %d %s", email, resp.StatusCode, body)
	}
	return resp
}

// sessionCookieOf returns the client's session cookie.
func (c *client) sessionCookieOf() *http.Cookie {
	u, _ := url.Parse(c.h.srv.URL)
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == sessionCookie {
			return ck
		}
	}
	return nil
}

// admin creates the bootstrap admin and logs in.
func (h *harness) admin() *client {
	h.t.Helper()
	if _, err := h.users.EnsureAdmin("admin@example.com", "admin-pass-1"); err != nil {
		h.t.Fatal(err)
	}
	c := h.client()
	c.login("admin@example.com", "admin-pass-1")
	return c
}

// invite creates a user through the Users page and returns the invite
// link path from the flash.
func (h *harness) invite(admin *client, email string, perms url.Values) string {
	h.t.Helper()
	vals := url.Values{"email": {email}}
	for k, v := range perms {
		vals[k] = v
	}
	resp, body := admin.postForm("/users/create", vals)
	if resp.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	_, page := admin.get("/users")
	return extractLink(h.t, page)
}

var linkRe = regexp.MustCompile(`id="link"[^>]*value="([^"]+)"`)

func extractLink(t *testing.T, page string) string {
	t.Helper()
	m := linkRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no link on the page:\n%s", page)
	}
	u, err := url.Parse(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return u.Path
}

// activate redeems an invite/reset link with a password and returns a
// logged-in client for the new account.
func (h *harness) activate(linkPath, password string) *client {
	h.t.Helper()
	c := h.client()
	resp, body := c.postForm(linkPath, url.Values{"password": {password}, "password2": {password}})
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		h.t.Fatalf("activate: %d %s", resp.StatusCode, body)
	}
	if c.sessionCookieOf() == nil {
		h.t.Fatal("activation did not log the user in")
	}
	return c
}

// --- setup notice ---

func TestSetupNoticeUntilFirstAccount(t *testing.T) {
	h := newHarness(t, nil)
	c := h.client()
	for _, path := range []string{"/", "/login", "/state", "/stream.mp3"} {
		resp, body := c.get(path)
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "iar remote setup") {
			t.Fatalf("%s before setup: %d %s", path, resp.StatusCode, body)
		}
	}
	// `iar remote setup` from another process: the server notices
	// without restarting.
	other := accounts.NewStore(h.users.Path())
	other.EnsureAdmin("owner@example.com", "owner-pass-1")
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(h.users.Path(), future, future)
	resp, _ := c.get("/")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
		t.Fatalf("after setup, / = %d -> %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// --- login ---

func TestLoginFlowAndCookie(t *testing.T) {
	h := newHarness(t, nil)
	h.users.EnsureAdmin("admin@example.com", "admin-pass-1")
	c := h.client()

	// Pages redirect to login; data routes answer 401.
	resp, _ := c.get("/")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
		t.Fatalf("anonymous / = %d -> %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = c.get("/account")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login?next=/account" {
		t.Fatalf("anonymous /account = %d -> %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, p := range []string{"/state", "/me", "/stream.mp3", "/api/chunks", "/chunks/untagged/x.mp3"} {
		resp, _ = c.get(p)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anonymous %s = %d", p, resp.StatusCode)
		}
	}
	resp, body := c.get("/login")
	if resp.StatusCode != 200 || !strings.Contains(body, `name="password"`) {
		t.Fatalf("login page: %d", resp.StatusCode)
	}

	// Wrong password and unknown account get the same message.
	resp, body1 := c.postForm("/login", url.Values{"email": {"admin@example.com"}, "password": {"nope"}})
	resp2, body2 := c.postForm("/login", url.Values{"email": {"ghost@example.com"}, "password": {"nope"}})
	if resp.StatusCode != http.StatusUnauthorized || resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad logins: %d %d", resp.StatusCode, resp2.StatusCode)
	}
	if !strings.Contains(body1, loginFailure) || !strings.Contains(body2, loginFailure) {
		t.Fatal("login failure message differs or missing")
	}
	if c.sessionCookieOf() != nil {
		t.Fatal("failed login set a cookie")
	}

	// Success: cookie flags, redirect to next.
	resp, _ = c.postForm("/login", url.Values{"email": {"Admin@Example.com"}, "password": {"admin-pass-1"}, "next": {"/account"}})
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/account" {
		t.Fatalf("login = %d -> %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	var ck *http.Cookie
	for _, x := range resp.Cookies() {
		if x.Name == sessionCookie {
			ck = x
		}
	}
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Secure || len(ck.Value) < 40 {
		t.Fatalf("session cookie = %+v", ck)
	}
	resp, body = c.get("/")
	if resp.StatusCode != 200 || !strings.Contains(body, `id="play"`) {
		t.Fatalf("logged-in / = %d", resp.StatusCode)
	}
	resp, body = c.get("/me")
	if resp.StatusCode != 200 || !strings.Contains(body, `"admin":true`) {
		t.Fatalf("/me = %d %s", resp.StatusCode, body)
	}
	// Open redirects are refused.
	resp, _ = c.postForm("/login", url.Values{"email": {"admin@example.com"}, "password": {"admin-pass-1"}, "next": {"//evil.example.com/x"}})
	if resp.Header.Get("Location") != "/" {
		t.Fatalf("open redirect: %s", resp.Header.Get("Location"))
	}

	// Logout kills the session.
	resp, _ = c.postForm("/logout", nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
	resp, _ = c.get("/state")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout /state = %d", resp.StatusCode)
	}
}

func TestSessionsSurviveRestart(t *testing.T) {
	h := newHarness(t, nil)
	c := h.admin()
	ck := c.sessionCookieOf()
	// A new server over the same files (a player restart).
	users := accounts.NewStore(h.users.Path())
	sessions := accounts.NewSessions(filepath.Join(h.dir, "remote", "sessions.json"), time.Hour)
	s2, _ := newServer(Config{Users: users, Sessions: sessions, SnippetsDir: h.dir}, h.ctl, NewStreamer(testLog()), testLog())
	s2.buildAllowedHosts(nil)
	srv2 := httptest.NewServer(s2.buildHandler())
	defer srv2.Close()
	u, _ := url.Parse(srv2.URL)
	s2.cfg.Port, _ = strconv.Atoi(u.Port())
	req, _ := http.NewRequest("GET", srv2.URL+"/state", nil)
	req.AddCookie(ck)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("session lost across restart: %d", resp.StatusCode)
	}
}

func TestLoginRateLimit(t *testing.T) {
	h := newHarness(t, nil)
	h.users.EnsureAdmin("admin@example.com", "admin-pass-1")
	c := h.client()
	for i := 0; i < loginAttempts; i++ {
		resp, _ := c.postForm("/login", url.Values{"email": {"admin@example.com"}, "password": {"wrong"}})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d", i, resp.StatusCode)
		}
	}
	resp, body := c.postForm("/login", url.Values{"email": {"admin@example.com"}, "password": {"admin-pass-1"}})
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "Too many attempts") {
		t.Fatalf("after %d failures: %d %s", loginAttempts, resp.StatusCode, body)
	}
	if c.sessionCookieOf() != nil {
		t.Fatal("throttled login succeeded")
	}
}

// --- permissions ---

func TestPermissionMatrix(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	mk := func(email string, perms url.Values) *client {
		link := h.invite(admin, email, perms)
		return h.activate(link, "password-"+email)
	}
	listener := mk("listener@example.com", nil)
	steerer := mk("steerer@example.com", url.Values{"steer": {"1"}})
	prompter := mk("prompter@example.com", url.Values{"new_prompt": {"1"}})
	saver := mk("saver@example.com", url.Values{"save": {"1"}})
	adminOnly := mk("adminonly@example.com", url.Values{"admin": {"1"}})
	anon := h.client()
	// The admin calls below act on this account, so the asserted ones
	// keep their permissions whatever order the rows run in.
	h.invite(admin, "target@example.com", nil)

	type call struct {
		method, path string
		form         url.Values
	}
	calls := map[string]call{
		"/":                {"GET", "/", nil},
		"/me":              {"GET", "/me", nil},
		"/state":           {"GET", "/state", nil},
		"/api/chunks":      {"GET", "/api/chunks", nil},
		"/account":         {"GET", "/account", nil},
		"/steer":           {"POST", "/steer", url.Values{"text": {"calmer"}}},
		"/lyrics-gen":      {"POST", "/lyrics-gen", url.Values{"name": {"smoothbrain"}}},
		"/language":        {"POST", "/language", url.Values{"name": {"English"}, "on": {"1"}}},
		"/languages":       {"POST", "/languages", url.Values{"names": {"English, Russian"}}},
		"/next":            {"POST", "/next", nil},
		"/loop":            {"POST", "/loop", nil},
		"/new":             {"POST", "/new", url.Values{"prompt": {"dark techno"}}},
		"/save":            {"POST", "/save", url.Values{"tag": {"gym"}, "which": {"t-1"}}},
		"/users":           {"GET", "/users", nil},
		"/api/sessions":    {"GET", "/api/sessions", nil},
		"/sessions/save":   {"POST", "/sessions/save", url.Values{"name": {"web-named"}}},
		"/sessions/load":   {"POST", "/sessions/load", url.Values{"name": {"gym-grind"}}},
		"/sessions/delete": {"POST", "/sessions/delete", url.Values{"name": {"gym-grind"}}},
		"/users/link":      {"POST", "/users/link", url.Values{"email": {"target@example.com"}}},
		"/users/update": {"POST", "/users/update", url.Values{
			"email": {"target@example.com"}, "steer": {"1"}}},
	}
	type expect map[string]int
	const (
		ok    = 200
		see   = 303
		redir = 302
		deny  = 403
		auth  = 401
	)
	matrix := map[string]struct {
		c    *client
		want expect
	}{
		"anonymous": {anon, expect{"/": redir, "/me": auth, "/state": auth, "/api/chunks": auth, "/account": redir,
			"/steer": auth, "/lyrics-gen": auth, "/language": auth, "/languages": auth, "/next": auth, "/loop": auth, "/new": auth, "/save": auth, "/users": redir, "/users/link": redir, "/users/update": redir,
			"/api/sessions": auth, "/sessions/save": auth, "/sessions/load": auth, "/sessions/delete": auth}},
		"listener": {listener, expect{"/": ok, "/me": ok, "/state": ok, "/api/chunks": ok, "/account": ok,
			"/steer": deny, "/lyrics-gen": deny, "/language": deny, "/languages": deny, "/next": deny, "/loop": deny, "/new": deny, "/save": deny, "/users": deny, "/users/link": deny, "/users/update": deny,
			"/api/sessions": ok, "/sessions/save": deny, "/sessions/load": deny, "/sessions/delete": deny}},
		"steerer": {steerer, expect{"/": ok, "/me": ok, "/state": ok, "/api/chunks": ok, "/account": ok,
			"/steer": ok, "/lyrics-gen": ok, "/language": ok, "/languages": deny, "/next": ok, "/loop": ok, "/new": deny, "/save": deny, "/users": deny, "/users/link": deny, "/users/update": deny,
			"/api/sessions": ok, "/sessions/save": deny, "/sessions/load": deny, "/sessions/delete": deny}},
		"prompter": {prompter, expect{"/": ok, "/me": ok, "/state": ok, "/api/chunks": ok, "/account": ok,
			"/steer": deny, "/lyrics-gen": deny, "/language": deny, "/languages": deny, "/next": deny, "/loop": deny, "/new": ok, "/save": deny, "/users": deny, "/users/link": deny, "/users/update": deny,
			"/api/sessions": ok, "/sessions/save": deny, "/sessions/load": ok, "/sessions/delete": deny}},
		"saver": {saver, expect{"/": ok, "/me": ok, "/state": ok, "/api/chunks": ok, "/account": ok,
			"/steer": deny, "/lyrics-gen": deny, "/language": deny, "/languages": deny, "/next": deny, "/loop": deny, "/new": deny, "/save": ok, "/users": deny, "/users/link": deny, "/users/update": deny,
			"/api/sessions": ok, "/sessions/save": ok, "/sessions/load": deny, "/sessions/delete": deny}},
		// Admin alone does not grant steer/new/save.
		"admin-only": {adminOnly, expect{"/": ok, "/me": ok, "/state": ok, "/api/chunks": ok, "/account": ok,
			"/steer": deny, "/lyrics-gen": deny, "/language": deny, "/languages": ok, "/next": deny, "/loop": deny, "/new": deny, "/save": deny, "/users": ok, "/users/link": see, "/users/update": see,
			"/api/sessions": ok, "/sessions/save": deny, "/sessions/load": deny, "/sessions/delete": ok}},
		"full admin": {admin, expect{"/": ok, "/me": ok, "/state": ok, "/api/chunks": ok, "/account": ok,
			"/steer": ok, "/lyrics-gen": ok, "/language": ok, "/languages": ok, "/next": ok, "/loop": ok, "/new": ok, "/save": ok, "/users": ok, "/users/link": see, "/users/update": see,
			"/api/sessions": ok, "/sessions/save": ok, "/sessions/load": ok, "/sessions/delete": ok}},
	}
	for who, row := range matrix {
		for name, want := range row.want {
			cl := calls[name]
			var resp *http.Response
			switch {
			case cl.method == "GET":
				resp, _ = row.c.get(cl.path)
			case strings.HasPrefix(cl.path, "/users"):
				resp, _ = row.c.postForm(cl.path, cl.form)
			default:
				resp, _ = row.c.postAPI(cl.path, cl.form)
			}
			if resp.StatusCode != want {
				t.Errorf("%s %s %s = %d, want %d", who, cl.method, cl.path, resp.StatusCode, want)
			}
		}
	}
	// Only the permitted calls reached the controller.
	h.ctl.mu.Lock()
	defer h.ctl.mu.Unlock()
	if len(h.ctl.steers) != 2 || h.ctl.skips != 2 || h.ctl.loops != 2 || len(h.ctl.news) != 2 || len(h.ctl.saves) != 2 {
		t.Fatalf("controller calls: steers=%v skips=%d loops=%d news=%v saves=%v", h.ctl.steers, h.ctl.skips, h.ctl.loops, h.ctl.news, h.ctl.saves)
	}
	// The lyric-writer switch rides the steer permission (steerer +
	// full admin).
	if len(h.ctl.lyrGens) != 2 || h.ctl.lyrGens[0] != "smoothbrain" {
		t.Fatalf("lyric writer calls: %v", h.ctl.lyrGens)
	}
	if h.ctl.saves[0][1] != "gym" || h.ctl.saves[0][0] != "t-1" {
		t.Fatalf("save which/tag not passed through: %v", h.ctl.saves)
	}
	// Session actions: save needs the save permission (saver + full
	// admin), load the new-prompt permission (prompter + full admin),
	// delete admin (admin-only + full admin).
	if len(h.ctl.named) != 2 || len(h.ctl.loaded) != 2 || len(h.ctl.deleted) != 2 {
		t.Fatalf("session calls: named=%v loaded=%v deleted=%v", h.ctl.named, h.ctl.loaded, h.ctl.deleted)
	}
	// Switching one language rides the steer permission (steerer + full
	// admin); editing the configured list is admin-only (admin-only +
	// full admin), because it rewrites the machine's config file.
	if len(h.ctl.langSet) != 2 || h.ctl.langSet[0] != [2]string{"English", "on"} {
		t.Fatalf("language switches: %v", h.ctl.langSet)
	}
	if len(h.ctl.langAll) != 2 || strings.Join(h.ctl.langAll[0], "|") != "English|Russian" {
		t.Fatalf("configured language lists: %v", h.ctl.langAll)
	}
	if len(h.ctl.notes) != 22 {
		t.Fatalf("remote actions must be announced to the local UI: %v", h.ctl.notes)
	}
}

func TestSessionsListingAndActions(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	resp, body := admin.get("/api/sessions")
	if resp.StatusCode != 200 {
		t.Fatalf("/api/sessions = %d %s", resp.StatusCode, body)
	}
	var l sessionsJSON
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatal(err)
	}
	if l.Current != "s1" || len(l.Named) != 2 || l.Named[0].Name != "s1" || !l.Named[0].Current || l.Named[1].Name != "gym-grind" {
		t.Fatalf("named group = %+v (current %q)", l.Named, l.Current)
	}
	if len(l.Presets) != len(session.Presets()) || l.Presets[0].Name != "grind" || l.Presets[0].Group != "high-energy" || l.Presets[0].Summary == "" {
		t.Fatalf("presets group = %+v", l.Presets)
	}
	if len(l.Auto) != 1 || l.Auto[0].Name != "session-20260821-100000" || l.Auto[0].Played != "26h ago" {
		t.Fatalf("auto group = %+v", l.Auto)
	}
	if l.Named[1].Played != "3h ago" || l.Named[1].Summary == "" {
		t.Fatalf("row details = %+v", l.Named[1])
	}
	for _, tc := range []struct{ path, field, value, want string }{
		{"/sessions/save", "name", "Road Trip", "session saved as Road Trip"},
		{"/sessions/load", "name", "sleep", "loaded sleep"},
		{"/sessions/delete", "name", "gym-grind", "session gym-grind deleted"},
	} {
		resp, body := admin.postAPI(tc.path, url.Values{tc.field: {tc.value}})
		if resp.StatusCode != 200 || !strings.Contains(body, tc.want) {
			t.Fatalf("%s = %d %s", tc.path, resp.StatusCode, body)
		}
		resp, _ = admin.postAPI(tc.path, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s without a name = %d", tc.path, resp.StatusCode)
		}
	}
	// The page carries the station list and the naming controls; the
	// page script drives the session endpoints.
	_, page := admin.get("/")
	for _, want := range []string{`id="sessionsave"`, `id="sessname"`, `id="sesssave"`, `id="sessions"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing %q", want)
		}
	}
	_, script := admin.get("/app.js")
	for _, want := range []string{"/api/sessions", "/sessions/delete"} {
		if !strings.Contains(script, want) {
			t.Fatalf("page script missing %q", want)
		}
	}
}

// --- invite and reset links ---

func TestInviteFlowWithoutEmail(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()

	resp, body := admin.postForm("/users/create", url.Values{"email": {"Jane@Example.com"}, "steer": {"1"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/users" {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	_, page := admin.get("/users")
	// The link is selectable text, not a copy button: nothing in the
	// remote writes to the clipboard.
	for _, want := range []string{"Account created for jane@example.com", "Invitation link", `id="link"`, "jane@example.com", "invited", "expires in"} {
		if !strings.Contains(page, want) {
			t.Fatalf("users page missing %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "emailed") || strings.Contains(page, "Email could not") {
		t.Fatal("without SMTP the page must not talk about email delivery")
	}
	link := extractLink(t, page)
	if !strings.HasPrefix(link, "/set-password/") {
		t.Fatalf("link path = %q", link)
	}
	// The flash is one-shot.
	_, again := admin.get("/users")
	if strings.Contains(again, `id="link"`) {
		t.Fatal("link flash shown twice")
	}
	// Pending list shows it.
	if !strings.Contains(again, "Pending links") || !strings.Contains(again, "Regenerate") {
		t.Fatal("pending links section missing")
	}

	// The invitee opens the link: email read-only, chooses a password.
	c := h.client()
	resp, body = c.get(link)
	if resp.StatusCode != 200 || !strings.Contains(body, `value="jane@example.com" readonly`) || !strings.Contains(body, "invited") {
		t.Fatalf("invite page = %d %s", resp.StatusCode, body)
	}
	resp, body = c.postForm(link, url.Values{"password": {"jane-pass-1"}, "password2": {"different"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "do not match") {
		t.Fatalf("mismatch = %d", resp.StatusCode)
	}
	resp, body = c.postForm(link, url.Values{"password": {"short"}, "password2": {"short"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "at least 8") {
		t.Fatalf("weak = %d %s", resp.StatusCode, body)
	}
	jane := h.activate(link, "jane-pass-1")
	resp, body = jane.get("/me")
	if resp.StatusCode != 200 || !strings.Contains(body, `"steer":true`) || !strings.Contains(body, `"admin":false`) {
		t.Fatalf("/me after activation = %d %s", resp.StatusCode, body)
	}
	// The link is dead now.
	resp, body = h.client().get(link)
	if resp.StatusCode != http.StatusGone || !strings.Contains(body, "not valid") {
		t.Fatalf("used link = %d", resp.StatusCode)
	}
	resp, _ = h.client().postForm(link, url.Values{"password": {"x-pass-123"}, "password2": {"x-pass-123"}})
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("used link post = %d", resp.StatusCode)
	}
	_, page = admin.get("/users")
	if strings.Contains(page, "Pending links") {
		t.Fatal("redeemed link still pending")
	}
	// Jane can log in with the password she chose; the admin never saw it.
	j2 := h.client()
	j2.login("jane@example.com", "jane-pass-1")
}

func TestInviteRegenerateAndRevoke(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	first := h.invite(admin, "kim@example.com", nil)

	// Regenerate: new link works, old one is dead.
	resp, _ := admin.postForm("/users/link", url.Values{"email": {"kim@example.com"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("regenerate = %d", resp.StatusCode)
	}
	_, page := admin.get("/users")
	second := extractLink(t, page)
	if second == first {
		t.Fatal("regenerate returned the same link")
	}
	if resp, _ := h.client().get(first); resp.StatusCode != http.StatusGone {
		t.Fatalf("old link after regenerate = %d", resp.StatusCode)
	}
	if resp, _ := h.client().get(second); resp.StatusCode != 200 {
		t.Fatalf("new link = %d", resp.StatusCode)
	}
	// Revoke: dead, and gone from the pending list.
	resp, _ = admin.postForm("/users/revoke-link", url.Values{"email": {"kim@example.com"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke = %d", resp.StatusCode)
	}
	if resp, _ := h.client().get(second); resp.StatusCode != http.StatusGone {
		t.Fatalf("revoked link = %d", resp.StatusCode)
	}
	_, page = admin.get("/users")
	if !strings.Contains(page, "revoked") || strings.Contains(page, "Pending links") {
		t.Fatalf("page after revoke:\n%s", page)
	}
	// Expired links are refused.
	secret, _, _ := h.users.IssueLink("kim@example.com")
	if resp, _ := h.client().get("/set-password/" + secret); resp.StatusCode != 200 {
		t.Fatalf("fresh link = %d", resp.StatusCode)
	}
	if resp, _ := h.client().get("/set-password/not-a-real-secret"); resp.StatusCode != http.StatusGone {
		t.Fatalf("bogus link = %d", resp.StatusCode)
	}
}

func TestResetLinkLogsOutOtherSessions(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	link := h.invite(admin, "lee@example.com", nil)
	lee := h.activate(link, "lee-pass-1")
	leePhone := h.client()
	leePhone.login("lee@example.com", "lee-pass-1")

	// An active account gets a RESET link.
	resp, _ := admin.postForm("/users/link", url.Values{"email": {"lee@example.com"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("reset link = %d", resp.StatusCode)
	}
	_, page := admin.get("/users")
	if !strings.Contains(page, "Password reset link") {
		t.Fatal("reset link not labeled as such")
	}
	reset := extractLink(t, page)
	resp, body := h.client().get(reset)
	if resp.StatusCode != 200 || !strings.Contains(body, "Choose a new password") {
		t.Fatalf("reset page = %d %s", resp.StatusCode, body)
	}
	fresh := h.activate(reset, "lee-pass-2")
	if resp, _ := fresh.get("/state"); resp.StatusCode != 200 {
		t.Fatalf("fresh session = %d", resp.StatusCode)
	}
	for name, c := range map[string]*client{"old browser": lee, "phone": leePhone} {
		if resp, _ := c.get("/state"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s still logged in after reset: %d", name, resp.StatusCode)
		}
	}
	if _, ok := h.users.Verify("lee@example.com", "lee-pass-1"); ok {
		t.Fatal("old password survived the reset")
	}
}

func TestInviteEmailsLinkWhenConfigured(t *testing.T) {
	m := &fakeMailer{}
	h := newHarness(t, m)
	admin := h.admin()
	link := h.invite(admin, "pat@example.com", nil)
	m.mu.Lock()
	sent := append([]string(nil), m.sent...)
	m.mu.Unlock()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "pat@example.com|Your Infinite AI Radio invitation|") {
		t.Fatalf("sent = %v", sent)
	}
	if !strings.Contains(sent[0], h.srv.URL+link) {
		t.Fatalf("email lacks the link %s:\n%s", link, sent[0])
	}
	if strings.Contains(strings.ToLower(sent[0]), "password:") {
		t.Fatal("email must never carry a password")
	}
	// The page says it was emailed and still shows the link.
	m.fail = true
	resp, _ := admin.postForm("/users/link", url.Values{"email": {"pat@example.com"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("regenerate = %d", resp.StatusCode)
	}
	_, page := admin.get("/users")
	if !strings.Contains(page, "Email could not be sent") || !strings.Contains(page, `id="link"`) {
		t.Fatalf("email failure must still show the link:\n%s", page)
	}
	// With SMTP the users page no longer describes hand-over only.
	if strings.Contains(page, "no email server configured") {
		t.Fatal("page claims no email server")
	}
}

// --- users page guards and account page ---

func TestUsersGuards(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	// Cannot delete yourself.
	admin.postForm("/users/delete", url.Values{"email": {"admin@example.com"}})
	_, page := admin.get("/users")
	if !strings.Contains(page, "cannot delete your own account") {
		t.Fatalf("self delete not refused:\n%s", page)
	}
	// Cannot remove the last admin.
	admin.postForm("/users/update", url.Values{"email": {"admin@example.com"}, "steer": {"1"}})
	_, page = admin.get("/users")
	if !strings.Contains(page, "remove the last admin") {
		t.Fatalf("last admin demotion not refused:\n%s", page)
	}
	// Deleting another user ends their sessions.
	link := h.invite(admin, "tmp@example.com", nil)
	tmp := h.activate(link, "tmp-pass-12")
	resp, _ := admin.postForm("/users/delete", url.Values{"email": {"tmp@example.com"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	if resp, _ := tmp.get("/state"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("deleted user still logged in: %d", resp.StatusCode)
	}
	// Unknown emails and invalid input produce errors, not crashes.
	admin.postForm("/users/update", url.Values{"email": {"ghost@example.com"}})
	_, page = admin.get("/users")
	if !strings.Contains(page, "no account with that email") {
		t.Fatalf("unknown update not reported:\n%s", page)
	}
	admin.postForm("/users/create", url.Values{"email": {"not an email"}})
	_, page = admin.get("/users")
	if !strings.Contains(page, "not a valid email") {
		t.Fatalf("invalid email not reported:\n%s", page)
	}
}

func TestAccountPasswordChange(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	other := h.client()
	other.login("admin@example.com", "admin-pass-1")

	resp, body := admin.postForm("/account/password", url.Values{"current": {"wrong"}, "password": {"new-pass-123"}, "password2": {"new-pass-123"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "current password is wrong") {
		t.Fatalf("wrong current = %d", resp.StatusCode)
	}
	resp, body = admin.postForm("/account/password", url.Values{"current": {"admin-pass-1"}, "password": {"new-pass-123"}, "password2": {"other"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "do not match") {
		t.Fatalf("mismatch = %d", resp.StatusCode)
	}
	resp, body = admin.postForm("/account/password", url.Values{"current": {"admin-pass-1"}, "password": {"new-pass-123"}, "password2": {"new-pass-123"}})
	if resp.StatusCode != 200 || !strings.Contains(body, "Password changed") {
		t.Fatalf("change = %d %s", resp.StatusCode, body)
	}
	// This browser stays logged in; the other one is out.
	if resp, _ := admin.get("/state"); resp.StatusCode != 200 {
		t.Fatalf("own session lost: %d", resp.StatusCode)
	}
	if resp, _ := other.get("/state"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("other session survived: %d", resp.StatusCode)
	}
	if _, ok := h.users.Verify("admin@example.com", "new-pass-123"); !ok {
		t.Fatal("new password rejected")
	}
}

// --- chunks ---

// syntheticMP3 builds a tagged MP3-shaped file (ID3v2.3 + one MPEG1
// header + zero padding): enough for the catalog and range serving.
func syntheticMP3(title, album string, bytes int) []byte {
	frame := func(id, text string) []byte {
		data := []byte{3} // UTF-8
		data = append(data, text...)
		f := []byte(id)
		f = binary.BigEndian.AppendUint32(f, uint32(len(data)))
		f = append(f, 0, 0)
		return append(f, data...)
	}
	body := append(frame("TIT2", title), frame("TALB", album)...)
	size := len(body)
	hdr := []byte{'I', 'D', '3', 3, 0, 0, byte(size>>21) & 0x7f, byte(size>>14) & 0x7f, byte(size>>7) & 0x7f, byte(size) & 0x7f}
	out := append(hdr, body...)
	out = append(out, 0xFF, 0xFB, 0x94, 0x00) // MPEG1 L3 128kbps 48kHz stereo
	return append(out, make([]byte, bytes-len(out))...)
}

func TestChunkListingAndRangeServing(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	dir := h.s.catalog.Dir()
	os.MkdirAll(filepath.Join(dir, "gym_grind"), 0o755)
	os.MkdirAll(filepath.Join(dir, "untagged"), 0o755)
	a := filepath.Join(dir, "gym_grind", "20260821-100000-rock.mp3")
	b := filepath.Join(dir, "untagged", "20260821-110000-piano.mp3")
	os.WriteFile(a, syntheticMP3("energetic rock", "gym_grind", 16000), 0o644)
	os.WriteFile(b, syntheticMP3("calm piano", "untagged", 32000), 0o644)
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(a, old, old)
	// A secret outside the snippets tree that must be unreachable.
	os.WriteFile(filepath.Join(h.dir, "secret.mp3"), []byte("secret"), 0o644)

	resp, body := admin.get("/api/chunks")
	if resp.StatusCode != 200 {
		t.Fatalf("/api/chunks = %d %s", resp.StatusCode, body)
	}
	var listing chunksJSON
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Chunks) != 2 || listing.Chunks[0].Title != "calm piano" || listing.Chunks[1].Tag != "gym_grind" {
		t.Fatalf("listing = %+v", listing)
	}
	if strings.Join(listing.Tags, ",") != "gym_grind,untagged" {
		t.Fatalf("tags = %v", listing.Tags)
	}
	if listing.Chunks[0].Seconds < 1.9 || listing.Chunks[0].Seconds > 2.1 { // 32000 B at 128 kbps
		t.Fatalf("duration = %v", listing.Chunks[0].Seconds)
	}
	if listing.Chunks[1].URL != "/chunks/gym_grind/20260821-100000-rock.mp3" {
		t.Fatalf("url = %q", listing.Chunks[1].URL)
	}

	// Whole file.
	resp, body = admin.get(listing.Chunks[1].URL)
	if resp.StatusCode != 200 || len(body) != 16000 || resp.Header.Get("Content-Type") != "audio/mpeg" || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("file = %d len %d type %s ranges %s", resp.StatusCode, len(body), resp.Header.Get("Content-Type"), resp.Header.Get("Accept-Ranges"))
	}
	// Range request (seeking).
	req, _ := http.NewRequest("GET", h.srv.URL+listing.Chunks[1].URL, nil)
	req.Header.Set("Range", "bytes=100-199")
	resp2, err := admin.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusPartialContent || len(part) != 100 || resp2.Header.Get("Content-Range") != "bytes 100-199/16000" {
		t.Fatalf("range = %d len %d %s", resp2.StatusCode, len(part), resp2.Header.Get("Content-Range"))
	}
	// Traversal and junk are 404, never served.
	for _, p := range []string{
		"/chunks/gym_grind/missing.mp3", "/chunks/../secret.mp3", "/chunks/gym_grind/..%2Fsecret.mp3",
		"/chunks/Gym_Grind/20260821-100000-rock.mp3", "/chunks/gym_grind/.hidden.mp3",
	} {
		resp, _ := admin.get(p)
		if resp.StatusCode == 200 {
			t.Fatalf("%s served", p)
		}
	}
	// Listening needs a login but no other permission.
	if resp, _ := h.client().get(listing.Chunks[1].URL); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous chunk = %d", resp.StatusCode)
	}
	// An empty library is an empty listing, not an error.
	os.RemoveAll(dir)
	resp, body = admin.get("/api/chunks")
	if resp.StatusCode != 200 || !strings.Contains(body, `"chunks":[]`) {
		t.Fatalf("empty listing = %d %s", resp.StatusCode, body)
	}
}

// --- page content ---

func TestPlayerPageContainsControls(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	_, page := admin.get("/")
	for _, want := range []string{
		`id="play"`, `id="next"`, `id="steer"`, `id="fresh"`, `id="save"`, `id="saveprev"`, `id="carsave"`, `id="carresume"`, `id="tag"`, `id="text"`, `id="now"`, `id="nowprompt"`,
		`id="mode-live"`, `id="mode-saved"`, `id="tags"`, `id="chunks"`, `id="savedaudio"`, `id="backloop"`,
		`id="steerstatus"`, `id="savestatus"`, `id="sessstatus"`,
		`href="/users"`, `href="/account"`, `action="/logout"`, "viewport", "/app.js", "manifest.webmanifest",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing %q", want)
		}
	}
	// The page script is served publicly and drives the stream.
	resp, script := admin.get("/app.js")
	if resp.StatusCode != 200 || !strings.Contains(script, "stream.mp3") || !strings.Contains(script, "mediaSession") {
		t.Fatalf("app.js = %d (stream/mediaSession present: %v/%v)", resp.StatusCode,
			strings.Contains(script, "stream.mp3"), strings.Contains(script, "mediaSession"))
	}
	// Non-admins get no Users link.
	link := h.invite(admin, "plain@example.com", nil)
	plain := h.activate(link, "plain-pass-1")
	_, page = plain.get("/")
	if strings.Contains(page, `href="/users"`) {
		t.Fatal("non-admin page shows the Users link")
	}
	resp, _ = plain.get("/users")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin /users = %d", resp.StatusCode)
	}
}

func TestLanguageListIsNotTruncatedIntoFragments(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	// A full catalogue is longer than a steering phrase. Cutting it mid
	// name leaves a fragment that still parses as a language, and the
	// answer then reports the mangled list as saved.
	names := []string{}
	for i := 0; i < 20; i++ {
		names = append(names, "Language Number "+strconv.Itoa(i))
	}
	joined := strings.Join(names, ", ")
	if len(joined) <= 300 {
		t.Fatalf("test list is only %d bytes; it must exceed the phrase cap", len(joined))
	}
	resp, _ := admin.postForm("/languages", url.Values{"names": {joined}})
	if resp.StatusCode != 200 {
		t.Fatalf("POST /languages = %d", resp.StatusCode)
	}
	got := h.ctl.languagesSet()
	if len(got) != len(names) {
		t.Fatalf("saved %d of %d languages: %v", len(got), len(names), got)
	}
	for i, n := range names {
		if got[i] != n {
			t.Fatalf("language %d = %q, want %q", i, got[i], n)
		}
	}
}

func TestStateCarriesSharedSteeringContext(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	resp, body := admin.get("/state")
	if resp.StatusCode != 200 {
		t.Fatalf("/state = %d", resp.StatusCode)
	}
	var st struct {
		Epoch       int    `json:"epoch"`
		SessionDesc string `json:"session_desc"`
		BasePrompt  string `json:"base_prompt"`
		Vocals      bool   `json:"vocals"`
		Tweaks      []struct {
			Raw         string `json:"raw"`
			Interpreted string `json:"interpreted"`
			Time        string `json:"time"`
		} `json:"tweaks"`
		Track *struct {
			ID        string  `json:"id"`
			Prompt    string  `json:"prompt"`
			DurationS float64 `json:"duration_s"`
			Title     string  `json:"title"`
			Subtitle  string  `json:"subtitle"`
			Number    int     `json:"number"`
			Saved     bool    `json:"saved"`
			Lang      string  `json:"lang"`
			Lyrics    string  `json:"lyrics"`
		} `json:"track"`
		Switching bool `json:"switching"`
		Looping   bool `json:"looping"`
		Prev      *struct {
			ID    string `json:"id"`
			Saved bool   `json:"saved"`
		} `json:"prev"`
		SavedIDs         []string `json:"saved_ids"`
		LyricsGenerator  string   `json:"lyrics_generator"`
		LyricsGenerators []struct {
			Name  string `json:"name"`
			Blurb string `json:"blurb"`
		} `json:"lyrics_generators"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("parsing /state: %v\n%s", err, body)
	}
	if st.Epoch != 7 || st.BasePrompt != "dark techno" || !st.Vocals || st.SessionDesc == "" {
		t.Fatalf("steering context wrong: %+v", st)
	}
	// The page needs all three to answer "did my language change land":
	// what the track is sung in, that a change is still generating, and
	// that what is playing is a replay rather than something new.
	if st.Track == nil || st.Track.Lang != "Bisaya (Cebuano)" {
		t.Fatalf("track language missing: %+v", st.Track)
	}
	if st.Track.Lyrics != "[Verse]\nnaay usa ka gabii" {
		t.Fatalf("track lyrics missing: %q", st.Track.Lyrics)
	}
	if !st.Switching || !st.Looping {
		t.Fatalf("switching/looping missing: switching=%v looping=%v", st.Switching, st.Looping)
	}
	if len(st.Tweaks) != 2 || st.Tweaks[0].Raw != "less guitars" || st.Tweaks[1].Interpreted != "faster tempo" || st.Tweaks[0].Time == "" {
		t.Fatalf("tweaks wrong: %+v", st.Tweaks)
	}
	if st.Track == nil || st.Track.ID != "t-1" || st.Track.Prompt != "dark techno, driving" || st.Track.DurationS != 150 || st.Track.Saved {
		t.Fatalf("track wrong: %+v", st.Track)
	}
	if st.Track.Title != "Dark Techno" || st.Track.Subtitle != "driving" || st.Track.Number != 3 {
		t.Fatalf("track display names wrong: %+v", st.Track)
	}
	if st.Prev == nil || st.Prev.ID != "t-0" || !st.Prev.Saved {
		t.Fatalf("prev wrong: %+v", st.Prev)
	}
	if len(st.SavedIDs) != 1 || st.SavedIDs[0] != "t-0" {
		t.Fatalf("saved_ids wrong: %+v", st.SavedIDs)
	}
	if st.LyricsGenerator != "scribe" || len(st.LyricsGenerators) < 2 ||
		st.LyricsGenerators[0].Name == "" || st.LyricsGenerators[0].Blurb == "" {
		t.Fatalf("lyric writer state wrong: %q %+v", st.LyricsGenerator, st.LyricsGenerators)
	}
}

// --- security middleware ---

func TestHostAllowlistRejectsForeignHosts(t *testing.T) {
	h := newHarness(t, nil)
	h.users.EnsureAdmin("admin@example.com", "admin-pass-1")
	port := strconv.Itoa(h.s.cfg.Port)

	req, _ := http.NewRequest("GET", h.srv.URL+"/login", nil)
	req.Host = "evil.example.com:" + port // DNS-rebinding shape
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host accepted: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", h.srv.URL+"/login", nil)
	req.Host = "127.0.0.1:1"
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong-port host accepted: %d", resp.StatusCode)
	}
	resp, _ = http.Get(h.srv.URL + "/login")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("legit host rejected: %d", resp.StatusCode)
	}
}

func TestMutationsNeedOriginOrHeader(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	ck := admin.sessionCookieOf()
	do := func(origin, header string) int {
		req, _ := http.NewRequest("POST", h.srv.URL+"/steer", strings.NewReader("text=evil"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if header != "" {
			req.Header.Set(csrfHeader, header)
		}
		req.AddCookie(ck)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// A bare cross-site-style POST (no Origin, no header) is refused.
	if got := do("", ""); got != http.StatusForbidden {
		t.Fatalf("bare POST = %d", got)
	}
	// A foreign Origin is refused even with the header.
	if got := do("http://evil.example.com", "1"); got != http.StatusForbidden {
		t.Fatalf("foreign-origin POST = %d", got)
	}
	// The page's fetch shape (header) and a same-origin form post
	// (Origin) both work.
	if got := do("", "1"); got != 200 {
		t.Fatalf("header POST = %d", got)
	}
	if got := do(h.srv.URL, ""); got != 200 {
		t.Fatalf("same-origin form POST = %d", got)
	}
	h.ctl.mu.Lock()
	defer h.ctl.mu.Unlock()
	if len(h.ctl.steers) != 2 {
		t.Fatalf("only the legit posts may reach the controller: %v", h.ctl.steers)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"": "/", "/": "/", "/account": "/account", "//evil.example.com": "/", "http://evil.example.com": "/",
		"/x\\y": "/", "/users?x=1": "/users?x=1",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- prefetch queue endpoints ---

func TestQueueListingAndTrackServing(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()

	// Anonymous requests are refused.
	anon := h.client()
	if resp, _ := anon.get("/api/queue"); resp.StatusCode != 401 {
		t.Fatalf("anonymous /api/queue = %d", resp.StatusCode)
	}
	if resp, _ := anon.get("/queue/t-1.mp3"); resp.StatusCode != 401 {
		t.Fatalf("anonymous /queue/t-1.mp3 = %d", resp.StatusCode)
	}

	resp, body := admin.get("/api/queue")
	if resp.StatusCode != 200 {
		t.Fatalf("/api/queue = %d", resp.StatusCode)
	}
	var q struct {
		Epoch  int `json:"epoch"`
		Tracks []struct {
			ID        string  `json:"id"`
			Prompt    string  `json:"prompt"`
			Title     string  `json:"title"`
			Subtitle  string  `json:"subtitle"`
			DurationS float64 `json:"duration_s"`
			Kind      string  `json:"kind"`
			URL       string  `json:"url"`
		} `json:"tracks"`
	}
	if err := json.Unmarshal([]byte(body), &q); err != nil {
		t.Fatalf("parsing queue: %v\n%s", err, body)
	}
	if q.Epoch != 7 || len(q.Tracks) != 3 {
		t.Fatalf("queue = %+v", q)
	}
	if q.Tracks[0].Kind != "queue" || q.Tracks[2].Kind != "library" || q.Tracks[0].URL == "" {
		t.Fatalf("queue rows = %+v", q.Tracks)
	}
	if q.Tracks[0].Title != "Dark Techno" || q.Tracks[0].Subtitle != "driving" || q.Tracks[2].Title != "Banked Techno" {
		t.Fatalf("queue rows lack display names: %+v", q.Tracks)
	}

	// A queued track serves as a valid MP3.
	resp, body = admin.get(q.Tracks[0].URL)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("track fetch = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if len(body) < 4000 || !strings.Contains(body[:4096], "\xff") {
		t.Fatalf("track bytes do not look like MP3 (%d bytes)", len(body))
	}
	full := len(body)

	// Range requests work (seeking): the tail of the file.
	req, _ := http.NewRequest("GET", admin.h.srv.URL+q.Tracks[0].URL, nil)
	req.Header.Set("Range", "bytes=1000-1999")
	rangeResp, err := admin.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rangeBody, _ := io.ReadAll(rangeResp.Body)
	rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusPartialContent || len(rangeBody) != 1000 {
		t.Fatalf("range = %d, %d bytes (full %d)", rangeResp.StatusCode, len(rangeBody), full)
	}

	// A gone track 404s.
	if resp, _ = admin.get("/queue/gone.mp3"); resp.StatusCode != 404 {
		t.Fatalf("gone track = %d", resp.StatusCode)
	}
	// Malformed names 404.
	if resp, _ = admin.get("/queue/notmp3"); resp.StatusCode != 404 {
		t.Fatalf("malformed name = %d", resp.StatusCode)
	}

	// Library-kind ids contain a slash ("lib:<vibe>/<stamp>"): the
	// listing must hand out the escaped form, and only that form is
	// served. Dead-zone prefetching depends on this exact route.
	lib := q.Tracks[2]
	if lib.Kind != "library" || lib.ID != "lib:techno/20260823-000000-0001" {
		t.Fatalf("library row = %+v", lib)
	}
	if want := "/queue/" + url.PathEscape(lib.ID) + ".mp3"; lib.URL != want || strings.Contains(lib.URL, "/20260823") {
		t.Fatalf("library url not escaped: %q (want %q)", lib.URL, want)
	}
	resp, body = admin.get(lib.URL)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("library track fetch = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if len(body) < 4000 || !strings.Contains(body[:4096], "\xff") {
		t.Fatalf("library track bytes do not look like MP3 (%d bytes)", len(body))
	}
	req, _ = http.NewRequest("GET", admin.h.srv.URL+lib.URL, nil)
	req.Header.Set("Range", "bytes=500-999")
	rangeResp, err = admin.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rangeBody, _ = io.ReadAll(rangeResp.Body)
	rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusPartialContent || len(rangeBody) != 500 {
		t.Fatalf("library range = %d, %d bytes", rangeResp.StatusCode, len(rangeBody))
	}
	// The raw-slash form is a different path entirely and must not
	// resolve to the track.
	if resp, _ = admin.get("/queue/" + lib.ID + ".mp3"); resp.StatusCode != 404 {
		t.Fatalf("raw-slash library path = %d, want 404", resp.StatusCode)
	}
}

func TestMP3CacheSingleFlight(t *testing.T) {
	c := newMP3Cache()
	var calls int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := c.get("x", func() ([]byte, error) {
				atomic.AddInt32(&calls, 1)
				time.Sleep(50 * time.Millisecond)
				return []byte("mp3"), nil
			})
			if err != nil || string(data) != "mp3" {
				t.Errorf("get = %q, %v", data, err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("encode ran %d times for one id", got)
	}
	// Failures are not cached.
	if _, err := c.get("bad", func() ([]byte, error) { return nil, errTrackGone }); err == nil {
		t.Fatal("error not surfaced")
	}
	if data, err := c.get("bad", func() ([]byte, error) { return []byte("ok"), nil }); err != nil || string(data) != "ok" {
		t.Fatalf("retry after failure = %q, %v", data, err)
	}
	// The cache stays bounded.
	for i := 0; i < mp3CacheSlots*2; i++ {
		id := fmt.Sprintf("id-%d", i)
		c.get(id, func() ([]byte, error) { return make([]byte, 10), nil })
	}
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > mp3CacheSlots {
		t.Fatalf("cache holds %d entries", n)
	}
}

// The page is no-store and the stylesheet is inlined into it, so a
// phone always gets the current layout. The script has to keep up: a
// plain max-age with no validator let a browser run an hour-old app.js
// against a fresh page, which is indistinguishable from a fix that did
// not work.
func TestStaticAssetsRevalidate(t *testing.T) {
	h := newHarness(t, nil)
	anon := h.client()
	for _, path := range []string{"/app.js", "/manifest.webmanifest"} {
		resp, body := anon.get(path)
		if resp.StatusCode != 200 || body == "" {
			t.Fatalf("%s = %d (%d bytes)", path, resp.StatusCode, len(body))
		}
		cc := resp.Header.Get("Cache-Control")
		if strings.Contains(cc, "max-age") && !strings.Contains(cc, "max-age=0") {
			t.Fatalf("%s may be pinned by a browser: Cache-Control %q", path, cc)
		}
		etag := resp.Header.Get("ETag")
		if etag == "" {
			t.Fatalf("%s has no validator, so a stale copy is never noticed", path)
		}
		// The same tag comes back unconditionally, and a matching
		// request costs a 304 rather than the whole body.
		req, err := http.NewRequest("GET", h.srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("If-None-Match", etag)
		again, err := anon.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		again.Body.Close()
		if again.StatusCode != http.StatusNotModified {
			t.Fatalf("%s with a matching If-None-Match = %d, want 304", path, again.StatusCode)
		}
		if again.Header.Get("ETag") != etag {
			t.Fatalf("%s changed its tag between identical requests", path)
		}
	}
}

func TestChunkCuration(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	dir := h.s.catalog.Dir()
	os.MkdirAll(filepath.Join(dir, "untagged"), 0o755)
	file := "20260831-120000-tumutunaw-ang-selyo.tl.mp3"
	path := filepath.Join(dir, "untagged", file)
	os.WriteFile(path, syntheticMP3("Tumutunaw ang Selyo", "untagged", 16000), 0o644)
	os.WriteFile(snippets.LyricsSidecar(path), []byte("[Verse 1]\nline one"), 0o644)

	// The listing carries language (from the file name), its name in
	// words, and the sidecar lyrics.
	_, body := admin.get("/api/chunks")
	var listing chunksJSON
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Chunks) != 1 {
		t.Fatalf("listing = %+v", listing)
	}
	c := listing.Chunks[0]
	if c.Language != "tl" || c.LanguageName != "Tagalog" {
		t.Fatalf("language = %q name = %q", c.Language, c.LanguageName)
	}
	if !strings.Contains(c.Lyrics, "line one") {
		t.Fatalf("lyrics = %q", c.Lyrics)
	}

	// Download variant announces an attachment with the on-disk name.
	resp, _ := admin.get(c.URL + "?dl=1")
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("disposition = %q", cd)
	}

	// Moving into a free-text tag creates the group.
	resp, body = admin.postAPI("/chunks/move", url.Values{"to": {"Late Night!"}, "item": {"untagged/" + file}})
	if resp.StatusCode != 200 || !strings.Contains(body, `"moved":1`) {
		t.Fatalf("move = %d %s", resp.StatusCode, body)
	}
	moved := filepath.Join(dir, "late_night", file)
	if _, err := os.Stat(moved); err != nil {
		t.Fatal("file did not move")
	}
	if _, err := os.Stat(snippets.LyricsSidecar(moved)); err != nil {
		t.Fatal("sidecar did not move")
	}

	// Delete removes both files.
	resp, body = admin.postAPI("/chunks/delete", url.Values{"tag": {"late_night"}, "file": {file}})
	if resp.StatusCode != 200 {
		t.Fatalf("delete = %d %s", resp.StatusCode, body)
	}
	if _, err := os.Stat(moved); err == nil {
		t.Fatal("file survived delete")
	}
}

func TestChunkRenameRewritesTitleAndName(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	h := newHarness(t, nil)
	admin := h.admin()
	dir := h.s.catalog.Dir()
	os.MkdirAll(filepath.Join(dir, "untagged"), 0o755)
	file := "20260831-120000-old-name.ru.mp3"
	path := filepath.Join(dir, "untagged", file)
	samples := make([]int16, 4800*2)
	if err := export.EncodeMP3(context.Background(), samples, path, export.MP3Options{Title: "Old Name"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(snippets.LyricsSidecar(path), []byte("words"), 0o644)

	resp, body := admin.postAPI("/chunks/rename", url.Values{
		"tag": {"untagged"}, "file": {file}, "title": {"Дождь на закате"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("rename = %d %s", resp.StatusCode, body)
	}
	newFile := "20260831-120000-дождь-на-закате.ru.mp3"
	if !strings.Contains(body, newFile) {
		t.Fatalf("rename reply = %s", body)
	}
	newPath := filepath.Join(dir, "untagged", newFile)
	if _, err := os.Stat(newPath); err != nil {
		t.Fatal("renamed file missing")
	}
	if _, err := os.Stat(snippets.LyricsSidecar(newPath)); err != nil {
		t.Fatal("sidecar did not follow")
	}
	info, err := snippets.ReadInfo(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "Дождь на закате" {
		t.Fatalf("ID3 title = %q", info.Title)
	}
}

// The phone showed "2 ready" while half an hour of music sat on disk:
// it was rendering the in-memory prefetch, which phased generation
// pins at two regardless of how far ahead the radio actually is.
func TestReadySummaryReportsTheDiskBuffer(t *testing.T) {
	fused := player.Status{Queued: 3}
	if got := readySummary(fused); got != "3 ready" {
		t.Errorf("fused summary = %q, want %q", got, "3 ready")
	}
	phased := player.Status{Phased: true, Queued: 2, BufferedTracks: 11, BufferedSeconds: 37 * 60}
	if got := readySummary(phased); got != "11 ready · 37m" {
		t.Errorf("phased summary = %q, want %q", got, "11 ready · 37m")
	}
	deep := player.Status{Phased: true, Queued: 2, BufferedTracks: 40, BufferedSeconds: 2*60*60 + 7*60}
	if got := readySummary(deep); got != "40 ready · 2h07m" {
		t.Errorf("deep summary = %q, want %q", got, "40 ready · 2h07m")
	}
	empty := player.Status{Phased: true, Queued: 0}
	if got := readySummary(empty); got != "0 ready" {
		t.Errorf("empty summary = %q, want %q", got, "0 ready")
	}
}

// Nothing the remote serves may write to the clipboard. A page that
// reaches for it can pester a listener with system "copied"
// notifications while they are only trying to hear music, and the
// words and the invitation link are both plain selectable text.
func TestNoPageWritesToTheClipboard(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	admin.postForm("/users/create", url.Values{"email": {"jane@example.com"}})
	pages := map[string]string{}
	for _, path := range []string{"/", "/users", "/account"} {
		_, body := admin.get(path)
		pages[path] = body
	}
	_, app := admin.get("/app.js")
	pages["/app.js"] = app
	for path, body := range pages {
		for _, banned := range []string{"navigator.clipboard", "execCommand(\"copy\")", "execCommand('copy')"} {
			// Comments may name the clipboard; code may not call it.
			for _, line := range strings.Split(body, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
					continue
				}
				if strings.Contains(line, banned) {
					t.Errorf("%s reaches for the clipboard: %q", path, strings.TrimSpace(line))
				}
			}
		}
	}
}
