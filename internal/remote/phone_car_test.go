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
// and the car - the media session's state, its song, whether the
// session is a live stream, and which buttons the page asks for -
// lifted from app.js and run in node with a navigator whose
// mediaSession writes every call down: the states set on it in
// states, the titles in titles, every setPositionState in positions
// (the declared length spelled out, since JSON has no word for
// Infinity, or "cleared" for a call with no arguments) with every
// attempt counted in positionCalls, and the actions with a handler
// in handled(). The registration of the actions, which app.js does
// at its top level, is lifted along with the functions.
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
	for _, name := range []string{"applyMediaMetadata", "mediaPlaybackState", "syncMediaLiveness", "msHandler", "msAction", "syncPrevAction"} {
		fns = append(fns, jsFunction(t, src, name))
	}
	register := regexp.MustCompile(`\[("[a-z]+", )*"[a-z]+"\]\.forEach\(function \(a\) \{\n\s*msHandler\(a, function \(\) \{ msAction\(a\); \}\);\n\s*\}\);`).FindString(src)
	if register == "" {
		t.Fatal("app.js does not register its media-session actions where the harness expects them")
	}
	harness := `
var mode = "live", carSave = true, lastNow = "", msArtist = "", lastMetaSig = "", mediaLive = null;
var repeatOne = null, pf = { active: false }, savedAudio = { pause: function () {} };
function tryResume() { return false; }
function resumeBuffered() {}
function savedPlay() {}
function pauseBuffered() {}
function stopListening() {}
function pfSkip() {}
function updateLoopState() {}
function step() {}
function carSaveAction() {}
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
` + strings.Join(fns, "\n") + `
` + register + `
syncPrevAction();
var out = {};
function finish() { console.log(JSON.stringify(out)); process.exit(0); }
` + script
	cmd := exec.Command(node, "-e", harness)
	outRaw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, outRaw)
	}
	if err := json.Unmarshal(outRaw, out); err != nil {
		t.Fatalf("harness output %q: %v", outRaw, err)
	}
}

// The owner's complaint: in the car, previous and next had given way
// to seek-back and seek-forward buttons that did nothing. The phone's
// browser offers seeking on every session that does not declare
// itself live, and previous and next come only from the page's own
// handlers. The live player declares a length without end the first
// time it tells the system anything and says it once rather than on
// every poll; the saved player clears it and gets a seekable session
// back; and the handlers asked for are play, pause, stop, next and
// previous - never a seek, which would be offered whatever the length.
func TestPhoneCarGetsPreviousAndNextNotSeek(t *testing.T) {
	var got struct {
		AfterStart []string `json:"afterStart"`
		AfterPolls []string `json:"afterPolls"`
		AfterSaved []string `json:"afterSaved"`
		AfterBack  []string `json:"afterBack"`
		Handlers   []string `json:"handlers"`
		States     []string `json:"states"`
		Titles     []string `json:"titles"`
	}
	phoneCar(t, `
// A live song starts.
mediaPlaybackState("playing");
out.afterStart = positions.slice();
// Polls, a pause and a resume go by, and the song changes twice.
lastNow = "Song A"; applyMediaMetadata(); applyMediaMetadata();
mediaPlaybackState("paused"); mediaPlaybackState("playing");
lastNow = "Song B"; applyMediaMetadata();
out.afterPolls = positions.slice();
// The saved player takes over: the radio stops, a saved song plays.
mode = "saved";
mediaPlaybackState("none");
mediaPlaybackState("playing");
lastNow = "Song C"; applyMediaMetadata();
out.afterSaved = positions.slice();
// Back to the radio.
mode = "live";
mediaPlaybackState("paused");
applyMediaMetadata();
out.afterBack = positions.slice();
out.handlers = handled();
out.states = states;
out.titles = titles;
finish();
`, &got)
	live := "duration Infinity rate 1 position 0"
	if strings.Join(got.AfterStart, ",") != live {
		t.Errorf("a live song starting declared %q; want %q", got.AfterStart, live)
	}
	if strings.Join(got.AfterPolls, ",") != live {
		t.Errorf("polls and a pause in live mode declared %q; want the one declaration %q", got.AfterPolls, live)
	}
	if want := live + ",cleared"; strings.Join(got.AfterSaved, ",") != want {
		t.Errorf("the saved player declared %q; want %q", got.AfterSaved, want)
	}
	if want := live + ",cleared," + live; strings.Join(got.AfterBack, ",") != want {
		t.Errorf("back in live mode declared %q; want %q", got.AfterBack, want)
	}
	if want := "nexttrack,pause,play,previoustrack,stop"; strings.Join(got.Handlers, ",") != want {
		t.Errorf("the page handles %q; want exactly %q", got.Handlers, want)
	}
	if want := "playing,paused,playing,none,playing,paused"; strings.Join(got.States, ",") != want {
		t.Errorf("the lock screen was told %q; want %q", got.States, want)
	}
	if want := "Song A,Song B,Song C"; strings.Join(got.Titles, ",") != want {
		t.Errorf("the lock screen was shown %q; want %q", got.Titles, want)
	}
	raw, err := os.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, seek := range []string{`"seekto"`, `"seekforward"`, `"seekbackward"`} {
		if strings.Contains(string(raw), seek) {
			t.Errorf("app.js mentions %s: a handled seek is offered to the car whatever the length", seek)
		}
	}
}

// A browser that refuses a length without end throws from the
// declaration, and an older one has no declaration to make. Either
// way the page carries on telling the lock screen its state and its
// song, and a refusing browser is asked once per change rather than
// on every poll.
func TestPhoneKeepsTheLockScreenWhenTheBrowserRefusesLiveness(t *testing.T) {
	var got struct {
		States        []string `json:"states"`
		Titles        []string `json:"titles"`
		Positions     []string `json:"positions"`
		PositionCalls int      `json:"positionCalls"`
		OlderStates   []string `json:"olderStates"`
	}
	phoneCar(t, `
positionThrows = true;
mediaPlaybackState("playing");
lastNow = "Song A"; applyMediaMetadata();
mediaPlaybackState("paused"); mediaPlaybackState("playing");
mode = "saved"; mediaPlaybackState("none");
out.states = states.slice();
out.titles = titles.slice();
out.positions = positions.slice();
out.positionCalls = positionCalls;
// An older browser, with no setPositionState at all.
states = []; mode = "live"; mediaLive = null;
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
	if got.PositionCalls != 2 {
		t.Errorf("a refusing browser was asked %d times across one change of mode; want 2", got.PositionCalls)
	}
	if want := "playing"; strings.Join(got.OlderStates, ",") != want {
		t.Errorf("a browser without setPositionState was told %q; want %q", got.OlderStates, want)
	}
}
