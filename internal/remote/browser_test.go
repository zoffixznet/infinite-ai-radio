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

// typeIntoAlert fills the open prompt box before it is accepted.
func (w *webDriver) typeIntoAlert(text string) {
	wdCall(w.t, "POST", w.base+"/alert/text", map[string]string{"text": text})
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
	// The live row and the saved one are painted by the same code now.
	// The direct stream has no rewind, so its row stays grayed with the
	// slider parked - the half of that shared code the saved screen
	// never exercises.
	var liveRow struct {
		Class string `json:"class"`
		Value string `json:"value"`
	}
	waitFor(t, 10*time.Second, "the live position row grayed on the direct stream", func() bool {
		w.exec(`return {class: document.getElementById('seekrow').className,
			value: document.getElementById('seek').value};`, &liveRow)
		return strings.Contains(liveRow.Class, "disabled") && liveRow.Value == "0"
	})
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
	// The song playing on this device is the top of the screen, the way
	// it is on the live page: name, facts, position row - all of it
	// above the tag switches and the list.
	var order struct {
		Now   float64 `json:"now"`
		Seek  float64 `json:"seek"`
		Tags  float64 `json:"tags"`
		Songs float64 `json:"songs"`
	}
	w.exec(`function top(id){return document.getElementById(id).getBoundingClientRect().top;}
		return {now: top('savednow'), seek: top('savedseekrow'),
			tags: top('tags'), songs: top('chunks')};`, &order)
	if !(order.Now < order.Seek && order.Seek < order.Tags && order.Tags < order.Songs) {
		t.Fatalf("saved screen order (name %.0f, position row %.0f, tags %.0f, songs %.0f) is not name-then-position-then-the-rest",
			order.Now, order.Seek, order.Tags, order.Songs)
	}
	// Nothing playing yet: the row is grayed and shows no length, the
	// same as the live row does on the direct stream.
	var idle struct {
		Class string `json:"class"`
		Dur   string `json:"dur"`
	}
	w.exec(`return {class: document.getElementById('savedseekrow').className,
		dur: document.getElementById('savedseekdur').textContent};`, &idle)
	if !strings.Contains(idle.Class, "disabled") || idle.Dur != "\u2013:\u2013\u2013" {
		t.Fatalf("the saved position row before anything plays: class %q length %q (expected grayed, with no length)", idle.Class, idle.Dur)
	}
	w.click(`#chunks .chunk button[data-action="Play"]`)
	saved := assertPlays(t, w, "savedaudio", 1.5, 20*time.Second)
	t.Logf("saved chunk: currentTime %.1fs readyState %d", saved.Time, saved.Ready)
	// The name is the name; the tag moved to its own line under it.
	var block struct {
		Title string `json:"title"`
		Meta  string `json:"meta"`
	}
	w.exec(`return {title: document.getElementById('savednow').textContent,
		meta: document.getElementById('savedmeta').textContent};`, &block)
	if strings.Contains(block.Title, "(") || block.Title == "" {
		t.Fatalf("the saved song's name = %q (expected the title alone)", block.Title)
	}
	if !strings.Contains(block.Meta, "demo") {
		t.Fatalf("the line under the saved song's name = %q (expected its tag)", block.Meta)
	}
	// The position row runs with the song and the slider rewinds it -
	// a saved song is a whole file, so unlike the live stream it seeks.
	var pos struct {
		Class string `json:"class"`
		Now   string `json:"now"`
		Dur   string `json:"dur"`
		Value string `json:"value"`
	}
	waitFor(t, 15*time.Second, "the saved position row to follow the song", func() bool {
		w.exec(`return {class: document.getElementById('savedseekrow').className,
			now: document.getElementById('savedseeknow').textContent,
			dur: document.getElementById('savedseekdur').textContent,
			value: document.getElementById('savedseek').value};`, &pos)
		return !strings.Contains(pos.Class, "disabled") &&
			pos.Now != "0:00" && pos.Dur != "0:00" && pos.Dur != "\u2013:\u2013\u2013" && pos.Value != "0"
	})
	t.Logf("saved position row: %s / %s (slider %s)", pos.Now, pos.Dur, pos.Value)
	// Rewinding is the whole point: the song is parked near its end and
	// the slider dragged back to a tenth of it. The whole drag is one
	// synchronous script so nothing moves in the middle of it - and the
	// script bails rather than acting when the duration is not known
	// yet, because a three-second song that has just rolled over spends
	// a moment reloading with duration NaN, and NaN into currentTime is
	// a TypeError, not a failed assertion. The pause on the first
	// attempt is what makes a retry find a still song.
	var seeked struct {
		OK     bool    `json:"ok"`
		Dur    float64 `json:"dur"`
		Before float64 `json:"before"`
		After  float64 `json:"after"`
	}
	waitFor(t, 15*time.Second, "a still song to rewind", func() bool {
		w.exec(`var a=document.getElementById('savedaudio'), s=document.getElementById('savedseek');
			a.pause();
			if (!isFinite(a.duration) || a.duration <= 0) return {ok: false};
			a.currentTime = a.duration * 0.8;
			var before = a.currentTime;
			s.value = "100";
			s.dispatchEvent(new Event('input', {bubbles: true}));
			s.dispatchEvent(new Event('change', {bubbles: true}));
			return {ok: true, dur: a.duration, before: before, after: a.currentTime};`, &seeked)
		return seeked.OK
	})
	if seeked.After >= seeked.Before-0.5 || math.Abs(seeked.After-seeked.Dur*0.1) > 0.3 {
		t.Fatalf("dragging the saved position row back moved a %.1fs song from %.1fs to %.1fs (expected about %.1fs)",
			seeked.Dur, seeked.Before, seeked.After, seeked.Dur*0.1)
	}
	t.Logf("saved rewind: %.1fs -> %.1fs of %.1fs", seeked.Before, seeked.After, seeked.Dur)
	// Playing again: what follows is about the row buttons answering a
	// tap, which is not a question about a stopped player.
	w.exec(`var p=document.getElementById('savedaudio').play();
		if (p && p.catch) p.catch(function () {});
		return true;`, nil)
	// The row's own button answers the tap: while that song is the one
	// playing it IS the pause, and pressing it pauses the player rather
	// than doing something of its own. Without that, a song that takes
	// a moment to start looks like a button that did nothing.
	waitFor(t, 10*time.Second, "the row button to become a pause", func() bool {
		var n int
		w.exec(`return document.querySelectorAll('#chunks .chunk button[data-action="Pause"]').length;`, &n)
		return n == 1
	})
	w.click(`#chunks .chunk button[data-action="Pause"]`)
	waitFor(t, 10*time.Second, "the row pause to pause the player", func() bool {
		var v struct {
			Paused bool   `json:"paused"`
			Action string `json:"action"`
		}
		w.exec(`var a=document.getElementById('savedaudio');
			var b=document.querySelector('#chunks .chunk .rowicon[data-action]');
			return {paused: !!(a && a.paused), action: b ? b.getAttribute('data-action') : ''};`, &v)
		return v.Paused && v.Action == "Play"
	})
	// Saved songs are banked on the device exactly like the stream's,
	// so pressing play is not a wait on the network - which on a car's
	// connection is what made this mode stutter at every song.
	waitFor(t, 30*time.Second, "the saved song banked on this device", func() bool {
		var keys []string
		w.execAsync(`var cb=arguments[arguments.length-1];
			var req=indexedDB.open("iar-radio");
			req.onerror=function(){cb([])};
			req.onsuccess=function(){
				try {
					var tx=req.result.transaction("chunks","readonly");
					tx.objectStore("chunks").getAllKeys().onsuccess=function(e){cb(e.target.result)};
				} catch (err) { cb([]); }
			};`, &keys)
		return len(keys) == 1
	})
	var bankNote string
	w.exec(`return (document.getElementById('savedbank')||{}).textContent||'';`, &bankNote)
	if !strings.Contains(bankNote, "ready on this device") {
		t.Fatalf("the saved bank's count says %q", bankNote)
	}
	w.click(`#chunks .chunk button[data-action="Play"]`)
	// The loop is one button in the transport bar, where the live page
	// keeps it, and it says it is on by wearing the ring rather than by
	// changing an icon's shade in a list row.
	var noLoopRows int
	w.exec(`return document.querySelectorAll('#chunks button[data-action="Loop"]').length;`, &noLoopRows)
	if noLoopRows != 0 {
		t.Fatalf("%d per-row loop buttons left in the list", noLoopRows)
	}
	w.click("#sloop")
	var looped struct {
		Text    string `json:"text"`
		Class   string `json:"class"`
		Pressed string `json:"pressed"`
	}
	w.exec(`var b=document.getElementById('sloop');
		return {text: document.getElementById('loopstate').textContent,
			class: b.className, pressed: b.getAttribute('aria-pressed')};`, &looped)
	if !strings.Contains(looped.Text, "Looping one song") {
		t.Fatalf("loop indicator = %q", looped.Text)
	}
	if !strings.Contains(looped.Class, "on") || looped.Pressed != "true" {
		t.Fatalf("the loop button while looping: class %q aria-pressed %q", looped.Class, looped.Pressed)
	}
	w.screenshot(shot("remote-saved.png"))
	w.click("#sloop")
	w.exec(`var b=document.getElementById('sloop');
		return {text: document.getElementById('loopstate').textContent,
			class: b.className, pressed: b.getAttribute('aria-pressed')};`, &looped)
	if strings.Contains(looped.Text, "one song") || looped.Pressed != "false" || strings.Contains(looped.Class, "on") {
		t.Fatalf("loop indicator after turning it off = %q (class %q, aria-pressed %q)", looped.Text, looped.Class, looped.Pressed)
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
	// A page that rewrites itself every couple of seconds tells the
	// phone it changed, every couple of seconds, forever - and Android
	// clipboard tools that watch for exactly that then read the
	// clipboard, which is what puts a system "copied" pill in front of
	// a listener who only pressed play. Nothing may change in the DOM
	// across polls while the radio's state is standing still.
	w.exec(`window.__mutations = 0; window.__mutlog = [];
		new MutationObserver(function (records) { window.__mutations += records.length;
			records.forEach(function (r) {
				var t = r.target;
				window.__mutlog.push(r.type + ':' + (t.id || (t.parentNode && t.parentNode.id) || t.nodeName));
			}); })
			.observe(document.body, {subtree: true, childList: true, characterData: true,
				attributes: true, attributeFilter: ["class", "aria-pressed", "aria-label", "value"]});
		return true;`, nil)
	time.Sleep(7 * time.Second) // three /state polls
	var mutations int
	w.exec(`return window.__mutations;`, &mutations)
	// The clock in the seek row is allowed to advance; a handful of
	// text updates a poll is that and nothing more.
	if mutations > 12 {
		var log []string
		w.exec(`return window.__mutlog;`, &log)
		t.Fatalf("the page mutated %d times over three idle polls; it should hold still: %v", mutations, log)
	}

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
	// Which build served this page. The machine prints the same string
	// when it starts, and the only reason to show it here is so the two
	// can be read against each other after a rebuild.
	var build string
	w.exec(`return (document.querySelector('.sheetversion')||{}).textContent||'';`, &build)
	if !strings.HasPrefix(build, "Infinite AI Radio ") ||
		strings.TrimSpace(strings.TrimPrefix(build, "Infinite AI Radio ")) == "" {
		t.Fatalf("the settings do not name the running build: %q", build)
	}
	w.click("#sheetclose")

	// The radio can be held from the phone, and a held radio says so
	// where it cannot be scrolled past - the case that matters is the
	// listener who forgot, whose device carries on playing songs it
	// already has until they run out.
	// Holding the radio is one tap on the app bar, not two taps and a
	// scroll inside the settings sheet - and the button says which
	// state the radio is in, with the switch in Settings following it,
	// because they are the same act.
	w.click("#hold")
	waitFor(t, 15*time.Second, "the standby banner to appear", func() bool {
		var v struct {
			Hidden bool   `json:"hidden"`
			Text   string `json:"text"`
			Held   bool   `json:"held"`
			Box    bool   `json:"box"`
		}
		w.exec(`var b=document.getElementById('standbybar');
			return {hidden: !!b.hidden,
				text: (document.getElementById('standbytext')||{}).textContent||"",
				held: document.getElementById('hold').classList.contains('on'),
				box: !!document.getElementById('standby').checked};`, &v)
		return !v.Hidden && strings.Contains(v.Text, "standby") && v.Held && v.Box
	})
	// Pressing play into a held radio is allowed, and the banner
	// switches to the wording for someone who is listening anyway:
	// what they get ends in silence, whether it is this device's own
	// banked songs running out or a stream carrying nothing.
	w.click("#play")
	waitFor(t, 15*time.Second, "the banner to warn a listener about the silence", func() bool {
		var text string
		w.exec(`return (document.getElementById('standbytext')||{}).textContent||"";`, &text)
		return strings.Contains(text, "silen") && !strings.Contains(text, "wake it to start again")
	})
	// Waking clears it for everyone, and the app-bar button lets go of
	// its held colour with it.
	w.click("#standbywake")
	waitFor(t, 15*time.Second, "the banner to clear on waking", func() bool {
		var v struct {
			Hidden bool `json:"hidden"`
			Held   bool `json:"held"`
			Button bool `json:"button"`
		}
		w.execAsync(`var cb = arguments[arguments.length - 1];
			fetch('/state').then(function (r) { return r.json() }).then(function (st) {
				cb({hidden: !!document.getElementById('standbybar').hidden, held: !!st.standby,
				    button: document.getElementById('hold').classList.contains('on')});
			});`, &v)
		return v.Hidden && !v.Held && !v.Button
	})
	// Leave listening off again: this device only asked for songs to
	// prove the warning, and a page that remembers wanting them would
	// start playing by itself on every later load.
	waitFor(t, 20*time.Second, "the device to stop asking for songs", func() bool {
		var v struct {
			Playing bool `json:"playing"`
			Wants   bool `json:"wants"`
		}
		w.exec(`var a = document.getElementById('liveaudio');
			return {playing: !!(a && !a.paused),
				wants: localStorage.getItem('iar.wasplaying') === 'true'};`, &v)
		if !v.Playing && !v.Wants {
			return true
		}
		w.exec(`document.getElementById('play').click(); return true;`, nil)
		return false
	})
	// Emptying the radio's own buffer is the one button that replaces
	// the preset detour - load something else, load this back - that
	// used to be the only way to start the batch ladder over. It throws
	// away everything made ahead, so it asks first. This station is
	// noise, which needs no generation at all, and the answer says so
	// rather than pretending something was done. (The music-mode
	// answer is asserted where there is a buffer to empty, in
	// TestRealBrowserBufferedNextExclusive.)
	w.click("#more")
	w.click("#bufflush")
	if text := w.alertText(); !strings.Contains(text, "Empty the radio's buffer?") {
		t.Fatalf("buffer flush confirmation = %q", text)
	}
	w.acceptAlert()
	waitFor(t, 15*time.Second, "the buffer flush ack", func() bool {
		var ack string
		w.exec(`return (document.getElementById('bufflushstatus')||{}).textContent||'';`, &ack)
		return strings.Contains(ack, "noise mode") && !strings.Contains(ack, "failed")
	})
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

	// Sessions with generated names accumulate one per change to the
	// sound, so their band has a single gesture that empties it - and
	// that gesture never takes the session that is playing.
	autoRows := func() int {
		var n int
		w.exec(`var bands = document.querySelectorAll('#sessions details.grp');
			for (var i = 0; i < bands.length; i++) {
				var head = bands[i].querySelector('summary');
				if (head && head.textContent.indexOf('Auto-saved') === 0) {
					return bands[i].querySelectorAll('.sess').length;
				}
			}
			return -1;`, &n)
		return n
	}
	// Changing the sound branches the session that was playing: it
	// keeps its name and stays in the list, and the change carries on
	// under a generated one. That is what makes going back possible,
	// and what makes these accumulate.
	var playingBefore string
	w.exec(`var e=document.querySelector('#sessions .sess.playing .ctitle'); return e ? e.textContent : '';`, &playingBefore)
	w.exec(`document.getElementById('steercard').open = true; return true;`, nil)
	w.typeInto("#text", "brown noise")
	w.click("#steer")
	waitFor(t, 20*time.Second, "the change to branch the session", func() bool {
		var playingNow string
		w.exec(`var e=document.querySelector('#sessions .sess.playing .ctitle'); return e ? e.textContent : '';`, &playingNow)
		return autoRows() >= 2 && playingNow != "" && playingNow != playingBefore
	})
	if !strings.Contains(playingBefore, "pink-noise-") {
		t.Fatalf("the session playing before the change was %q", playingBefore)
	}
	kept := strings.TrimSpace(strings.Split(playingBefore, "\u00b7")[0])
	waitFor(t, 10*time.Second, "the session before the change to still be listed", func() bool {
		var listed bool
		w.exec(fmt.Sprintf(`var want = %q;
			var rows = document.querySelectorAll('#sessions .sess .ctitle');
			for (var i = 0; i < rows.length; i++) {
				if (rows[i].textContent.indexOf(want) === 0) return true;
			}
			return false;`, kept), &listed)
		return listed
	})

	w.click("#autowipe")
	if text := w.alertText(); !strings.Contains(text, "automatic sessions") {
		t.Fatalf("bulk delete confirmation = %q", text)
	}
	w.acceptAlert()
	waitFor(t, 15*time.Second, "the automatic sessions to be cleared", func() bool {
		w.exec(`return document.getElementById('sessstatus').textContent;`, &ackText)
		return strings.Contains(ackText, "automatic session")
	})
	waitFor(t, 15*time.Second, "only the playing session left in the band", func() bool { return autoRows() == 1 })

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

// fakeCaptions read like the per-song descriptions a real planner
// returns, and the player derives a display name from the first of
// them, so a sandbox with no helper model still shows song-like names.
var fakeCaptions = []string{
	"Harbour lights", "Steel in the water", "Neon rain", "Ash and anchor",
	"Long way down", "Paper boats", "Hold the line", "Wire and glass",
}

// fakeCaption picks one deterministically from a task id, so a shoot
// and a test see stable names and neighbours differ.
func fakeCaption(id string) string {
	sum := 0
	for _, r := range id {
		sum = sum*31 + int(r)
	}
	if sum < 0 {
		sum = -sum
	}
	return fakeCaptions[sum%len(fakeCaptions)]
}

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
			// A real engine echoes the description it actually used,
			// which differs per song and reads like a song rather than
			// a tag list; the player names a song from it when no
			// helper wrote one, and the pictures in the README are
			// taken from a machine with no helper. Rotate so no two
			// songs in a row share a name.
			row := map[string]any{
				"status": 1, "prompt": fakeCaption(id) + ", " + task.prompt,
				"seed_value": "7", "lyrics": lyrics,
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
	// The words carry the name of the song they belong to, both on the
	// page and in what the Copy button hands over - a sheet of lyrics
	// with no name on it is a puzzle once it is in a notes app. Read
	// the two together so a track change between reads cannot make
	// them look mismatched.
	waitFor(t, 15*time.Second, "the lyrics to be headed by the song's name", func() bool {
		var v struct {
			Title string `json:"title"`
			Now   string `json:"now"`
		}
		w.exec(`return {title: (document.getElementById('lyrtitle')||{}).textContent||"",
		                now: (document.getElementById('now')||{}).textContent||""};`, &v)
		return v.Title != "" && v.Title == v.Now
	})
	w.exec(`window.__copied = null;
		navigator.clipboard = navigator.clipboard || {};
		navigator.clipboard.writeText = function (t) { window.__copied = t; return Promise.resolve(); };
		return true;`, nil)
	w.click("#lyrcopy")
	waitFor(t, 10*time.Second, "the copied sheet to carry the title", func() bool {
		var v struct {
			Copied string `json:"copied"`
			Title  string `json:"title"`
			Words  string `json:"words"`
		}
		w.exec(`return {copied: window.__copied || "",
		                title: (document.getElementById('lyrtitle')||{}).textContent||"",
		                words: (document.getElementById('lyrics')||{}).textContent||""};`, &v)
		if v.Copied == "" {
			return false
		}
		if !strings.HasPrefix(v.Copied, v.Title) {
			t.Fatalf("copied sheet does not start with the song's name %q: %q", v.Title, v.Copied[:min(80, len(v.Copied))])
		}
		return strings.Contains(v.Copied, "[Chorus]")
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
	// buffered mode must save the DEVICE's playing track and mark it
	// "Saved:" on the car metadata for as long as that song plays.
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
	// The unreachable-radio marker can legitimately sit in front of the
	// saved one; what is under test here is the saved one.
	savedMarked := func(title string) bool {
		return strings.HasPrefix(strings.TrimPrefix(title, "[X] "), "Saved: ")
	}
	var mark string
	w.exec(`return navigator.mediaSession.metadata ? navigator.mediaSession.metadata.title : '';`, &mark)
	if !savedMarked(mark) {
		t.Fatalf("car metadata after the save = %q", mark)
	}
	// The page's headline carries the same marker, and for the same
	// reason: the answer has to be there when the driver looks up, not
	// only at the instant the save landed.
	var headline string
	w.exec(`return (document.getElementById('now')||{}).textContent||'';`, &headline)
	if !strings.HasPrefix(headline, "Saved: ") {
		t.Fatalf("the headline does not say the song is saved: %q", headline)
	}
	// And it stays put while that song plays. This used to be a
	// two-second flash, which meant learning whether the song was safe
	// required watching the screen at exactly the right moment - on a
	// slow signal, where the save takes longest, that moment was
	// routinely missed.
	var srcAtSave string
	w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
		for (var i=0;i<2;i++) { if (a[i] && !a[i].paused) return a[i].src; }
		return '';`, &srcAtSave)
	held := time.Now().Add(5 * time.Second)
	for time.Now().Before(held) {
		var v struct {
			Title string `json:"title"`
			Src   string `json:"src"`
		}
		w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
			var src=''; for (var i=0;i<2;i++) { if (a[i] && !a[i].paused) src=a[i].src; }
			var m=('mediaSession' in navigator) && navigator.mediaSession.metadata;
			return {title: m ? m.title : '', src: src};`, &v)
		if v.Src != srcAtSave {
			break // the song moved on; the marker belongs to the next one
		}
		if !savedMarked(v.Title) {
			t.Fatalf("the saved marker was taken back while the song was still playing: %q", v.Title)
		}
		time.Sleep(250 * time.Millisecond)
	}
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
			var req=indexedDB.open("iar-radio");
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

	// Flush means flush, even with the radio switched off. The songs
	// are gone from the device and nothing plays, rather than being
	// handed straight back out of the browser's own cache - which is
	// what made a flushed bank refill with the same songs and restart
	// the song that was playing.
	w.click("#jumplive")
	emptied := func() (string, bool) {
		var st struct {
			Count   string `json:"count"`
			Playing bool   `json:"playing"`
		}
		w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
			var on=false; a.forEach(function (x) { if (x && !x.paused) on = true; });
			return {count: (document.getElementById('devcount')||{}).textContent||"", playing: on};`, &st)
		return st.Count, strings.HasPrefix(st.Count, "0 song") && !st.Playing
	}
	waitFor(t, 20*time.Second, "the device to empty", func() bool {
		_, ok := emptied()
		return ok
	})
	// Still empty a few polls later: a refill out of the browser's own
	// cache would land within one.
	time.Sleep(6 * time.Second)
	if count, ok := emptied(); !ok {
		t.Fatalf("a flushed device refilled itself while the radio was off: %q", count)
	}

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
	// (generation takes ~2-3s per track), and long enough that a device
	// stays on one song across the checks below - two of them are about
	// which song a control acts on, which is not a question if the song
	// changes underneath.
	fe.setTrackSeconds(60)
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

	// The page must follow the song THIS device is playing. Skipping the
	// MACHINE moves it on without touching the phone's bank, which is
	// exactly the state that renamed the song under a listener who was
	// only looping one: two different songs, one headline. The lock
	// screen has always followed the device, so they must agree - and
	// the words on screen must be this device's song's, not the ones
	// the speakers are singing. (Songs here share a name, because the
	// sandbox has no helper model to write per-song ones; the ids and
	// the lyrics are what tell them apart.)
	w.execAsync(`var cb = arguments[arguments.length - 1];
		fetch('/next', {method: 'POST', headers: {'X-IAR-Remote': '1'}}).then(function () { cb(true) });`, nil)
	waitFor(t, 25*time.Second, "the page to follow this device's song, not the machine's", func() bool {
		var v struct {
			Now    string `json:"now"`
			Card   string `json:"card"`
			Server string `json:"server"`
			SrvLyr string `json:"srvlyr"`
			Lyrics string `json:"lyrics"`
		}
		w.execAsync(`var cb = arguments[arguments.length - 1];
			fetch('/state').then(function (r) { return r.json() }).then(function (st) {
				var m = ('mediaSession' in navigator) && navigator.mediaSession.metadata;
				var words = ((st.track && st.track.lyrics) || "").split("\n");
				cb({now: (document.getElementById('now')||{}).textContent||"",
				    card: m ? m.title : "",
				    server: (st.track && (st.track.title || st.track.prompt)) || "",
				    srvlyr: words.length > 1 ? words[1] : "",
				    lyrics: (document.getElementById('lyrics')||{}).textContent||""});
			});`, &v)
		if v.Card == "" || v.Server == "" || v.Card == v.Server {
			return false // not yet on different songs
		}
		if v.Now != v.Card {
			t.Fatalf("the headline named the machine's song, not this device's: headline %q, device %q, machine %q",
				v.Now, v.Card, v.Server)
		}
		// The words follow the device too - they are what the listener
		// is actually hearing.
		if v.SrvLyr == "" || strings.Contains(v.Lyrics, v.SrvLyr) {
			t.Fatalf("the words on screen are the machine's, not this device's: %q in %q", v.SrvLyr, v.Lyrics)
		}
		return true
	})

	// The pencil beside the headline renames THAT song - the one this
	// device is playing - and leaves the machine's alone. Renaming from
	// the saved list would mean leaving the live page, which stops the
	// radio; this is the whole point of the control.
	var machineBefore string
	w.execAsync(`var cb = arguments[arguments.length - 1];
		fetch('/state').then(function (r) { return r.json() }).then(function (st) {
			cb((st.track && st.track.title) || "");
		});`, &machineBefore)
	w.click("#rename")
	w.typeIntoAlert("Harbour Lights")
	w.acceptAlert()
	waitFor(t, 25*time.Second, "this device's song to take the new name", func() bool {
		var v struct {
			Now    string `json:"now"`
			Card   string `json:"card"`
			Server string `json:"server"`
		}
		w.execAsync(`var cb = arguments[arguments.length - 1];
			fetch('/state').then(function (r) { return r.json() }).then(function (st) {
				var m = ('mediaSession' in navigator) && navigator.mediaSession.metadata;
				cb({now: (document.getElementById('now')||{}).textContent||"",
				    card: m ? m.title : "",
				    server: (st.track && st.track.title) || ""});
			});`, &v)
		if v.Server == "Harbour Lights" {
			t.Fatalf("renaming this device's song renamed the machine's instead: %q", v.Server)
		}
		return v.Now == "Harbour Lights" && v.Card == "Harbour Lights"
	})
	if machineBefore == "Harbour Lights" {
		t.Fatal("the machine was already playing a song by that name")
	}

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

	// Emptying the radio's buffer keeps the station and its steering
	// and starts the batch ladder over, so a listener who does not like
	// what is queued gets fresh songs without the detour through
	// another preset that used to be the only way.
	w.click("#more")
	w.click("#bufflush")
	if text := w.alertText(); !strings.Contains(text, "Empty the radio's buffer?") {
		t.Fatalf("buffer flush confirmation = %q", text)
	}
	w.acceptAlert()
	waitFor(t, 20*time.Second, "the radio to say it is generating again", func() bool {
		var ack string
		w.exec(`return (document.getElementById('bufflushstatus')||{}).textContent||'';`, &ack)
		return strings.Contains(ack, "generating again from the top")
	})
	w.click("#sheetclose")
}

// TestRealBrowserSkippedSongNeverComesBack holds the promise Next makes
// on a buffered device: the song is gone. The device used to fall back
// on replaying what it already had when nothing new had arrived, and
// with one song banked that meant a press of Next started the song the
// listener had just rejected over from the top - most reliably right
// after a new preset, when the radio has nothing fresh yet and pressing
// Next is exactly what a listener does. Now the song leaves the bank
// for good, the trouble beeps sound, and the device waits.
func TestRealBrowserSkippedSongNeverComesBack(t *testing.T) {
	need(t, "geckodriver", "firefox", "pactl", "ffmpeg", "go")
	sinkName, _ := nullSink(t)
	sb, fe := startMusicSandboxCfg(t, `"buffer_tracks":1,"library_max_mb":0`)
	// Long songs, so nothing ends of its own accord while the skips
	// below are being counted.
	fe.setTrackSeconds(60)
	driver := startGeckodriver(t, sinkName)
	w := newWebDriver(t, driver)
	loginAdmin(t, w, sb.base)

	w.exec(`document.getElementById('buffered').click(); return true;`, nil)
	w.click("#play")
	waitFor(t, 60*time.Second, "playing with a song banked ahead", func() bool {
		var pill string
		w.exec(`return document.getElementById('streamstate').textContent;`, &pill)
		return strings.Contains(pill, "ahead")
	})

	// Take the radio away: what is on the device is now all there is,
	// which is the situation the old fallback existed for.
	sb.killPlayer()

	// A car screen shows the song and nothing else, so a device playing
	// happily out of its own bank looks exactly like one the radio is
	// still feeding - until the bank runs out. The marker in front of
	// the name is the difference, and it appears while the music is
	// still playing rather than after it stops.
	waitFor(t, 20*time.Second, "the car title to mark the radio unreachable", func() bool {
		var v struct {
			Title   string `json:"title"`
			Playing bool   `json:"playing"`
		}
		w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
			var on=false; for (var i=0;i<2;i++) { if (a[i] && !a[i].paused) on=true; }
			var m=('mediaSession' in navigator) && navigator.mediaSession.metadata;
			return {title: m ? m.title : '', playing: on};`, &v)
		if strings.HasPrefix(v.Title, "[X] ") && !v.Playing {
			t.Fatalf("the marker only arrived after the music stopped: %q", v.Title)
		}
		return strings.HasPrefix(v.Title, "[X] ")
	})

	banked := func() []string {
		var keys []string
		w.execAsync(`var cb=arguments[arguments.length-1];
			var req=indexedDB.open("iar-radio");
			req.onerror=function(){cb([])};
			req.onsuccess=function(){
				try {
					var tx=req.result.transaction("tracks","readonly");
					tx.objectStore("tracks").getAllKeys().onsuccess=function(e){cb(e.target.result)};
				} catch (err) { cb([]); }
			};`, &keys)
		return keys
	}
	playing := func() bool {
		var on bool
		w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
			for (var i=0;i<2;i++) { if (a[i] && !a[i].paused) return true; }
			return false;`, &on)
		return on
	}
	if len(banked()) == 0 {
		t.Fatal("nothing was banked on the device to skip through")
	}

	// Skip until the device runs out. Every press must consume a song
	// rather than cycle: a device that replayed would never stop.
	deadline := time.Now().Add(90 * time.Second)
	presses := 0
	for time.Now().Before(deadline) && playing() {
		w.click("#next")
		presses++
		if presses > 30 {
			t.Fatal("the device kept finding something to play after 30 skips; it is cycling")
		}
		time.Sleep(900 * time.Millisecond) // the manual-skip debounce
	}
	if playing() {
		t.Fatal("the device never ran out of songs to skip to")
	}

	// Everything skipped is off the device and on the list of what
	// never comes back, and the page says it is waiting rather than
	// claiming to play.
	waitFor(t, 15*time.Second, "the bank to be empty and the page to say it is waiting", func() bool {
		var pill string
		w.exec(`return document.getElementById('streamstate').textContent;`, &pill)
		return len(banked()) == 0 && strings.Contains(pill, "waiting")
	})
	var tossed string
	w.exec(`return localStorage.getItem('iar.tossed') || '';`, &tossed)
	if tossed == "" || tossed == "{}" {
		t.Fatalf("nothing was recorded as skipped: %q", tossed)
	}

	// And it stays silent. The regression this guards is the opposite:
	// the skipped song starting again a moment later.
	hold := time.Now().Add(8 * time.Second)
	for time.Now().Before(hold) {
		if playing() {
			t.Fatal("a skipped song started playing again")
		}
		time.Sleep(400 * time.Millisecond)
	}
	if n := len(banked()); n != 0 {
		t.Fatalf("%d skipped song(s) are still stored on the device", n)
	}

	// And the marker is not a one-way door: the radio coming back takes
	// it off again.
	sb.startPlayer(t)
	waitFor(t, 30*time.Second, "the car title to drop the marker", func() bool {
		var title string
		w.exec(`var m=('mediaSession' in navigator) && navigator.mediaSession.metadata;
			return m ? m.title : '';`, &title)
		return title != "" && !strings.HasPrefix(title, "[X] ")
	})
}

// TestRealBrowserSilentSongIsNotReportedAsPlaying reproduces the state a
// listener could only escape by pressing Next: the page reporting
// "playing (buffered)" with no sound and a dead timeline. A song is
// named the instant it is picked, seconds before play() settles, and
// the status repaint on the next poll read that name rather than the
// element - so a song that never started was painted straight over its
// own error, and the device sat in silence claiming to work.
func TestRealBrowserSilentSongIsNotReportedAsPlaying(t *testing.T) {
	need(t, "geckodriver", "firefox", "pactl", "ffmpeg", "go")
	sinkName, _ := nullSink(t)
	sb, fe := startMusicSandboxCfg(t, `"buffer_tracks":2,"library_max_mb":0`)
	fe.setTrackSeconds(60)
	driver := startGeckodriver(t, sinkName)
	w := newWebDriver(t, driver)
	loginAdmin(t, w, sb.base)

	// Every attempt to start a song fails the way a truncated download
	// does: the song is picked, so the page knows its name, but no
	// sound ever comes of it. Armed before play, so the very first song
	// is the one that refuses.
	w.exec(`HTMLMediaElement.prototype.play = function () {
			return Promise.reject(new DOMException("no decoder", "NotSupportedError"));
		};
		document.getElementById('buffered').click();
		return true;`, nil)
	w.click("#play")

	honest := false
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		var v struct {
			Pill    string `json:"pill"`
			Playing bool   `json:"playing"`
		}
		w.exec(`var a=[document.getElementById('bufaudio0'),document.getElementById('bufaudio1')];
			var on=false; for (var i=0;i<2;i++) { if (a[i] && !a[i].paused) on=true; }
			return {pill: (document.getElementById('streamstate')||{}).textContent||"",
				playing: on};`, &v)
		if !v.Playing && strings.Contains(v.Pill, "playing (buffered)") {
			t.Fatalf("the page reports %q with no sound coming out of it", v.Pill)
		}
		for _, honestly := range []string{"would not start", "will not play", "waiting for the radio"} {
			if strings.Contains(v.Pill, honestly) {
				honest = true
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !honest {
		t.Fatal("the page never said the songs would not start")
	}
	_ = sb
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

// seedChunk writes one more saved song into a sandbox's library, so a
// test can be about which of two songs a control acts on.
func seedChunk(t *testing.T, sb *sandbox, tag, file, title string, seconds int) {
	t.Helper()
	samples := make([]int16, seconds*audio.SampleRate*audio.Channels)
	for i := 0; i < len(samples); i += 2 {
		v := int16(6000 * math.Sin(2*math.Pi*440*float64(i/2)/audio.SampleRate))
		samples[i], samples[i+1] = v, v
	}
	path := filepath.Join(sb.dir, "data", "snippets", tag, file)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := export.EncodeMP3(context.Background(), samples, path,
		export.MP3Options{Title: title, Album: tag}); err != nil {
		t.Fatal(err)
	}
}

// TestRealBrowserSavedLoopFollowsTheSongPlaying guards the one thing the
// loop's move into the transport bar has to get right: the lamp says
// whether THIS song repeats. The element's loop property survives a
// change of source, so a loop left set while another song is picked
// would repeat the new song forever behind a lamp still naming the old
// one - and, because a looping element never fires "ended", the
// playlist could never advance again.
func TestRealBrowserSavedLoopFollowsTheSongPlaying(t *testing.T) {
	need(t, "geckodriver", "firefox", "pactl", "ffmpeg", "go")
	sinkName, _ := nullSink(t)
	sb := prepareSandbox(t, "noise", "")
	// Long enough that it is still playing while the loop is turned on
	// and the other song picked: this test is about which song the loop
	// holds, which is not a question if the song ends underneath it.
	seedChunk(t, sb, "demo", "20260821-121000-long-tone.mp3", "long tone", 25)
	finishSandbox(t, sb)
	driver := startGeckodriver(t, sinkName)
	w := newWebDriver(t, driver)
	loginAdmin(t, w, sb.base)

	w.click("#mode-saved")
	waitFor(t, 10*time.Second, "both saved songs listed", func() bool {
		var n int
		w.exec(`return document.querySelectorAll('#chunks .chunk').length;`, &n)
		return n == 2
	})
	w.click(`#chunks button[aria-label="Play long tone"]`)
	assertPlays(t, w, "savedaudio", 0.5, 20*time.Second)
	w.click("#sloop")
	var on struct {
		Loop  bool   `json:"loop"`
		Class string `json:"class"`
		State string `json:"state"`
	}
	waitFor(t, 10*time.Second, "the loop to take hold of the long tone", func() bool {
		w.exec(`var b=document.getElementById('sloop');
			return {loop: document.getElementById('savedaudio').loop, class: b.className,
				state: document.getElementById('loopstate').textContent};`, &on)
		return on.Loop && strings.Contains(on.Class, "on") && strings.Contains(on.State, "long tone")
	})

	// Picking the other song is "play this one", the same as Skip: the
	// loop lets go rather than transferring itself.
	w.click(`#chunks button[aria-label="Play demo tone"]`)
	var off struct {
		Loop    bool   `json:"loop"`
		Class   string `json:"class"`
		Pressed string `json:"pressed"`
		State   string `json:"state"`
		Now     string `json:"now"`
	}
	waitFor(t, 10*time.Second, "the loop to let go with the song it held", func() bool {
		w.exec(`var b=document.getElementById('sloop');
			return {loop: document.getElementById('savedaudio').loop, class: b.className,
				pressed: b.getAttribute('aria-pressed'),
				state: document.getElementById('loopstate').textContent,
				now: document.getElementById('savednow').textContent};`, &off)
		return !off.Loop && !strings.Contains(off.Class, "on") && off.Pressed == "false"
	})
	if strings.Contains(off.State, "long tone") {
		t.Fatalf("the loop readout still names the song that stopped playing: %q", off.State)
	}
	if !strings.Contains(off.Now, "demo tone") {
		t.Fatalf("the name at the top of the screen = %q (expected the song just picked)", off.Now)
	}
}
