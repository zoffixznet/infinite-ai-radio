//go:build browser

package remote_test

// A real-browser test of the phone remote: headless Firefox, driven over
// the WebDriver protocol through geckodriver, against the real binary
// with a sandboxed data directory and the null audio backend. Firefox's
// own audio output goes to a PulseAudio/PipeWire null sink created for
// the run, so nothing is ever audible.
//
// Run with: make browser-test
// Set IAR_SHOTS=<dir> to also capture phone-size screenshots of each page.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"iar/internal/audio"
	"iar/internal/export"
)

// --- minimal WebDriver client ---

type webDriver struct {
	t    *testing.T
	base string // http://127.0.0.1:port/session/<id>
}

func wdCall(t *testing.T, method, url string, body any) json.RawMessage {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	} else if method == "POST" {
		buf.WriteString("{}")
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("webdriver %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Value json.RawMessage `json:"value"`
	}
	json.Unmarshal(raw, &out)
	if resp.StatusCode != 200 {
		t.Fatalf("webdriver %s %s: %d %s", method, url, resp.StatusCode, raw)
	}
	return out.Value
}

func newWebDriver(t *testing.T, driverURL string) *webDriver {
	t.Helper()
	caps := map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{
		"browserName": "firefox",
		"moz:firefoxOptions": map[string]any{
			"args": []string{"-headless"},
			"prefs": map[string]any{
				"media.autoplay.default":         0,
				"media.autoplay.blocking_policy": 0,
			},
		},
	}}}
	v := wdCall(t, "POST", driverURL+"/session", caps)
	var s struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(v, &s)
	if s.SessionID == "" {
		t.Fatalf("no session id in %s", v)
	}
	wd := &webDriver{t: t, base: driverURL + "/session/" + s.SessionID}
	t.Cleanup(func() { wdCall(t, "DELETE", wd.base, nil) })
	wdCall(t, "POST", wd.base+"/window/rect", map[string]any{"width": 390, "height": 844})
	return wd
}

func (w *webDriver) navigate(url string) {
	wdCall(w.t, "POST", w.base+"/url", map[string]string{"url": url})
}

func (w *webDriver) url() string {
	var s string
	json.Unmarshal(wdCall(w.t, "GET", w.base+"/url", nil), &s)
	return s
}

func (w *webDriver) find(css string) string {
	v := wdCall(w.t, "POST", w.base+"/element", map[string]string{"using": "css selector", "value": css})
	var m map[string]string
	json.Unmarshal(v, &m)
	for _, id := range m {
		return id
	}
	w.t.Fatalf("element %q not found: %s", css, v)
	return ""
}

func (w *webDriver) click(css string) {
	wdCall(w.t, "POST", w.base+"/element/"+w.find(css)+"/click", nil)
}

func (w *webDriver) typeInto(css, text string) {
	wdCall(w.t, "POST", w.base+"/element/"+w.find(css)+"/value", map[string]string{"text": text})
}

// exec runs a script synchronously and decodes its return value.
func (w *webDriver) exec(script string, out any) {
	v := wdCall(w.t, "POST", w.base+"/execute/sync", map[string]any{"script": script, "args": []any{}})
	if out != nil {
		json.Unmarshal(v, out)
	}
}

// execAsync runs a script that calls its last argument with a result.
func (w *webDriver) execAsync(script string, out any) {
	v := wdCall(w.t, "POST", w.base+"/execute/async", map[string]any{"script": script, "args": []any{}})
	if out != nil {
		json.Unmarshal(v, out)
	}
}

func (w *webDriver) deleteCookies() {
	wdCall(w.t, "DELETE", w.base+"/cookie", nil)
}

// findXPath locates an element by XPath.
func (w *webDriver) findXPath(xpath string) string {
	v := wdCall(w.t, "POST", w.base+"/element", map[string]string{"using": "xpath", "value": xpath})
	var m map[string]string
	json.Unmarshal(v, &m)
	for _, id := range m {
		return id
	}
	w.t.Fatalf("xpath %q not found", xpath)
	return ""
}

// alertText reads the open confirmation dialog's text.
func (w *webDriver) alertText() string {
	var s string
	json.Unmarshal(wdCall(w.t, "GET", w.base+"/alert/text", nil), &s)
	return s
}

// acceptAlert presses OK on the open dialog.
func (w *webDriver) acceptAlert() {
	wdCall(w.t, "POST", w.base+"/alert/accept", nil)
}

// sessionButton is the XPath of an action button inside a session row.
func sessionButton(name, label string) string {
	return fmt.Sprintf(`//div[@id='sessions']//div[contains(@class,'sess')][.//div[@class='ctitle'][starts-with(normalize-space(),'%s')]]//button[normalize-space()='%s']`, name, label)
}

func (w *webDriver) screenshot(path string) {
	if path == "" {
		return
	}
	var b64 string
	json.Unmarshal(wdCall(w.t, "GET", w.base+"/screenshot", nil), &b64)
	png, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(path, png, 0o644); err != nil {
		w.t.Fatal(err)
	}
	w.t.Logf("screenshot: %s (%d bytes)", path, len(png))
}

// waitFor polls cond until it holds or the budget runs out.
func waitFor(t *testing.T, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- sandboxed product setup ---

func need(t *testing.T, bins ...string) {
	t.Helper()
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not installed; the browser test needs %s", b, strings.Join(bins, ", "))
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// nullSink loads a PulseAudio/PipeWire null sink for the test and
// returns its name and index.
func nullSink(t *testing.T) (name string, index string) {
	t.Helper()
	name = fmt.Sprintf("iar-browser-test-%d", os.Getpid())
	out, err := exec.Command("pactl", "load-module", "module-null-sink", "sink_name="+name,
		"sink_properties=device.description="+name).Output()
	if err != nil {
		t.Skipf("cannot create a null sink (pactl): %v", err)
	}
	module := strings.TrimSpace(string(out))
	t.Cleanup(func() { exec.Command("pactl", "unload-module", module).Run() })
	sinks, _ := exec.Command("pactl", "list", "short", "sinks").Output()
	for _, line := range strings.Split(string(sinks), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == name {
			index = f[0]
		}
	}
	if index == "" {
		t.Fatalf("null sink %s not listed", name)
	}
	return name, index
}

// firefoxSinkIndex finds which sink Firefox's playback stream targets.
func firefoxSinkIndex() (string, bool) {
	out, err := exec.Command("pactl", "list", "sink-inputs").Output()
	if err != nil {
		return "", false
	}
	sink := ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Sink:") {
			sink = strings.TrimSpace(strings.TrimPrefix(line, "Sink:"))
		}
		if strings.HasPrefix(line, "application.name") && strings.Contains(strings.ToLower(line), "firefox") {
			return sink, true
		}
	}
	return "", false
}

type sandbox struct {
	dir, bin string
	port     int
	base     string
	player   *exec.Cmd
	stdin    io.WriteCloser
}

func buildBinary(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "iar")
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/iar")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func startSandbox(t *testing.T) *sandbox {
	t.Helper()
	dir := t.TempDir()
	sb := &sandbox{dir: dir, bin: buildBinary(t, dir), port: freePort(t)}
	sb.base = fmt.Sprintf("http://127.0.0.1:%d", sb.port)
	os.MkdirAll(filepath.Join(dir, "config"), 0o755)
	os.WriteFile(filepath.Join(dir, "config", "config.json"),
		[]byte(fmt.Sprintf(`{"remote":{"enabled":true,"port":%d}}`, sb.port)), 0o600)
	env := append(os.Environ(), "IAR_DATA_DIR="+filepath.Join(dir, "data"), "IAR_CONFIG_DIR="+filepath.Join(dir, "config"))

	// First admin, the way the README says.
	setup := exec.Command(sb.bin, "remote", "setup", "--email", "admin@example.com", "--password-stdin")
	setup.Env = env
	setup.Stdin = strings.NewReader("admin-pass-1\n")
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("remote setup: %v\n%s", err, out)
	}

	// One saved chunk so saved mode has something to play: 3 s tone.
	samples := make([]int16, 3*audio.SampleRate*audio.Channels)
	for i := 0; i < len(samples); i += 2 {
		v := int16(6000 * math.Sin(2*math.Pi*330*float64(i/2)/audio.SampleRate))
		samples[i], samples[i+1] = v, v
	}
	chunk := filepath.Join(dir, "data", "snippets", "demo", "20260821-120000-demo-tone.mp3")
	os.MkdirAll(filepath.Dir(chunk), 0o755)
	if err := export.EncodeMP3(context.Background(), samples, chunk, export.MP3Options{Title: "demo tone", Album: "demo"}); err != nil {
		t.Fatal(err)
	}

	// The player: noise engine (no GPU), null audio, plain mode with
	// stdin held open.
	sb.player = exec.Command(sb.bin, "--engine", "noise", "--player", "null", "--plain")
	sb.player.Env = env
	var err error
	sb.stdin, err = sb.player.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	logFile, _ := os.Create(filepath.Join(dir, "player.out"))
	sb.player.Stdout = logFile
	sb.player.Stderr = logFile
	if err := sb.player.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		io.WriteString(sb.stdin, "quit\n")
		done := make(chan struct{})
		go func() { sb.player.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			sb.player.Process.Kill()
		}
		logFile.Close()
		if t.Failed() {
			out, _ := os.ReadFile(filepath.Join(dir, "player.out"))
			t.Logf("player output:\n%s", out)
		}
	})
	waitFor(t, 20*time.Second, "remote to listen", func() bool {
		resp, err := http.Get(sb.base + "/login")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	return sb
}

func startGeckodriver(t *testing.T, sinkName string) string {
	t.Helper()
	port := freePort(t)
	cmd := exec.Command("geckodriver", "--port", fmt.Sprint(port))
	// Firefox inherits the environment: its audio goes to the null sink.
	cmd.Env = append(os.Environ(), "PULSE_SINK="+sinkName, "MOZ_HEADLESS=1")
	logFile, _ := os.Create(filepath.Join(t.TempDir(), "geckodriver.log"))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		logFile.Close()
	})
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitFor(t, 15*time.Second, "geckodriver", func() bool {
		resp, err := http.Get(url + "/status")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	return url
}

// audioProgress reads an <audio> element's progress.
type audioProgress struct {
	Time  float64 `json:"t"`
	Ready int     `json:"r"`
	Pill  string  `json:"pill"`
}

func (w *webDriver) audioState(elementID string) (audioProgress, bool) {
	var p *audioProgress
	w.exec(`var a=document.getElementById('`+elementID+`'); if(!a) return null;
		var pill=document.getElementById('streamstate');
		return {t:a.currentTime, r:a.readyState, pill: pill ? pill.textContent : ''};`, &p)
	if p == nil {
		return audioProgress{}, false
	}
	return *p, true
}

// assertPlays waits until the element has advanced by at least minAdvance
// seconds with healthy buffering.
func assertPlays(t *testing.T, w *webDriver, elementID string, minAdvance float64, budget time.Duration) audioProgress {
	t.Helper()
	var first, last audioProgress
	started := false
	waitFor(t, budget, elementID+" to advance "+fmt.Sprint(minAdvance)+"s", func() bool {
		p, ok := w.audioState(elementID)
		if !ok {
			return false
		}
		if !started && p.Time > 0 {
			first, started = p, true
		}
		last = p
		return started && p.Time-first.Time >= minAdvance
	})
	if last.Ready < 3 {
		t.Fatalf("%s readyState = %d (expected HAVE_FUTURE_DATA or better)", elementID, last.Ready)
	}
	return last
}

var linkRe = regexp.MustCompile(`^https?://[^/]+/set-password/[A-Za-z0-9_-]+$`)

func TestRealBrowser(t *testing.T) {
	need(t, "geckodriver", "firefox", "pactl", "ffmpeg", "go")
	shots := os.Getenv("IAR_SHOTS")
	shot := func(name string) string {
		if shots == "" {
			return ""
		}
		os.MkdirAll(shots, 0o755)
		return filepath.Join(shots, name)
	}

	sinkName, sinkIndex := nullSink(t)
	sb := startSandbox(t)
	driver := startGeckodriver(t, sinkName)
	w := newWebDriver(t, driver)

	// --- login page ---
	w.navigate(sb.base + "/")
	waitFor(t, 10*time.Second, "login redirect", func() bool { return strings.HasSuffix(w.url(), "/login") })
	w.screenshot(shot("remote-login.png"))
	w.typeInto("#email", "admin@example.com")
	w.typeInto("#password", "admin-pass-1")
	w.click("button[type=submit]")
	waitFor(t, 10*time.Second, "player page", func() bool { return strings.TrimSuffix(w.url(), "/") == sb.base })

	// --- live stream plays (audio to the null sink) ---
	waitFor(t, 10*time.Second, "permissions applied", func() bool {
		var hidden bool
		w.exec(`return document.getElementById('savecard').hidden;`, &hidden)
		return !hidden
	})
	w.click("#play")
	live := assertPlays(t, w, "liveaudio", 3, 30*time.Second)
	if !strings.HasPrefix(live.Pill, "playing") {
		t.Fatalf("stream pill says %q while audio advances", live.Pill)
	}
	t.Logf("live stream: currentTime %.1fs readyState %d pill %q", live.Time, live.Ready, live.Pill)
	waitFor(t, 10*time.Second, "firefox stream on the null sink", func() bool {
		got, ok := firefoxSinkIndex()
		return ok && got == sinkIndex
	})
	t.Logf("firefox audio confirmed on null sink %s (#%s)", sinkName, sinkIndex)
	w.screenshot(shot("remote-player.png"))

	// --- saved chunks: client-side playback of a saved MP3 ---
	w.click("#mode-saved")
	waitFor(t, 10*time.Second, "chunk list", func() bool {
		var n int
		w.exec(`return document.querySelectorAll('#chunks .chunk').length;`, &n)
		return n == 1
	})
	var liveGone bool
	w.exec(`return document.getElementById('liveaudio') === null;`, &liveGone)
	if !liveGone {
		t.Fatal("switching to saved mode must stop the live stream element")
	}
	w.click("#chunks .chunk button.action")
	saved := assertPlays(t, w, "savedaudio", 1.5, 20*time.Second)
	t.Logf("saved chunk: currentTime %.1fs readyState %d", saved.Time, saved.Ready)
	w.click("#chunks .chunk button.action:nth-of-type(2)") // Loop this one
	var loopText string
	w.exec(`return document.getElementById('loopstate').textContent;`, &loopText)
	if !strings.Contains(loopText, "Looping one chunk") {
		t.Fatalf("loop indicator = %q", loopText)
	}
	w.screenshot(shot("remote-saved.png"))
	w.click("#backloop")
	w.exec(`return document.getElementById('loopstate').textContent;`, &loopText)
	if strings.Contains(loopText, "one chunk") {
		t.Fatalf("loop indicator after back = %q", loopText)
	}
	w.click("#mode-live")

	// --- sessions: save under a name, start a preset, delete one ---
	w.typeInto("#sessname", "Road Trip")
	w.click("#sesssave")
	var ackText string
	waitFor(t, 10*time.Second, "session save ack", func() bool {
		w.exec(`return document.getElementById('sessstatus').textContent;`, &ackText)
		return strings.Contains(ackText, "session saved as road-trip")
	})
	waitFor(t, 10*time.Second, "road-trip listed as playing", func() bool {
		var n int
		w.exec(`return document.querySelectorAll('#sessions .sess.playing').length;`, &n)
		var title string
		w.exec(`var e=document.querySelector('#sessions .sess.playing .ctitle'); return e ? e.textContent : '';`, &title)
		return n == 1 && strings.HasPrefix(title, "road-trip")
	})
	wdCall(t, "POST", w.base+"/element/"+w.findXPath(sessionButton("pink-noise", "Start preset"))+"/click", nil)
	waitFor(t, 10*time.Second, "preset start ack", func() bool {
		w.exec(`return document.getElementById('sessstatus').textContent;`, &ackText)
		return strings.Contains(ackText, "preset pink-noise")
	})
	waitFor(t, 10*time.Second, "road-trip no longer playing", func() bool {
		var title string
		w.exec(`var e=document.querySelector('#sessions .sess.playing .ctitle'); return e ? e.textContent : '';`, &title)
		return strings.HasPrefix(title, "pink-noise-")
	})
	// Delete goes through a confirmation dialog.
	wdCall(t, "POST", w.base+"/element/"+w.findXPath(sessionButton("road-trip", "Delete"))+"/click", nil)
	if text := w.alertText(); !strings.Contains(text, "Delete session road-trip?") {
		t.Fatalf("confirmation dialog text = %q", text)
	}
	w.acceptAlert()
	waitFor(t, 10*time.Second, "delete ack", func() bool {
		w.exec(`return document.getElementById('sessstatus').textContent;`, &ackText)
		return strings.Contains(ackText, "session road-trip deleted")
	})
	waitFor(t, 10*time.Second, "road-trip row gone", func() bool {
		var n int
		w.exec(`var rows=document.querySelectorAll('#sessions .sess .ctitle'); var n=0; rows.forEach(function(r){ if (r.textContent.indexOf('road-trip')===0) n++; }); return n;`, &n)
		return n == 0
	})
	if _, err := os.Stat(filepath.Join(sb.dir, "data", "sessions", "road-trip.json")); err == nil {
		t.Fatal("deleted session file still on disk")
	}

	// --- users page: invite a listener with no other permission ---
	w.navigate(sb.base + "/users")
	w.typeInto("form[action='/users/create'] input[name=email]", "listener@example.com")
	w.click("form[action='/users/create'] input[name=steer]") // untick the default
	w.click("form[action='/users/create'] button[type=submit]")
	var link string
	waitFor(t, 10*time.Second, "invite link", func() bool {
		w.exec(`var l=document.getElementById('link'); return l ? l.value : '';`, &link)
		return link != ""
	})
	if !linkRe.MatchString(link) || !strings.HasPrefix(link, sb.base) {
		t.Fatalf("invite link = %q", link)
	}
	w.screenshot(shot("remote-users.png"))

	// --- the invitee: set a password through the link, then listen ---
	w.deleteCookies()
	w.navigate(link)
	var emailShown string
	w.exec(`return document.getElementById('email').value;`, &emailShown)
	if emailShown != "listener@example.com" {
		t.Fatalf("invite page shows %q", emailShown)
	}
	w.typeInto("#password", "listener-pass-1")
	w.typeInto("#password2", "listener-pass-1")
	w.click("button[type=submit]")
	waitFor(t, 10*time.Second, "invitee on the player page", func() bool { return strings.TrimSuffix(w.url(), "/") == sb.base })
	var me struct {
		Email string `json:"email"`
		Save  bool   `json:"save"`
		Steer bool   `json:"steer"`
	}
	w.execAsync(`var cb=arguments[arguments.length-1]; fetch('/me').then(function(r){return r.json()}).then(cb);`, &me)
	if me.Email != "listener@example.com" || me.Save || me.Steer {
		t.Fatalf("invitee /me = %+v", me)
	}
	// Denied actions are hidden...
	waitFor(t, 10*time.Second, "permission hiding", func() bool {
		var hidden []bool
		w.exec(`return [document.getElementById('savecard').hidden, document.getElementById('steercard').hidden, document.querySelector('a[href="/users"]') === null, document.getElementById('sessionsave').hidden, document.querySelectorAll('#sessions button').length === 0 && document.querySelectorAll('#sessions .sess').length > 0];`, &hidden)
		return len(hidden) == 5 && hidden[0] && hidden[1] && hidden[2] && hidden[3] && hidden[4]
	})
	// ...and rejected server-side even when forced.
	var status int
	w.execAsync(`var cb=arguments[arguments.length-1]; fetch('/save',{method:'POST',headers:{'X-IAR-Remote':'1'}}).then(function(r){cb(r.status)});`, &status)
	if status != 403 {
		t.Fatalf("forced /save by a listener = %d, want 403", status)
	}
	// Listening still works for them.
	w.click("#play")
	assertPlays(t, w, "liveaudio", 2, 30*time.Second)
	// The invite link is dead now.
	w.navigate(link)
	var invalid bool
	w.exec(`return document.body.textContent.indexOf('not valid') >= 0;`, &invalid)
	if !invalid {
		t.Fatal("used invite link still works")
	}

	// No underruns while all of that streamed.
	logData, _ := os.ReadFile(filepath.Join(sb.dir, "data", "logs", "iar.log"))
	if bytes.Contains(logData, []byte(`"event":"underrun"`)) {
		t.Fatal("underruns logged during the browser session")
	}
	for _, ev := range []string{"remote_login", "remote_link_issued", "remote_link_redeemed", "remote_denied", "remote_stream_open"} {
		if !bytes.Contains(logData, []byte(`"event":"`+ev+`"`)) {
			t.Errorf("log lacks event %s", ev)
		}
	}
}
