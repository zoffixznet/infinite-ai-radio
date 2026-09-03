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
	"slices"
	"strings"
	"sync"
	"syscall"
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

// The browser runs at phone proportions, so the tests and the README
// screenshots see the layout a listener sees. Firefox will not make its
// window narrower than 500 CSS pixels, so rather than fight it the
// height is the one that gives a phone's 9:19.5 shape at that width
// (500 x 1082 has the same aspect as 390 x 844). The remote's stylesheet
// has no width breakpoints, so the layout is identical either way.
const (
	phoneWidth  = 500
	phoneHeight = 1082
)

func newWebDriver(t *testing.T, driverURL string) *webDriver {
	return newWebDriverScaled(t, driverURL, 1)
}

// newWebDriverScaled opens headless Firefox at phone size. scale is the
// device pixel ratio: 1 for tests, higher for crisp screenshots.
func newWebDriverScaled(t *testing.T, driverURL string, scale int) *webDriver {
	t.Helper()
	caps := map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{
		"browserName": "firefox",
		"moz:firefoxOptions": map[string]any{
			"args": []string{
				"-headless",
				fmt.Sprintf("--width=%d", phoneWidth),
				fmt.Sprintf("--height=%d", phoneHeight),
			},
			"prefs": map[string]any{
				"media.autoplay.default":         0,
				"media.autoplay.blocking_policy": 0,
				"layout.css.devPixelsPerPx":      fmt.Sprint(scale),
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
	wd.fitViewport(phoneWidth, phoneHeight)
	return wd
}

// fitViewport sizes the window until the page sees the viewport we want.
// Firefox clamps the rect it is given and counts it in its own units
// (which a device pixel ratio further skews), so rather than trusting
// one request this measures what the document actually got and corrects
// proportionally until it lands.
func (w *webDriver) fitViewport(width, height int) {
	type size struct {
		W int `json:"w"`
		H int `json:"h"`
	}
	for i := 0; i < 6; i++ {
		var vp size
		w.exec(`return {w: window.innerWidth, h: window.innerHeight};`, &vp)
		if vp.W == width && vp.H == height {
			return
		}
		if vp.W == 0 || vp.H == 0 {
			return
		}
		var rect size
		json.Unmarshal(wdCall(w.t, "GET", w.base+"/window/rect", nil), &struct {
			Width  *int `json:"width"`
			Height *int `json:"height"`
		}{&rect.W, &rect.H})
		wdCall(w.t, "POST", w.base+"/window/rect", map[string]any{
			"width":  int(math.Round(float64(rect.W) * float64(width) / float64(vp.W))),
			"height": int(math.Round(float64(rect.H) * float64(height) / float64(vp.H))),
		})
	}
	var got size
	w.exec(`return {w: window.innerWidth, h: window.innerHeight};`, &got)
	w.t.Logf("viewport settled at %dx%d (wanted %dx%d)", got.W, got.H, width, height)
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
// sessionButton locates a station row's action by what the action does.
// The row itself is the button that starts the station, so the match is
// on data-action rather than on visible text.
func sessionButton(name, action string) string {
	return fmt.Sprintf(`//div[@id='sessions']//*[contains(@class,'sess')][.//*[contains(@class,'ctitle')][starts-with(normalize-space(),'%s')]]/descendant-or-self::button[@data-action='%s']`, name, action)
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
	engine   string   // "noise" or "acestep" (fake daemon)
	args     []string // extra flags for the player process
	player   *exec.Cmd
	stdin    io.WriteCloser
	logFile  *os.File
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
	sb := prepareSandbox(t, "noise", "")
	finishSandbox(t, sb)
	return sb
}

// startMusicSandbox boots a music-mode sandbox: a fake ACE-Step engine
// daemon (instant sine tracks) that the app adopts, so queue, prefetch
// and steering behave as with the real engine, GPU-free.
func startMusicSandbox(t *testing.T) (*sandbox, *fakeEngine) {
	return startMusicSandboxCfg(t, "")
}

// startMusicSandboxCfg is startMusicSandbox with extra top-level config
// keys (raw JSON fragment like `"buffer_tracks":1`).
func startMusicSandboxCfg(t *testing.T, cfgExtra string) (*sandbox, *fakeEngine) {
	sb := prepareSandbox(t, "acestep", cfgExtra)
	// A minimal on-disk install layout so the app agrees the engine
	// exists; generation itself goes to the fake daemon.
	engineDir := filepath.Join(sb.dir, "data", "engine")
	os.MkdirAll(filepath.Join(engineDir, ".venv"), 0o755)
	os.MkdirAll(filepath.Join(engineDir, "checkpoints", "acestep-v15-turbo"), 0o755)
	os.WriteFile(filepath.Join(engineDir, "pyproject.toml"), []byte("[project]\n"), 0o644)
	os.WriteFile(filepath.Join(engineDir, "checkpoints", "acestep-v15-turbo", "weights.bin"), []byte("x"), 0o644)
	fe := startFakeEngine(t, filepath.Join(sb.dir, "data", "state"))
	finishSandbox(t, sb)
	return sb, fe
}

// finishSandbox starts the player and waits for the remote.
func finishSandbox(t *testing.T, sb *sandbox) {
	t.Helper()
	sb.startPlayer(t)
	t.Cleanup(func() {
		sb.stopPlayer()
		if t.Failed() {
			out, _ := os.ReadFile(filepath.Join(sb.dir, "player.out"))
			t.Logf("player output:\n%s", out)
		}
	})
	sb.waitListening(t)
}

// prepareSandbox builds the binary and lays out config, admin account
// and a demo chunk, without starting the player.
func prepareSandbox(t *testing.T, engine, cfgExtra string) *sandbox {
	t.Helper()
	dir := t.TempDir()
	sb := &sandbox{dir: dir, bin: buildBinary(t, dir), port: freePort(t), engine: engine}
	sb.base = fmt.Sprintf("http://127.0.0.1:%d", sb.port)
	os.MkdirAll(filepath.Join(dir, "config"), 0o755)
	if cfgExtra != "" {
		cfgExtra = "," + cfgExtra
	}
	os.WriteFile(filepath.Join(dir, "config", "config.json"),
		[]byte(fmt.Sprintf(`{"remote":{"enabled":true,"port":%d}%s}`, sb.port, cfgExtra)), 0o600)

	// First admin, the way the README says.
	setup := exec.Command(sb.bin, "remote", "setup", "--email", "admin@example.com", "--password-stdin")
	setup.Env = sb.env()
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
	return sb
}

func (sb *sandbox) env() []string {
	return append(os.Environ(),
		"IAR_DATA_DIR="+filepath.Join(sb.dir, "data"),
		"IAR_CONFIG_DIR="+filepath.Join(sb.dir, "config"))
}

// startPlayer launches (or relaunches) the player process: chosen
// engine, null audio, plain mode with stdin held open.
func (sb *sandbox) startPlayer(t *testing.T) {
	t.Helper()
	sb.player = exec.Command(sb.bin, append([]string{
		"--engine", sb.engine, "--player", "null", "--plain"}, sb.args...)...)
	// Tie the player's life to the test's. A test that dies without
	// running its cleanups - killed, panicking, or terminated by the
	// engine supervision it is pretending to be - must not leave a
	// player behind, because a live player keeps spawning engine
	// daemons of its own.
	sb.player.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	sb.player.Env = sb.env()
	var err error
	sb.stdin, err = sb.player.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	sb.logFile, _ = os.OpenFile(filepath.Join(sb.dir, "player.out"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	sb.player.Stdout = sb.logFile
	sb.player.Stderr = sb.logFile
	if err := sb.player.Start(); err != nil {
		t.Fatal(err)
	}
}

// stopPlayer asks the player to quit, killing it if it lingers.
func (sb *sandbox) stopPlayer() {
	if sb.player == nil {
		return
	}
	io.WriteString(sb.stdin, "quit\n")
	done := make(chan struct{})
	go func() { sb.player.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		sb.player.Process.Kill()
	}
	sb.logFile.Close()
	sb.player = nil
}

// killPlayer terminates the player abruptly (a crash or lost machine).
func (sb *sandbox) killPlayer() {
	if sb.player == nil {
		return
	}
	sb.player.Process.Kill()
	sb.player.Wait()
	sb.logFile.Close()
	sb.player = nil
}

func (sb *sandbox) waitListening(t *testing.T) {
	t.Helper()
	waitFor(t, 20*time.Second, "remote to listen", func() bool {
		resp, err := http.Get(sb.base + "/login")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
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
	// A field report of phantom "Copied" notifications during playback:
	// wrap every clipboard write path and count. The page must never
	// touch the clipboard unless a copy button is pressed.
	w.exec(`window.__clipWrites = 0;
		if (navigator.clipboard && navigator.clipboard.writeText) {
			var orig = navigator.clipboard.writeText.bind(navigator.clipboard);
			navigator.clipboard.writeText = function (t) { window.__clipWrites++; return orig(t); };
		}
		var origExec = document.execCommand;
		document.execCommand = function (cmd) {
			if (String(cmd).toLowerCase() === 'copy') window.__clipWrites++;
			return origExec.apply(document, arguments);
		};
		return true;`, nil)
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
	w.click(`#chunks .chunk button[data-action="Play"]`)
	saved := assertPlays(t, w, "savedaudio", 1.5, 20*time.Second)
	t.Logf("saved chunk: currentTime %.1fs readyState %d", saved.Time, saved.Ready)
	w.click(`#chunks .chunk button[data-action="Loop"]`)
	var loopText string
	w.exec(`return document.getElementById('loopstate').textContent;`, &loopText)
	if !strings.Contains(loopText, "Looping one song") {
		t.Fatalf("loop indicator = %q", loopText)
	}
	w.screenshot(shot("remote-saved.png"))
	w.click("#backloop")
	w.exec(`return document.getElementById('loopstate').textContent;`, &loopText)
	if strings.Contains(loopText, "one song") {
		t.Fatalf("loop indicator after back = %q", loopText)
	}
	w.click("#mode-live")

	// --- sessions: save under a name, start a preset, delete one ---
	// Naming a session is a configure-once action and lives in the
	// settings sheet; starting and deleting are on the main screen.
	w.click("#more")
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
	// The per-device settings a traveller depends on: hours of music
	// banked for a dead zone, and a cue when the radio goes quiet.
	var settings struct {
		Tiers []string `json:"tiers"`
		Cues  bool     `json:"cues"`
	}
	w.exec(`var sel=document.getElementById('buflevel');
		var out=[];
		for (var i=0;i<sel.options.length;i++) out.push(sel.options[i].value);
		return {tiers: out, cues: document.getElementById('audiocues').checked};`, &settings)
	if !slices.Contains(settings.Tiers, "ultra") {
		t.Fatalf("no hours-deep buffering tier offered: %v", settings.Tiers)
	}
	if !settings.Cues {
		t.Fatal("audio cues for trouble were not on by default")
	}
	w.click("#sheetclose")
	// Station bands are collapsible; open them all so the target row is
	// clickable.
	w.exec(`document.querySelectorAll('#sessions details').forEach(function (d) { d.open = true; }); return true;`, nil)
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

	// The whole live/saved/session stretch above played, skipped,
	// switched tabs and reloaded state - with zero clipboard writes.
	var clipWrites int
	w.exec(`return window.__clipWrites;`, &clipWrites)
	if clipWrites != 0 {
		t.Fatalf("the page wrote to the clipboard %d time(s) without a copy button being pressed", clipWrites)
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
		w.exec(`return [document.getElementById('savecard').hidden, document.getElementById('steerbox').hidden, document.querySelector('a[href="/users"]') === null, document.getElementById('sessionsave').hidden, document.querySelectorAll('#sessions button').length === 0 && document.querySelectorAll('#sessions .sess').length > 0];`, &hidden)
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

// --- fake ACE-Step engine daemon ---

// fakeEngine serves the slice of the ACE-Step API the app uses,
// producing short sine tracks instantly. Recording it in the engine
// daemon state file makes the app adopt it like a real daemon.
// fakeTask is one job the fake engine accepted.
type fakeTask struct {
	prompt string
	// planOnly marks a planning job: it answers with audio codes and
	// metadata instead of a rendered file.
	planOnly bool
	// seconds is the duration the caller asked for, 0 when unset.
	seconds float64
}

type fakeEngine struct {
	mu      sync.Mutex
	nextID  int
	tasks   map[string]fakeTask // task id -> job
	prompts []string
	// trackSeconds is the length of generated tracks (default 6: short,
	// so track boundaries arrive quickly).
	trackSeconds int
	// lyrics, when set, replaces the per-task placeholder words the
	// engine echoes back.
	lyrics string
}

// setLyrics fixes the words the engine reports for every track.
func (f *fakeEngine) setLyrics(s string) {
	f.mu.Lock()
	f.lyrics = s
	f.mu.Unlock()
}

// setTrackSeconds changes the length of subsequently generated tracks.
func (f *fakeEngine) setTrackSeconds(sec int) {
	f.mu.Lock()
	f.trackSeconds = sec
	f.mu.Unlock()
}

func (f *fakeEngine) handler() http.Handler {
	wrap := func(data any) map[string]any {
		return map[string]any{"data": data, "code": 200, "error": nil}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(wrap(map[string]any{
			"status": "ok", "models_initialized": true, "loaded_model": "acestep-v15-turbo",
		}))
	})
	mux.HandleFunc("/release_task", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Prompt        string  `json:"prompt"`
			SampleQuery   string  `json:"sample_query"`
			PlanOnly      bool    `json:"plan_only"`
			AudioDuration float64 `json:"audio_duration"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		prompt := req.Prompt
		if prompt == "" {
			prompt = req.SampleQuery
		}
		f.mu.Lock()
		f.nextID++
		id := fmt.Sprintf("task-%d", f.nextID)
		f.tasks[id] = fakeTask{prompt: prompt, planOnly: req.PlanOnly, seconds: req.AudioDuration}
		f.prompts = append(f.prompts, prompt)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(wrap(map[string]any{"task_id": id}))
	})
	mux.HandleFunc("/query_result", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TaskIDs []string `json:"task_id_list"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var rows []map[string]any
		f.mu.Lock()
		for _, id := range req.TaskIDs {
			task := f.tasks[id]
			// The engine echoes back the words it sang; the page
			// shows them, so the fake has to return some.
			lyrics := "[Verse]\nthe words for " + id + "\n\n[Chorus]\nand its chorus"
			if f.lyrics != "" {
				lyrics = f.lyrics
			}
			row := map[string]any{
				"status": 1, "prompt": task.prompt, "seed_value": "7", "lyrics": lyrics,
			}
			if task.planOnly {
				// A planning job answers with audio codes and the
				// metadata the render job is built from; no audio
				// exists yet.
				secs := task.seconds
				if secs == 0 {
					secs = float64(f.trackSeconds)
				}
				row["audio_codes"] = "codes-for-" + id
				row["metas"] = map[string]any{
					"bpm": 120, "duration": secs,
					"keyscale": "C major", "timesignature": "4/4",
				}
			} else {
				row["file"] = "/v1/audio?path=" + id
			}
			inner, _ := json.Marshal([]map[string]any{row})
			rows = append(rows, map[string]any{"task_id": id, "status": 1, "result": string(inner)})
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(wrap(rows))
	})
	mux.HandleFunc("/v1/audio", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("path")
		f.mu.Lock()
		task := f.tasks[id]
		n := len(task.prompt) // vary the tone a little per prompt
		secs := f.trackSeconds
		if task.seconds > 0 {
			secs = int(task.seconds)
		}
		f.mu.Unlock()
		freq := 220 + float64(n%12)*30
		samples := make([]int16, secs*audio.SampleRate*audio.Channels)
		for i := 0; i < len(samples); i += 2 {
			v := int16(8000 * math.Sin(2*math.Pi*freq*float64(i/2)/audio.SampleRate))
			samples[i], samples[i+1] = v, v
		}
		w.Write(audio.EncodeWAV(samples))
	})
	return mux
}

// generated returns how many tracks were requested so far.
func (f *fakeEngine) generated() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

// lastPrompt returns the most recent generation prompt.
func (f *fakeEngine) lastPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) == 0 {
		return ""
	}
	return f.prompts[len(f.prompts)-1]
}

// startFakeEngine serves the fake engine and records it as the adopted
// engine daemon in stateDir.
func startFakeEngine(t *testing.T, stateDir string) *fakeEngine {
	t.Helper()
	fe := &fakeEngine{tasks: map[string]fakeTask{}, trackSeconds: 6}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: fe.handler()}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	port := l.Addr().(*net.TCPAddr).Port
	os.MkdirAll(stateDir, 0o755)
	// The app supervises whatever PID the state file names and
	// terminates it when phased generation hibernates the engine. Name a
	// harmless placeholder process, never the test binary - recording
	// our own PID here means the first hibernation kills the test.
	decoy := exec.Command("sleep", "120")
	if err := decoy.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		decoy.Process.Kill()
		decoy.Wait()
	})
	state := fmt.Sprintf(`{"pid": %d, "port": %d, "engine_dir": "", "started": %q}`,
		decoy.Process.Pid, port, time.Now().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(stateDir, "engine.json"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	return fe
}

// login drives the login form for the admin account.
func loginAdmin(t *testing.T, w *webDriver, base string) {
	t.Helper()
	w.navigate(base + "/login")
	w.typeInto("#email", "admin@example.com")
	w.typeInto("#password", "admin-pass-1")
	w.click("button[type=submit]")
	waitFor(t, 10*time.Second, "player page after login", func() bool {
		return strings.TrimSuffix(w.url(), "/") == base
	})
}

// TestRealBrowserResilience covers the failure-and-recovery paths of
// the phone remote against a music-mode sandbox: shared steering state
// across reloads, the Next control, automatic stream recovery after the
// server dies, the terminal expired-login branch, and buffered playback
// continuing across a track boundary with the server gone.
func TestRealBrowserResilience(t *testing.T) {
	need(t, "geckodriver", "firefox", "pactl", "ffmpeg", "go")
	sinkName, _ := nullSink(t)
	sb, fe := startMusicSandbox(t)
	driver := startGeckodriver(t, sinkName)
	w := newWebDriver(t, driver)

	loginAdmin(t, w, sb.base)
	waitFor(t, 30*time.Second, "tracks generating", func() bool { return fe.generated() >= 2 })

	// --- the save button holds still between polls ---
	// The "already saved" look is re-derived on every /state poll. It
	// once came from a lookup that answered undefined rather than
	// false, which classList.toggle reads as "no value given" and so
	// flipped the class: the button flashed between its two looks
	// forever. Nothing here has been saved, so the class must not
	// change at all.
	waitFor(t, 20*time.Second, "the save card", func() bool {
		var hidden bool
		w.exec(`return document.getElementById('savecard').hidden;`, &hidden)
		return !hidden
	})
	w.exec(`window.__saveFlips = 0;
		new MutationObserver(function (ms) { window.__saveFlips += ms.length; })
			.observe(document.getElementById('save'), {attributes: true, attributeFilter: ['class']});
		return 0;`, new(int))
	time.Sleep(7 * time.Second) // three /state polls
	var flips int
	w.exec(`return window.__saveFlips;`, &flips)
	if flips != 0 {
		t.Fatalf("the save button changed its look %d times while nothing was saved", flips)
	}

	// --- steering state is shared and survives a reload ---
	io.WriteString(sb.stdin, "no guitars more synths\n")
	waitFor(t, 15*time.Second, "tweak chip appears", func() bool {
		var n int
		w.exec(`return document.querySelectorAll('#tweaks .chip').length;`, &n)
		return n >= 1
	})
	w.navigate(sb.base + "/")
	var base string
	waitFor(t, 15*time.Second, "steering state after reload", func() bool {
		var chip string
		w.exec(`var c=document.querySelector('#tweaks .chip'); return c ? c.textContent : '';`, &chip)
		w.exec(`return document.getElementById('baseprompt').textContent;`, &base)
		return strings.Contains(chip, "no guitars more synths") && base != "" && base != "…"
	})
	// The steering panel is set-and-forget, so it starts collapsed; the
	// lyrics under it change every track and start open.
	var steerOpen, lyricsOpen bool
	w.exec(`return document.getElementById('steercard').open;`, &steerOpen)
	w.exec(`return document.getElementById('lyricscard').open;`, &lyricsOpen)
	if steerOpen {
		t.Error("the steering panel started open")
	}
	if !lyricsOpen {
		t.Error("the lyrics panel started closed")
	}
	// The words the engine sang reach the page, section markers and all.
	waitFor(t, 20*time.Second, "lyrics on the page", func() bool {
		var lyr string
		w.exec(`return document.getElementById('lyrics').textContent;`, &lyr)
		return strings.Contains(lyr, "the words for task-") && strings.Contains(lyr, "[Chorus]")
	})
	waitFor(t, 20*time.Second, "steer reached generation", func() bool {
		return strings.Contains(fe.lastPrompt(), "synths")
	})
	if strings.Contains(fe.lastPrompt(), "guitar") {
		t.Fatalf("negated instrument still in the prompt: %q", fe.lastPrompt())
	}

	// --- direct stream + Next ---
	w.click("#play")
	assertPlays(t, w, "liveaudio", 2, 30*time.Second)

	// --- the live loop button asks the radio itself ---
	// A generated track is playing (the lyrics assertions above proved
	// it), so the toggle must arm, light the button from /state, and
	// release on the second tap. The earlier steer must have fully
	// landed first: mid-switch the toggle honestly refuses, so wait
	// for /state to stop reporting the switch.
	waitFor(t, 30*time.Second, "a generated track in the now-playing line", func() bool {
		var meta string
		w.exec(`return document.getElementById('meta').textContent;`, &meta)
		return strings.Contains(meta, "Track ")
	})
	waitFor(t, 30*time.Second, "the steer's switch to land", func() bool {
		var st struct {
			Switching bool `json:"switching"`
		}
		w.execAsync(`var cb=arguments[arguments.length-1]; fetch('/state').then(function(r){return r.json()}).then(cb);`, &st)
		return !st.Switching
	})
	w.click("#loop")
	waitFor(t, 10*time.Second, "loop-on ack", func() bool {
		var ack string
		w.exec(`return document.getElementById('steerstatus').textContent;`, &ack)
		return strings.Contains(ack, "looping")
	})
	waitFor(t, 10*time.Second, "loop button lit from /state", func() bool {
		var on bool
		w.exec(`return document.getElementById('loop').classList.contains('on');`, &on)
		return on
	})
	w.click("#loop")
	waitFor(t, 10*time.Second, "loop-off ack", func() bool {
		var ack string
		w.exec(`return document.getElementById('steerstatus').textContent;`, &ack)
		return strings.Contains(ack, "loop off")
	})

	// A marionette click occasionally evaporates mid-repaint (the
	// element is present, unobscured and enabled - verified with
	// elementFromPoint - yet the event never reaches the page), so the
	// skip is clicked until its acknowledgement shows. A second skip
	// landing is harmless here.
	var status string
	skipDeadline := time.Now().Add(15 * time.Second)
	for {
		w.click("#next")
		settle := time.Now().Add(2 * time.Second)
		for time.Now().Before(settle) {
			w.exec(`return document.getElementById('steerstatus').textContent;`, &status)
			if strings.Contains(status, "skipping") {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		if strings.Contains(status, "skipping") {
			break
		}
		if time.Now().After(skipDeadline) {
			t.Fatalf("skip never acknowledged; steerstatus: %q", status)
		}
	}

	// --- the server dies: the stream recovers with no interaction ---
	sb.killPlayer()
	waitFor(t, 30*time.Second, "reconnecting state", func() bool {
		var pill string
		w.exec(`return document.getElementById('streamstate').textContent;`, &pill)
		return strings.Contains(pill, "reconnecting")
	})
	sb.startPlayer(t)
	sb.waitListening(t)
	waitFor(t, 60*time.Second, "stream self-recovers", func() bool {
		p, ok := w.audioState("liveaudio")
		return ok && strings.HasPrefix(p.Pill, "playing")
	})

	// --- a connectivity event during a stall reconnects immediately ---
	// Freezing the server keeps the TCP connection open with no data:
	// the element stalls without erroring. A connectivity signal must
	// then tear it down and reconnect at once, not wait for the
	// watchdog.
	exec.Command("kill", "-STOP", fmt.Sprint(sb.player.Process.Pid)).Run()
	// Wait until playback has made no progress for a sustained stretch
	// (the element keeps playing buffered audio for a while first).
	var lastCT float64 = -1
	var stableSince time.Time
	waitFor(t, 40*time.Second, "playback stalled for several seconds", func() bool {
		p, ok := w.audioState("liveaudio")
		if !ok {
			return false
		}
		if p.Time != lastCT {
			lastCT = p.Time
			stableSince = time.Now()
			return false
		}
		// The page's own progress bookkeeping ticks every 2s, so the
		// stall must comfortably exceed threshold + tick granularity.
		return !stableSince.IsZero() && time.Since(stableSince) > 6*time.Second
	})
	// Mark the stalled element, fire the connectivity event, and expect
	// a NEW element (the old one torn down) well before the watchdog
	// would have acted.
	w.exec(`var a=document.getElementById('liveaudio'); if (a) a.setAttribute('data-stalled','1');
		window.dispatchEvent(new Event('online')); return true;`, nil)
	waitFor(t, 3*time.Second, "immediate reconnect on the connectivity event", func() bool {
		var fresh bool
		w.exec(`var a=document.getElementById('liveaudio');
			return !!a && !a.hasAttribute('data-stalled');`, &fresh)
		return fresh
	})
	exec.Command("kill", "-CONT", fmt.Sprint(sb.player.Process.Pid)).Run()
	waitFor(t, 60*time.Second, "stream recovers after the stall", func() bool {
		p, ok := w.audioState("liveaudio")
		return ok && strings.HasPrefix(p.Pill, "playing")
	})

	// --- an expired login is terminal: no reconnect loop ---
	w.deleteCookies()
	sb.killPlayer()
	sb.startPlayer(t)
	sb.waitListening(t)
	terminal := func(pill string) bool {
		// Both the poll's logged-out branch and the stream's 401
		// classification are correct terminal states.
		return strings.Contains(pill, "session expired") || strings.Contains(pill, "logged out")
	}
	waitFor(t, 60*time.Second, "terminal expired-session state", func() bool {
		var pill string
		w.exec(`return document.getElementById('streamstate').textContent;`, &pill)
		return terminal(pill)
	})
	time.Sleep(3 * time.Second)
	var pill string
	w.exec(`return document.getElementById('streamstate').textContent;`, &pill)
	if !terminal(pill) {
		t.Fatalf("expired session did not stay terminal: %q", pill)
	}

	// --- buffered playback survives the server disappearing ---
	loginAdmin(t, w, sb.base)
	w.exec(`document.getElementById('buffered').click(); return true;`, nil)
	w.click("#play")
	waitFor(t, 60*time.Second, "buffered playback starts", func() bool {
		var st struct {
			Time  float64 `json:"t"`
			Ahead string  `json:"pill"`
		}
		w.exec(`var a=document.getElementById('bufaudio0')||document.getElementById('bufaudio1');
			var pill=document.getElementById('streamstate');
			return {t: a ? a.currentTime : 0, pill: pill.textContent};`, &st)
		return st.Time > 1 && strings.Contains(st.Ahead, "ahead")
	})
	// Wait until at least one track is prefetched ahead.
	waitFor(t, 60*time.Second, "a track prefetched ahead", func() bool {
		var pill string
		w.exec(`return document.getElementById('streamstate').textContent;`, &pill)
		return strings.Contains(pill, "1 ahead") || strings.Contains(pill, "2 ahead") ||
			strings.Contains(pill, "3 ahead") || strings.Contains(pill, "4 ahead") ||
			strings.Contains(pill, "5 ahead")
	})
	// The loop button in buffered mode loops this device's own track:
	// the playing element's loop flag flips with the button, no server
	// round trip involved.
	w.click("#loop")
	waitFor(t, 5*time.Second, "buffered loop engaged", func() bool {
		var st struct {
			Loop bool `json:"loop"`
			On   bool `json:"on"`
		}
		w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
			var el=null; a.forEach(function(x){ if (x && !x.paused) el = x; });
			return {loop: el ? el.loop : false, on: document.getElementById('loop').classList.contains('on')};`, &st)
		return st.Loop && st.On
	})
	w.click("#loop")
	waitFor(t, 5*time.Second, "buffered loop released", func() bool {
		var st struct {
			Loop bool `json:"loop"`
			On   bool `json:"on"`
		}
		w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
			var el=null; a.forEach(function(x){ if (x && !x.paused) el = x; });
			return {loop: el ? el.loop : true, on: document.getElementById('loop').classList.contains('on')};`, &st)
		return !st.Loop && !st.On
	})

	// The media session stays honest in buffered mode: the title is
	// this device's playing track's SHORT name (never the raw prompt),
	// the artist carries the device-local track number and the
	// genre/mood subtitle, and no placeholder ever shows.
	waitFor(t, 15*time.Second, "buffered media-session metadata", func() bool {
		var md struct {
			Title  string `json:"title"`
			Artist string `json:"artist"`
		}
		w.exec(`if (!('mediaSession' in navigator) || !navigator.mediaSession.metadata) return {title:'',artist:''};
			var m = navigator.mediaSession.metadata;
			return {title: m.title, artist: m.artist};`, &md)
		if md.Artist == "buffered playback" {
			t.Fatalf("media session artist is a placeholder: %+v", md)
		}
		if md.Title == "" {
			return false
		}
		// The raw prompt is a comma-tag soup; the short title is not.
		if strings.Contains(md.Title, ",") {
			t.Fatalf("car title looks like a raw prompt: %q", md.Title)
		}
		return strings.HasPrefix(md.Artist, "Track ") && md.Title != "Infinite AI Radio"
	})

	// --- the car's previous-track button saves what the driver hears ---
	// The media-session action is dispatched exactly as Chrome would;
	// buffered mode must save the DEVICE's playing track, flash "Saved:"
	// on the car metadata, and restore the honest title afterwards.
	// Wait for the early part of a track so the played track cannot
	// change under the assertions below.
	waitFor(t, 30*time.Second, "early in a buffered track", func() bool {
		var ct float64
		w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
			for (var i=0;i<2;i++) { if (a[i] && !a[i].paused) return a[i].currentTime; }
			return -1;`, &ct)
		return ct > 0.2 && ct < 3
	})
	w.exec(`document.dispatchEvent(new CustomEvent("iar:msaction", {detail: "previoustrack"})); return true;`, nil)
	waitFor(t, 15*time.Second, "car save acknowledged", func() bool {
		var ack string
		w.exec(`return document.getElementById('savestatus').textContent;`, &ack)
		// The optimistic line already says "saving this track…", so wait
		// for the server's answer - which names the file - rather than
		// racing the request that is still in flight.
		return strings.Contains(ack, "saving this track to ") || strings.Contains(ack, "already saved:")
	})
	// The on-page save button greys out for this device's track at once.
	var greyed bool
	w.exec(`return document.getElementById('save').classList.contains('saved');`, &greyed)
	if !greyed {
		t.Fatal("save button not greyed after the car save")
	}
	var flash string
	w.exec(`return navigator.mediaSession.metadata ? navigator.mediaSession.metadata.title : '';`, &flash)
	if !strings.HasPrefix(flash, "Saved: ") {
		t.Fatalf("car metadata flash = %q", flash)
	}
	waitFor(t, 6*time.Second, "car metadata restored after the flash", func() bool {
		var title string
		w.exec(`return navigator.mediaSession.metadata ? navigator.mediaSession.metadata.title : '';`, &title)
		return title != "" && !strings.HasPrefix(title, "Saved: ")
	})
	waitFor(t, 15*time.Second, "saved snippet file on disk", func() bool {
		matches, _ := filepath.Glob(filepath.Join(sb.dir, "data", "snippets", "untagged", "*.mp3"))
		return len(matches) == 1
	})
	// A rapid burst of car-button presses collapses to at most one
	// save (debounce + per-track idempotency); the played track may
	// legitimately have advanced once meanwhile.
	before, _ := filepath.Glob(filepath.Join(sb.dir, "data", "snippets", "untagged", "*.mp3"))
	w.exec(`for (var i=0;i<3;i++) document.dispatchEvent(new CustomEvent("iar:msaction", {detail: "previoustrack"})); return true;`, nil)
	time.Sleep(2 * time.Second)
	if after, _ := filepath.Glob(filepath.Join(sb.dir, "data", "snippets", "untagged", "*.mp3")); len(after) > len(before)+1 {
		t.Fatalf("car-button spam produced %d new files", len(after)-len(before))
	}
	// At maximum depth the prefetcher reaches the library-kind filler
	// rows, whose ids contain a slash and travel through the escaped
	// track route; a banked track landing in IndexedDB proves that
	// path end to end in a real browser.
	w.exec(`var sel=document.getElementById('buflevel'); sel.value='max';
		sel.dispatchEvent(new Event('change')); return true;`, nil)
	waitFor(t, 60*time.Second, "a library-kind track prefetched", func() bool {
		var keys []string
		w.execAsync(`var cb=arguments[arguments.length-1];
			var req=indexedDB.open("iar-radio",1);
			req.onerror=function(){cb([])};
			req.onsuccess=function(){
				try {
					var tx=req.result.transaction("tracks","readonly");
					tx.objectStore("tracks").getAllKeys().onsuccess=function(e){cb(e.target.result)};
				} catch (err) { cb([]); }
			};`, &keys)
		for _, k := range keys {
			if strings.HasPrefix(k, "lib:") {
				return true
			}
		}
		return false
	})
	var srcBefore string
	w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
		for (var i=0;i<2;i++) { if (a[i] && !a[i].paused) return a[i].src; }
		return '';`, &srcBefore)
	sb.killPlayer()
	// Playback must continue and cross into the next prefetched track.
	// The banked tracks run minutes long, so a natural boundary cannot
	// arrive inside a test budget: jump the playing track to just
	// before its end. The boundary logic - onended advancing into the
	// staged element with the server gone - is what is under test.
	w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
		for (var i=0;i<2;i++) { if (a[i] && !a[i].paused && isFinite(a[i].duration) && a[i].duration > 3) { a[i].currentTime = a[i].duration - 2; break; } }
		return true;`, nil)
	waitFor(t, 45*time.Second, "playback continues across a boundary offline", func() bool {
		var cur struct {
			Src  string  `json:"src"`
			Time float64 `json:"t"`
		}
		w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
			for (var i=0;i<2;i++) { if (a[i] && !a[i].paused && a[i].currentTime>0.5) return {src:a[i].src, t:a[i].currentTime}; }
			return {src:'', t:0};`, &cur)
		return cur.Src != "" && cur.Src != srcBefore
	})
	sb.startPlayer(t)
	sb.waitListening(t)
}

// TestRealBrowserBufferedNextExclusive reproduces the smallest store the
// buffered player can have (the playing track plus exactly one other:
// server buffer of one track, no library filler) and asserts a manual
// Next is a true swap: the outgoing element is paused and disarmed within a
// short deadline, exactly one element produces audio at all times, and
// the skipped track cannot replay through a stale ended handler.
func TestRealBrowserBufferedNextExclusive(t *testing.T) {
	need(t, "geckodriver", "firefox", "pactl", "ffmpeg", "go")
	sinkName, _ := nullSink(t)
	sb, fe := startMusicSandboxCfg(t, `"buffer_tracks":1,"library_max_mb":0`)
	// Long enough that the single-slot queue is reliably occupied
	// (generation takes ~2-3s per track).
	fe.setTrackSeconds(20)
	driver := startGeckodriver(t, sinkName)
	w := newWebDriver(t, driver)
	loginAdmin(t, w, sb.base)

	w.exec(`document.getElementById('buffered').click(); return true;`, nil)
	w.click("#play")

	type bufState struct {
		Playing  int      `json:"playing"`
		PlaySrc  string   `json:"playsrc"`
		Others   []string `json:"others"`
		Paused   bool     `json:"paused"`   // the non-playing element is paused (or absent)
		Disarmed bool     `json:"disarmed"` // the non-playing element has no ended handler
	}
	readState := func() bufState {
		var st bufState
		w.exec(`var els=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
			var out={playing:0, playsrc:'', others:[], paused:true, disarmed:true};
			for (var i=0;i<2;i++) {
				var a=els[i];
				if (!a) continue;
				if (!a.paused && !a.ended) { out.playing++; out.playsrc=a.src; }
				else {
					out.others.push(a.src||'');
					if (!a.paused) out.paused=false;
					if (a.onended) out.disarmed=false;
				}
			}
			// A second playing element means the outgoing one was neither
			// paused nor disarmed.
			if (out.playing>1) { out.paused=false; out.disarmed=false; }
			return out;`, &st)
		return st
	}

	// Wait for playback plus exactly one prefetched track (store of 2).
	waitFor(t, 60*time.Second, "playing with one track ahead", func() bool {
		var pill string
		w.exec(`return document.getElementById('streamstate').textContent;`, &pill)
		st := readState()
		return st.Playing == 1 && strings.Contains(pill, "1 ahead")
	})
	before := readState()

	w.click("#next")

	// The device-local skip is acknowledged honestly (read before the
	// status line's auto-clear).
	var status string
	w.exec(`return document.getElementById('steerstatus').textContent;`, &status)
	if !strings.Contains(status, "this device") {
		t.Fatalf("skip ack does not say it is device-local: %q", status)
	}

	// Within a short deadline the swap must be exclusive: one element
	// playing the OTHER track, the outgoing element paused and disarmed.
	waitFor(t, 3*time.Second, "exclusive swap to the staged track", func() bool {
		st := readState()
		return st.Playing == 1 && st.PlaySrc != before.PlaySrc && st.Paused && st.Disarmed
	})
	after := readState()

	// Hold the invariant across the window where the skipped track's
	// stale ended handler would fire (fake tracks are 6s long): never
	// two elements playing, never a handler re-armed on a paused
	// element, and the new track is never cut back to the skipped one.
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		st := readState()
		if st.Playing > 1 {
			t.Fatalf("two elements playing at once after Next: %+v", st)
		}
		if !st.Paused {
			t.Fatalf("outgoing element not paused after Next: %+v", st)
		}
		if st.Playing == 1 && st.PlaySrc == before.PlaySrc {
			var ct float64
			w.exec(`var els=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
				for (var i=0;i<2;i++) { if (els[i] && !els[i].paused) return els[i].currentTime; }
				return 0;`, &ct)
			// Returning to the skipped track is legitimate only as the
			// natural wraparound AFTER the new track played through.
			if time.Since(deadline.Add(-7*time.Second)) < 4500*time.Millisecond {
				t.Fatalf("skipped track replayed %.1fs after Next (stale handler)", time.Since(deadline.Add(-7*time.Second)).Seconds())
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	_ = after
}

// TestRealBrowserAutoResume covers the car-off pause machinery: a
// system pause (one our code did not initiate) must keep the audio
// element intact with an honest status instead of tearing it down and
// restarting playback, and the media-session play action, the play
// button and a page reload must resume it in place.
func TestRealBrowserAutoResume(t *testing.T) {
	need(t, "geckodriver", "firefox", "pactl", "ffmpeg", "go")
	sinkName, _ := nullSink(t)
	sb, fe := startMusicSandbox(t)
	_ = fe
	driver := startGeckodriver(t, sinkName)
	w := newWebDriver(t, driver)
	loginAdmin(t, w, sb.base)

	pill := func() string {
		var s string
		w.exec(`return document.getElementById('streamstate').textContent;`, &s)
		return s
	}

	// --- direct mode ---
	w.click("#play")
	assertPlays(t, w, "liveaudio", 2, 30*time.Second)

	// A pause we did not initiate is a system pause: honest status,
	// paused media session, element kept.
	w.exec(`document.getElementById('liveaudio').pause(); return true;`, nil)
	waitFor(t, 10*time.Second, "system-pause status", func() bool {
		return strings.Contains(pill(), "audio output disconnected")
	})
	var msState string
	w.exec(`return navigator.mediaSession.playbackState;`, &msState)
	if msState != "paused" {
		t.Fatalf("media session playbackState = %q", msState)
	}
	// The watchdog and connectivity nudges must leave the paused
	// element alone (no teardown, no loudspeaker restart).
	w.exec(`window.dispatchEvent(new Event('online')); return true;`, nil)
	time.Sleep(9 * time.Second)
	var kept bool
	w.exec(`var a=document.getElementById('liveaudio'); return !!a && a.paused;`, &kept)
	if !kept {
		t.Fatal("paused element was torn down or restarted")
	}
	if !strings.Contains(pill(), "audio output disconnected") {
		t.Fatalf("paused status lost: %q", pill())
	}

	// The car's play command resumes the SAME element in place.
	var before float64
	w.exec(`return document.getElementById('liveaudio').currentTime;`, &before)
	w.exec(`document.dispatchEvent(new CustomEvent("iar:msaction", {detail: "play"})); return true;`, nil)
	waitFor(t, 15*time.Second, "resume after the play action", func() bool {
		var st struct {
			Time   float64 `json:"t"`
			Paused bool    `json:"p"`
		}
		w.exec(`var a=document.getElementById('liveaudio'); return a ? {t:a.currentTime, p:a.paused} : {t:0,p:true};`, &st)
		return !st.Paused && st.Time > before+0.5
	})

	// The play button resumes a pending pause too (it must not stop).
	w.exec(`document.getElementById('liveaudio').pause(); return true;`, nil)
	waitFor(t, 10*time.Second, "second system pause", func() bool {
		return strings.Contains(pill(), "audio output disconnected")
	})
	w.click("#play")
	waitFor(t, 15*time.Second, "resume after tapping play", func() bool {
		var paused bool
		w.exec(`var a=document.getElementById('liveaudio'); return a ? a.paused : true;`, &paused)
		return !paused
	})

	// --- cold start: a reload while listening resumes by itself ---
	w.navigate(sb.base + "/")
	waitFor(t, 30*time.Second, "playback after a reload", func() bool {
		var t2 float64
		w.exec(`var a=document.getElementById('liveaudio'); return a ? a.currentTime : -1;`, &t2)
		return t2 > 0.5
	})

	// --- buffered mode ---
	w.exec(`document.getElementById('buffered').click(); return true;`, nil)
	waitFor(t, 60*time.Second, "buffered playback", func() bool {
		var ct float64
		w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
			for (var i=0;i<2;i++) { if (a[i] && !a[i].paused) return a[i].currentTime; }
			return -1;`, &ct)
		return ct > 1
	})
	w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
		for (var i=0;i<2;i++) { if (a[i] && !a[i].paused) { a[i].pause(); break; } }
		return true;`, nil)
	waitFor(t, 10*time.Second, "buffered system-pause status", func() bool {
		return strings.Contains(pill(), "audio output disconnected")
	})
	var pausedID string
	w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
		for (var i=0;i<2;i++) { if (a[i] && a[i].currentTime>0 && a[i].paused) return a[i].id; }
		return '';`, &pausedID)
	w.exec(`document.dispatchEvent(new CustomEvent("iar:msaction", {detail: "play"})); return true;`, nil)
	waitFor(t, 15*time.Second, "buffered resume on the same element", func() bool {
		var ok bool
		w.exec(`var a=document.getElementById('`+pausedID+`'); return !!a && !a.paused && a.currentTime>0.2;`, &ok)
		return ok
	})
	// Exactly one element audible after the resume.
	var playing int
	w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
		var n=0; for (var i=0;i<2;i++) { if (a[i] && !a[i].paused && !a[i].ended) n++; }
		return n;`, &playing)
	if playing != 1 {
		t.Fatalf("%d elements playing after resume", playing)
	}
}
