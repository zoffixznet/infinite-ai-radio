package remote

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// phoneCar runs script against what the phone tells the lock screen
// and the car - the media session's state, its song, the song's
// length and the place in it, and which buttons the page asks for -
// lifted from app.js and run in node with a navigator whose
// mediaSession writes every call down: the states set on it in
// states, the titles in titles, every setPositionState in positions
// (the length, rate and place spelled out, or "cleared" for a call
// with no arguments) with every attempt counted in positionCalls,
// and the actions with a handler in handlers, listed by handled().
// The registration of the actions, which app.js does at its top
// level, is lifted along with the functions. The live player is one
// fake element, el, on a pf the script fills with playing(); the
// saved player is savedAudio; and what the buttons are routed to
// lands in calls.
//
// The whole of it runs inside one function, so the fake navigator is
// a binding of that function's own: node 21 and later put a
// navigator of their own on the global object, a getter with no
// setter, and a top-level var of the same name in a -e script is
// silently dropped against it, leaving the lifted functions looking
// at node's navigator, which has no mediaSession, and recording
// nothing. Anything thrown lands in out.errors, which fails the test;
// the only line printed is the JSON.
func phoneCar(t *testing.T, script string, out any) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	raw, err := os.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	var fns []string
	for _, name := range []string{"applyMediaMetadata", "mediaPlaybackState", "syncMediaLiveness", "syncMediaPosition", "pfSeekTo",
		"msAction", "seekOffset", "seekSaved", "msHandler", "syncPrevAction"} {
		fns = append(fns, jsFunction(t, src, name))
	}
	register := regexp.MustCompile(`\[("[a-z]+", )*"[a-z]+"\]\.forEach\(function \(a\) \{\n\s*msHandler\(a, function \(d\) \{ msAction\(a, d\); \}\);\n\s*\}\);`).FindString(src)
	if register == "" {
		t.Fatal("app.js does not register its media-session actions where the harness expects them")
	}
	harness := `
var out = { errors: [], done: false };
function finish() { out.done = true; console.log(JSON.stringify(out)); process.exit(0); }
process.on("uncaughtException", function (e) { out.errors.push(String(e && e.stack || e)); finish(); });
setTimeout(function () { out.errors.push("the scenario never finished"); finish(); }, 5000);
(function () {
var mode = "live", carSave = true, lastNow = "", msArtist = "", lastMetaSig = "", mediaLive = null;
var repeatOne = null, calls = [];
var el = { duration: NaN, currentTime: 0, addEventListener: function () {}, removeEventListener: function () {} };
var pf = { active: false, playingId: null, els: [el, null], cur: 0 };
var savedAudio = { src: "", duration: NaN, currentTime: 0, pause: function () { calls.push("saved pause"); } };
function resumeBuffered() { calls.push("resume"); }
function savedPlay() { calls.push("saved play"); }
function pauseBuffered() { calls.push("pause"); }
function stopListening() { calls.push("stop"); }
function pfSkip() { calls.push("skip"); }
function updateLoopState() {}
function step(dir) { calls.push("step " + dir); }
function carSaveAction() { calls.push("save"); }
function offlineMark() { return ""; }
function saveMark() { return ""; }
function MediaMetadata(m) { this.title = m.title; this.artist = m.artist; }
var states = [], titles = [], positions = [], handlers = {}, positionCalls = 0, positionThrows = false;
var ms = {
  setActionHandler: function (a, fn) { if (fn) handlers[a] = fn; else delete handlers[a]; },
  setPositionState: function (s) {
    positionCalls++;
    if (positionThrows) throw new TypeError("duration is not a finite floating-point value");
    positions.push(arguments.length ? "duration " + s.duration + " rate " + s.playbackRate + " position " + s.position : "cleared");
  }
};
Object.defineProperty(ms, "playbackState", { set: function (s) { states.push(s); } });
Object.defineProperty(ms, "metadata", { set: function (m) { titles.push(m.title); } });
var navigator = { mediaSession: ms };
function handled() { return Object.keys(handlers).sort(); }
// playing is a live song on the device, of that length, at its top.
function playing(id, dur) { pf.active = true; pf.playingId = id; el.duration = dur; el.currentTime = 0; }
` + strings.Join(fns, "\n") + `
` + register + `
syncPrevAction();
` + script + `
})();
`
	cmd := exec.Command(node, "-e", harness)
	outRaw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, outRaw)
	}
	var ran struct {
		Errors []string `json:"errors"`
		Done   bool     `json:"done"`
	}
	if err := json.Unmarshal(outRaw, &ran); err != nil {
		t.Fatalf("harness output %q: %v", outRaw, err)
	}
	if len(ran.Errors) > 0 {
		t.Fatalf("the scenario threw: %s", strings.Join(ran.Errors, "\n"))
	}
	if !ran.Done {
		t.Fatalf("the scenario never finished; seen so far: %s", outRaw)
	}
	if err := json.Unmarshal(outRaw, out); err != nil {
		t.Fatalf("harness output %q: %v", outRaw, err)
	}
}

func equalFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The owner's complaint, the second time round: the car had got
// previous and next back by the radio declaring itself endless, and
// the declaration took the car's progress bar with it. The car draws
// the seek pair in that slot whatever the page declares, so the radio
// now declares the song's real length and place - when the song
// starts, is paused, resumed or moved, and when its length lands,
// never on a poll - and the bar is back. Nothing is declared while
// the length is unknown, the place is kept within the song, the saved
// player declares nothing of its own and clears the radio's
// declaration when it takes the session, and the handlers asked for
// are the transport, previous and next, and the three seeks.
func TestPhoneCarGetsItsBarBack(t *testing.T) {
	var got struct {
		BeforeLength []string  `json:"beforeLength"`
		AfterStart   []string  `json:"afterStart"`
		AfterPolls   []string  `json:"afterPolls"`
		AfterPause   []string  `json:"afterPause"`
		SeekTimes    []float64 `json:"seekTimes"`
		AfterSeek    []string  `json:"afterSeek"`
		AfterSaved   []string  `json:"afterSaved"`
		AfterBack    []string  `json:"afterBack"`
		Handlers     []string  `json:"handlers"`
		States       []string  `json:"states"`
		Titles       []string  `json:"titles"`
	}
	phoneCar(t, `
// A live song starts before its length is known.
playing("a", NaN);
mediaPlaybackState("playing");
syncMediaPosition();
out.beforeLength = positions.slice();
// The length lands.
el.duration = 180;
syncMediaPosition();
out.afterStart = positions.slice();
// Polls go by while the song plays on: the bar follows it by itself.
lastNow = "Song A"; applyMediaMetadata(); el.currentTime = 20; applyMediaMetadata();
out.afterPolls = positions.slice();
// A pause and a resume.
el.currentTime = 42;
mediaPlaybackState("paused"); mediaPlaybackState("playing");
out.afterPause = positions.slice();
// The bar is dragged: to a place in the song, past its end, and by a
// client that named no place.
handlers.seekto({ seekTime: 60 });
out.seekTimes = [el.currentTime];
handlers.seekto({ seekTime: 500 });
out.seekTimes.push(el.currentTime);
handlers.seekto({});
handlers.seekto();
out.seekTimes.push(el.currentTime);
out.afterSeek = positions.slice();
// The next song is named, and then the saved player takes over: the
// radio stops, a saved song plays.
lastNow = "Song B"; applyMediaMetadata();
mode = "saved"; pf.active = false; pf.playingId = null;
mediaPlaybackState("none");
mediaPlaybackState("playing");
lastNow = "Song C"; applyMediaMetadata();
out.afterSaved = positions.slice();
// Back to the radio, on a new song.
mode = "live";
playing("d", 200);
mediaPlaybackState("playing");
mediaPlaybackState("paused");
applyMediaMetadata();
out.afterBack = positions.slice();
out.handlers = handled();
out.states = states;
out.titles = titles;
finish();
`, &got)
	if len(got.BeforeLength) != 0 {
		t.Errorf("a song whose length is not known yet declared %q; want nothing", got.BeforeLength)
	}
	start := "duration 180 rate 1 position 0"
	if strings.Join(got.AfterStart, ",") != start {
		t.Errorf("a live song's length landing declared %q; want %q", got.AfterStart, start)
	}
	if strings.Join(got.AfterPolls, ",") != start {
		t.Errorf("polls in live mode declared %q; want nothing past the start's %q", got.AfterPolls, start)
	}
	paused := start + ",duration 180 rate 1 position 42,duration 180 rate 1 position 42"
	if strings.Join(got.AfterPause, ",") != paused {
		t.Errorf("a pause and a resume declared %q; want %q", got.AfterPause, paused)
	}
	if want := []float64{60, 179, 179}; !equalFloats(got.SeekTimes, want) {
		t.Errorf("the bar dragged moved the song to %v; want %v (a place past the end kept inside the song, no place at all ignored)", got.SeekTimes, want)
	}
	seeks := paused + ",duration 180 rate 1 position 60,duration 180 rate 1 position 179"
	if strings.Join(got.AfterSeek, ",") != seeks {
		t.Errorf("the bar dragged declared %q; want %q", got.AfterSeek, seeks)
	}
	if want := seeks + ",cleared"; strings.Join(got.AfterSaved, ",") != want {
		t.Errorf("the saved player declared %q; want %q", got.AfterSaved, want)
	}
	if want := seeks + ",cleared,duration 200 rate 1 position 0,duration 200 rate 1 position 0"; strings.Join(got.AfterBack, ",") != want {
		t.Errorf("back in live mode declared %q; want %q", got.AfterBack, want)
	}
	if want := "nexttrack,pause,play,previoustrack,seekbackward,seekforward,seekto,stop"; strings.Join(got.Handlers, ",") != want {
		t.Errorf("the page handles %q; want exactly %q", got.Handlers, want)
	}
	if want := "playing,paused,playing,none,playing,playing,paused"; strings.Join(got.States, ",") != want {
		t.Errorf("the lock screen was told %q; want %q", got.States, want)
	}
	if want := "Song A,Song B,Song C"; strings.Join(got.Titles, ",") != want {
		t.Errorf("the lock screen was shown %q; want %q", got.Titles, want)
	}
	// The length landing and a move the page did not make itself - a
	// looping song's rewind - reach the bar from the element's own
	// events, which the harness has no element to fire.
	raw, err := os.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{`"durationchange"`, `"seeked"`} {
		if !strings.Contains(string(raw), `el.addEventListener(`+ev+`, function () { if (pf.els[pf.cur] === el) syncMediaPosition(); });`) {
			t.Errorf("app.js does not tell the bar the place on the element's %s", ev)
		}
	}
}

// Android Auto draws fast-forward and rewind where previous and next
// were, and forwards them to the page alone. On the radio they do
// what next and previous do: fast-forward skips, rewind saves while
// the car-save setting is on and nothing with it off - the car draws
// the button either way, so its handler stays - and the saved player
// seeks by them, ten seconds unless the system names a distance, kept
// within the song.
func TestPhoneCarSeekPairSavesAndSkips(t *testing.T) {
	var got struct {
		Live           []string  `json:"live"`
		Idle           []string  `json:"idle"`
		NoSave         []string  `json:"noSave"`
		NoSaveHandlers []string  `json:"noSaveHandlers"`
		SavedTimes     []float64 `json:"savedTimes"`
		Saved          []string  `json:"saved"`
	}
	phoneCar(t, `
playing("a", 180);
handlers.seekforward(); handlers.nexttrack();
handlers.seekbackward(); handlers.previoustrack();
handlers.play(); handlers.pause(); handlers.stop();
out.live = calls.slice(); calls = [];
// Nothing playing on the device: a skip has nothing to skip.
pf.active = false;
handlers.seekforward(); handlers.nexttrack();
out.idle = calls.slice(); calls = [];
// The car-save setting off.
pf.active = true; carSave = false; syncPrevAction();
handlers.seekbackward();
out.noSave = calls.slice(); calls = [];
out.noSaveHandlers = handled();
carSave = true; syncPrevAction();
// The saved player.
mode = "saved"; pf.active = false; pf.playingId = null;
savedAudio.src = "blob:c"; savedAudio.duration = 200; savedAudio.currentTime = 50;
handlers.seekforward(); out.savedTimes = [savedAudio.currentTime];
handlers.seekbackward({ seekOffset: 30 }); out.savedTimes.push(savedAudio.currentTime);
handlers.seekforward({ seekOffset: 500 }); out.savedTimes.push(savedAudio.currentTime);
handlers.seekbackward({ seekOffset: 500 }); out.savedTimes.push(savedAudio.currentTime);
handlers.seekto({ seekTime: 120 }); out.savedTimes.push(savedAudio.currentTime);
handlers.seekto({ seekTime: -5 }); out.savedTimes.push(savedAudio.currentTime);
handlers.nexttrack(); handlers.previoustrack(); handlers.play(); handlers.pause();
out.saved = calls.slice();
finish();
`, &got)
	if want := "skip,skip,save,save,resume,pause,stop"; strings.Join(got.Live, ",") != want {
		t.Errorf("the car's buttons in live mode did %q; want %q", got.Live, want)
	}
	if len(got.Idle) != 0 {
		t.Errorf("with nothing playing the car's skip did %q; want nothing", got.Idle)
	}
	if len(got.NoSave) != 0 {
		t.Errorf("with car-save off the car's rewind did %q; want nothing", got.NoSave)
	}
	if want := "nexttrack,pause,play,seekbackward,seekforward,seekto,stop"; strings.Join(got.NoSaveHandlers, ",") != want {
		t.Errorf("with car-save off the page handles %q; want %q (no previous, the rewind kept)", got.NoSaveHandlers, want)
	}
	if want := []float64{60, 30, 200, 0, 120, 0}; !equalFloats(got.SavedTimes, want) {
		t.Errorf("the saved player was moved to %v; want %v", got.SavedTimes, want)
	}
	if want := "step 1,step -1,saved play,saved pause"; strings.Join(got.Saved, ",") != want {
		t.Errorf("the car's buttons in saved mode did %q; want %q", got.Saved, want)
	}
}

// A browser that refuses the declaration throws from it, and an older
// one has no declaration to make. Either way the page carries on
// telling the lock screen its state and its song, and a refusing
// browser is asked at the moments the place is told - a start, a
// pause, a resume, the change of mode - and not on a poll.
func TestPhoneKeepsTheLockScreenWhenTheBrowserRefusesThePosition(t *testing.T) {
	var got struct {
		States        []string `json:"states"`
		Titles        []string `json:"titles"`
		Positions     []string `json:"positions"`
		PositionCalls int      `json:"positionCalls"`
		OlderStates   []string `json:"olderStates"`
	}
	phoneCar(t, `
positionThrows = true;
playing("a", 180);
mediaPlaybackState("playing");
lastNow = "Song A"; applyMediaMetadata(); applyMediaMetadata();
el.currentTime = 42;
mediaPlaybackState("paused"); mediaPlaybackState("playing");
mode = "saved"; pf.active = false; pf.playingId = null; mediaPlaybackState("none");
out.states = states.slice();
out.titles = titles.slice();
out.positions = positions.slice();
out.positionCalls = positionCalls;
// An older browser, with no setPositionState at all.
states = []; mode = "live"; mediaLive = null; playing("b", 100);
delete ms.setPositionState;
mediaPlaybackState("playing"); applyMediaMetadata();
out.olderStates = states;
finish();
`, &got)
	if want := "playing,paused,playing,none"; strings.Join(got.States, ",") != want {
		t.Errorf("with the declaration refused, the lock screen was told %q; want %q", got.States, want)
	}
	if want := "Song A"; strings.Join(got.Titles, ",") != want {
		t.Errorf("with the declaration refused, the lock screen was shown %q; want %q", got.Titles, want)
	}
	if len(got.Positions) != 0 {
		t.Errorf("a refused declaration was recorded as made: %q", got.Positions)
	}
	if got.PositionCalls != 4 {
		t.Errorf("a refusing browser was asked %d times across a start, a pause, a resume and a change of mode; want 4", got.PositionCalls)
	}
	if want := "playing"; strings.Join(got.OlderStates, ",") != want {
		t.Errorf("a browser without setPositionState was told %q; want %q", got.OlderStates, want)
	}
}
