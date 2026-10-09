package remote

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// phonePlayer runs script against the phone's own transport - the
// start, the pause, the resume, the full stop, the skip, the advance
// and the car's controls - lifted from app.js and run in node with
// the page, the storage and the audio elements faked, and decodes
// the JSON the script prints into out.
//
// The audio elements behave the way a browser's do for the purposes
// here: setting a new src rewinds the element and lands its length a
// tick later, play() settles a tick later and is interrupted by a
// pause() that lands in between, and a pause() fires the page's own
// pause handler, so a pause the page asked for and one it did not are
// told apart the way they are in a browser. Everything the page
// starts playing lands in plays with the element, the song and the
// place it started from; every pfPlay call in picks; the button's
// states in buttons; the lock screen's in media; the status line's
// in statuses. Storage (kv) outlives reload(), as the browser's does.
func phonePlayer(t *testing.T, script string, out any) {
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
	for _, name := range []string{"pfTossed", "pfPlayable", "pfListed", "pfReady", "pfNextId", "heardMark", "loadHeard",
		"heardMerged", "saveHeard", "clearHeard", "pfMarkHeard", "pfUnhear", "tossedStored", "loadTossed", "saveTossed",
		"pfForget", "pfToss", "pfAdoptRecords", "quiet", "onSystemPause", "tryResume", "startBuffered", "stopBuffered",
		"pauseBuffered", "resumeBuffered", "pfUnpause", "pfSavePosition", "pfClearPosition", "pfResumeTarget", "pfSeekTo",
		"pfPlay", "pfAdvance", "pfSkip", "pfStatus", "startListening", "stopListening", "msAction"} {
		fns = append(fns, jsFunction(t, src, name))
	}
	capLine := regexp.MustCompile(`var maxHeard = \d+;`).FindString(src)
	if capLine == "" {
		t.Fatal("app.js does not declare maxHeard")
	}
	tossCapLine := regexp.MustCompile(`var maxTossed = \d+;`).FindString(src)
	if tossCapLine == "" {
		t.Fatal("app.js does not declare maxTossed")
	}
	harness := `
var window = { AbortController: AbortController };
var clock = 1000000;
Date.now = function () { return clock++; };
var kv = {};
var store = {
  get: function (k, d) { return k in kv ? JSON.parse(kv[k]) : d; },
  set: function (k, v) { kv[k] = JSON.stringify(v); }
};
` + capLine + `
` + tossCapLine + `
// The place is written on every tick here, so a test can read it
// straight after the tick it is about.
var positionEveryMs = 0;
var pfStallMs = 8000, maxPlayFails = 3;
var resumePending = false, autoStarting = false, pfPlayFails = 0, lastManualSkip = 0;
var lastNow = "", msArtist = "", mode = "live", carSave = false, repeatOne = null;
var stateEl = { textContent: "" };
var statuses = [], buttons = [], media = [], plays = [], picks = [], systemPauses = 0;
var dbRecords = [];
function $() { return { textContent: "", hidden: false }; }
function setText() {}
function setHTML() {}
function setAttr() {}
function setStatus(el, text) { statuses.push(text); }
function streamState(text) { statuses.push(text); }
function pfState(text) { if (pf.active) statuses.push(text); }
function setPlayButton(on) { buttons.push(!!on); }
function mediaPlaybackState(s) { media.push(s); }
function applyMediaMetadata() {}
function pfShowMinutes() {}
function updateSaveButtons() {}
function updateSeek() {}
function paintNow() {}
function pfPreloadNext() {}
function pfRefreshQueue() {}
function pfAbortQueue() {}
function pfEnsureDownloads() {}
function pfNothingNew() { return false; }
function disarmGestureStart() {}
function armGestureStart() {}
function syncWarm() {}
function primeAudio() {}
function playTroubleCue() { return 0; }
function pfSetLoop() { pf.loop = false; }
function savedPlay() {}
function updateLoopState() {}
function step() {}
function carSaveAction() {}
var savedAudio = { pause: function () {} };
function dbReady() { return Promise.resolve(); }
function idbStore() { return { put: function () { return {}; }, get: function () { return {}; }, "delete": function () { return {}; }, getAll: function () { return { all: true }; }, clear: function () { return {}; } }; }
function idbReq(req) { return Promise.resolve(req && req.all ? dbRecords : null); }
var URL = { createObjectURL: function (blob) { return "blob:" + (blob && blob.id || "x"); }, revokeObjectURL: function () {} };
var durations = {};
var playRefuse = null;
// makeEl is one audio element, faked to the letter of what the page
// relies on.
function makeEl(i) {
  var el = { id: "bufaudio" + i, paused: true, ended: false, currentTime: 0, duration: NaN, loop: false,
    onended: null, ontimeupdate: null, _appPause: false, parentNode: null, listeners: {}, pending: null };
  var srcNow = "";
  Object.defineProperty(el, "src", {
    get: function () { return srcNow; },
    set: function (v) {
      if (v === srcNow) return;
      srcNow = v;
      el.currentTime = 0;
      el.duration = NaN;
      el.ended = false;
      if (!v) return;
      // The length lands a tick later, as metadata does.
      setTimeout(function () {
        if (srcNow !== v) return;
        el.duration = durations[v] || 100;
        (el.listeners.loadedmetadata || []).slice().forEach(function (fn) { fn(); });
      }, 0);
    }
  });
  el.play = function () {
    return new Promise(function (resolve, reject) {
      if (playRefuse) { var e = playRefuse; playRefuse = null; reject(e); return; }
      el.pending = function (err) { el.pending = null; if (err) { reject(err); return; } el.paused = false; plays.push({ el: i, id: pf.playingId, t: el.currentTime }); resolve(); };
      setTimeout(function () { if (el.pending) el.pending(null); }, 1);
    });
  };
  el.pause = function () {
    if (el.pending) { var e = new Error("interrupted by pause"); e.name = "AbortError"; el.pending(e); }
    if (el.paused) return;
    el.paused = true;
    onSystemPause({ target: el });
  };
  el.load = function () {};
  el.removeAttribute = function (n) { if (n === "src") el.src = ""; };
  el.addEventListener = function (n, fn) { (el.listeners[n] = el.listeners[n] || []).push(fn); };
  el.removeEventListener = function (n, fn) { el.listeners[n] = (el.listeners[n] || []).filter(function (f) { return f !== fn; }); };
  // tick is the element playing on by that many seconds.
  el.tick = function (secs) { el.currentTime += secs; if (el.ontimeupdate) el.ontimeupdate(); };
  return el;
}
function pfEl(i) {
  if (!pf.els[i]) pf.els[i] = makeEl(i);
  return pf.els[i];
}
var pf;
// reload is what opening the page does: every field starts over, and
// only what was written to storage comes back.
function reload() {
  pf = { active: false, warm: false, db: null, epoch: -1, rows: [], have: {}, playingId: null, prevId: null,
    els: [null, null], cur: 0, ctrl: null, queueCtrl: null, queueTag: "", fetchTimer: null, queueTimer: null,
    offline: false, seen: {}, wrapped: false, wantPlay: false, paused: false, resumeAt: null, posSavedAt: 0,
    loop: false, storeFull: false, tossed: {}, bad: {}, playWhy: "", lastAdvance: 0, nudged: false, skipped: 0,
    switchOnDownload: false };
  loadTossed();
  pf.seen = loadHeard();
  resumePending = false; lastManualSkip = 0; playRefuse = null;
  statuses = []; buttons = []; media = []; plays = []; picks = []; systemPauses = 0;
}
// bank puts songs on the device as a running page has them; listed
// puts them in the radio's listing in that order.
function bank(list) {
  list.split(/\s+/).filter(Boolean).forEach(function (id) {
    pf.have[id] = { url: "blob:" + id, title: "Song " + id, dur: 100, hash: "h-" + id, prompt: "" };
  });
}
function listed(list) {
  pf.rows = list.split(/\s+/).filter(Boolean).map(function (id) { return { id: id, title: "Song " + id, duration_s: 100, hash: "h-" + id }; });
}
// records is what the device's store hands a fresh page.
function records(list) {
  dbRecords = list.split(/\s+/).filter(Boolean).map(function (id) {
    return { id: id, blob: { id: id }, title: "Song " + id, dur: 100, hash: "h-" + id, prompt: "", saved: Date.now() };
  });
}
function running() { pf.active = true; pf.wantPlay = true; }
function cur() { return pf.els[pf.cur]; }
function snap() {
  var el = cur();
  return { id: pf.playingId, active: pf.active, paused: pf.paused, el: el ? el.id : "", src: el ? el.src : "", t: el ? el.currentTime : -1,
    elPaused: el ? el.paused : true, stPaused: store.get("iar.paused", null), stPlaying: store.get("iar.wasplaying", null),
    position: store.get("iar.position", null), button: buttons.length ? buttons[buttons.length - 1] : null,
    media: media.length ? media[media.length - 1] : "", status: statuses.length ? statuses[statuses.length - 1] : "",
    picks: picks.map(function (p) { return p.id; }), plays: plays.map(function (p) { return p.id + "@" + p.t; }) };
}
function wait(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
` + strings.Join(fns, "\n") + `
// Every pick of a song, and every pause the page takes for the
// system's, are counted around the functions as they ship.
var realPfPlay = pfPlay;
pfPlay = function (id) { picks.push({ id: id }); return realPfPlay(id); };
var realOnSystemPause = onSystemPause;
onSystemPause = function (e) { var was = resumePending; realOnSystemPause(e); if (resumePending && !was) systemPauses++; };
var out = {};
function finish() { console.log(JSON.stringify(out)); process.exit(0); }
setTimeout(function () { out.hung = true; finish(); }, 5000);
` + script
	cmd := exec.Command(node, "-e", harness)
	outRaw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, outRaw)
	}
	var hung struct {
		Hung bool `json:"hung"`
	}
	if err := json.Unmarshal(outRaw, &hung); err != nil {
		t.Fatalf("harness output %q: %v", outRaw, err)
	}
	if hung.Hung {
		t.Fatalf("the player never settled; seen so far: %s", outRaw)
	}
	if err := json.Unmarshal(outRaw, out); err != nil {
		t.Fatalf("harness output %q: %v", outRaw, err)
	}
}

// playerSnap is one reading of the player: the song, the element
// under it, the stores, and what the button, the lock screen and the
// status line last said.
type playerSnap struct {
	ID       string   `json:"id"`
	Active   bool     `json:"active"`
	Paused   bool     `json:"paused"`
	El       string   `json:"el"`
	Src      string   `json:"src"`
	T        float64  `json:"t"`
	ElPaused bool     `json:"elPaused"`
	StPaused *bool    `json:"stPaused"`
	StPlay   *bool    `json:"stPlaying"`
	Position *songPos `json:"position"`
	Button   *bool    `json:"button"`
	Media    string   `json:"media"`
	Status   string   `json:"status"`
	Picks    []string `json:"picks"`
	Plays    []string `json:"plays"`
}

type songPos struct {
	ID string  `json:"id"`
	T  float64 `json:"t"`
}

func boolText(b *bool) string {
	if b == nil {
		return "unset"
	}
	if *b {
		return "true"
	}
	return "false"
}

// The owner's complaint: a good song, a pause to talk to someone, and
// the song gone - the main button was a stop, and play started the
// next song. A pause now keeps the song on its element at the place
// it reached, writes the pause and the place down, tells the lock
// screen it is paused rather than gone, and says "paused" on the
// page; the status check that runs on every poll leaves a paused
// song alone however long it stays paused, an output coming back does
// not start it, and the resume plays the same element from the same
// place without picking a song.
func TestPhonePauseKeepsTheSongAndResumesItInPlace(t *testing.T) {
	var got struct {
		Playing      playerSnap `json:"playing"`
		Paused       playerSnap `json:"paused"`
		SystemPauses int        `json:"systemPauses"`
		Later        playerSnap `json:"later"`
		TryResume    bool       `json:"tryResume"`
		Resumed      playerSnap `json:"resumed"`
	}
	phonePlayer(t, `
reload(); running(); bank("A B C"); listed("A B C");
pfPlay("A");
wait(5).then(function () {
  cur().tick(42);
  out.playing = snap();
  pauseBuffered();
  out.paused = snap();
  out.systemPauses = systemPauses;
  // Minutes go by paused; the poll keeps checking the status.
  clock += 600000;
  pfStatus();
  out.later = snap();
  // The car's output comes back, the page becomes visible: neither
  // is a reason to make sound.
  out.tryResume = tryResume();
  resumeBuffered();
  return wait(5);
}).then(function () {
  out.resumed = snap();
  finish();
});
`, &got)
	p := got.Playing
	if p.ID != "A" || p.ElPaused || p.T != 42 || p.Position == nil || p.Position.ID != "A" || p.Position.T != 42 {
		t.Fatalf("before the pause: %+v (want A playing at 42 with its place written)", p)
	}
	p = got.Paused
	if !p.Active || !p.Paused || p.ID != "A" || p.Src != "blob:A" || p.T != 42 || !p.ElPaused {
		t.Errorf("a pause changed the song: %+v (want A kept on its element at 42, paused, device still running)", p)
	}
	if boolText(p.StPaused) != "true" || boolText(p.StPlay) != "false" {
		t.Errorf("a pause wrote iar.paused=%s iar.wasplaying=%s; want true and false", boolText(p.StPaused), boolText(p.StPlay))
	}
	if p.Position == nil || p.Position.ID != "A" || p.Position.T != 42 {
		t.Errorf("a pause wrote the place %+v; want A at 42", p.Position)
	}
	if p.Media != "paused" || p.Button == nil || *p.Button {
		t.Errorf("a pause told the lock screen %q and the button %s; want paused and Play", p.Media, boolText(p.Button))
	}
	if !strings.HasPrefix(p.Status, "paused · ") || !strings.Contains(p.Status, "2 ahead") {
		t.Errorf("a pause said %q; want \"paused · 2 ahead\"", p.Status)
	}
	if got.SystemPauses != 0 {
		t.Errorf("the page's own pause was taken for the system's %d times", got.SystemPauses)
	}
	l := got.Later
	if !l.ElPaused || l.T != 42 || len(l.Picks) != 1 || !strings.HasPrefix(l.Status, "paused") {
		t.Errorf("ten minutes into the pause the status check left the song: %+v (want still paused at 42, no prod, status still paused)", l)
	}
	if got.TryResume {
		t.Error("an output coming back resumed a song the listener had paused")
	}
	r := got.Resumed
	if r.Paused || r.ID != "A" || r.Src != "blob:A" || r.ElPaused || r.T != 42 {
		t.Errorf("the resume: %+v (want A playing on from 42 on the same element)", r)
	}
	if strings.Join(r.Picks, " ") != "A" || strings.Join(r.Plays, " ") != "A@0 A@42" {
		t.Errorf("the resume picked %v and played %v; want no new pick and the same element played from 42", r.Picks, r.Plays)
	}
	if boolText(r.StPaused) != "false" || boolText(r.StPlay) != "true" || r.Media != "playing" || r.Button == nil || !*r.Button {
		t.Errorf("the resume left iar.paused=%s iar.wasplaying=%s lock screen %q button %s; want false, true, playing, Pause",
			boolText(r.StPaused), boolText(r.StPlay), r.Media, boolText(r.Button))
	}
}

// The page reloaded while paused - by a tap, or by a browser waking a
// tab it had put to sleep - does not start by itself, and the one
// press of Play brings the same song back from where it was. A reload
// while playing comes back the same way, since the place is written
// as the song plays. A place past the end of the song is clamped. And
// a remembered song that is no longer on the device gives way to the
// next one, with a word about it.
func TestPhoneReloadedWhilePausedComesBackToTheSameSong(t *testing.T) {
	var got struct {
		Cold     playerSnap `json:"cold"`
		Back     playerSnap `json:"back"`
		Playing  playerSnap `json:"playing"`
		Clamped  playerSnap `json:"clamped"`
		Gone     playerSnap `json:"gone"`
		GoneSaid []string   `json:"goneSaid"`
	}
	phonePlayer(t, `
reload(); running(); bank("A B C"); listed("A B C");
pfPlay("A");
wait(5).then(function () {
  cur().tick(42);
  pauseBuffered();
  // The page goes away and comes back: only storage and the device's
  // store survive.
  reload(); records("A B C");
  out.cold = snap();
  // The one press.
  resumeBuffered();
  return wait(10);
}).then(function () {
  out.back = snap();
  // Playing on, and reloaded mid-song without a pause.
  cur().tick(10);
  reload(); records("A B C");
  startListening();
  return wait(10);
}).then(function () {
  out.playing = snap();
  // A place past the end of the song.
  store.set("iar.position", { id: "A", t: 500 });
  reload(); records("A B C");
  startListening();
  return wait(10);
}).then(function () {
  out.clamped = snap();
  // The remembered song is no longer on the device.
  store.set("iar.position", { id: "A", t: 30 });
  reload(); records("B C");
  startListening();
  return wait(10);
}).then(function () {
  out.gone = snap();
  out.goneSaid = statuses.slice();
  finish();
});
`, &got)
	c := got.Cold
	if c.Active || boolText(c.StPaused) != "true" || boolText(c.StPlay) != "false" || c.Position == nil || c.Position.ID != "A" || c.Position.T != 42 {
		t.Fatalf("a page reloaded while paused: %+v (want not running, iar.paused true, iar.wasplaying false, the place A at 42)", c)
	}
	b := got.Back
	if !b.Active || b.Paused || b.ID != "A" || b.ElPaused || b.T != 42 || strings.Join(b.Plays, " ") != "A@42" {
		t.Errorf("one press after the reload: %+v (want A playing from 42)", b)
	}
	if boolText(b.StPaused) != "false" || boolText(b.StPlay) != "true" {
		t.Errorf("after the press iar.paused=%s iar.wasplaying=%s; want false and true", boolText(b.StPaused), boolText(b.StPlay))
	}
	p := got.Playing
	if p.ID != "A" || p.ElPaused || p.T != 52 || strings.Join(p.Plays, " ") != "A@52" {
		t.Errorf("a reload while playing came back as %+v; want A from 52, where it was", p)
	}
	cl := got.Clamped
	if cl.ID != "A" || cl.T != 99 {
		t.Errorf("a place past the end came back as %+v; want A clamped to 99 of its 100 seconds", cl)
	}
	g := got.Gone
	if g.ID != "B" || g.T != 0 || g.Position == nil || g.Position.ID != "B" {
		t.Errorf("with the remembered song gone the page came back as %+v; want B from the top, and the place now B's", g)
	}
	said := false
	for _, s := range got.GoneSaid {
		if strings.Contains(s, "no longer on this device") {
			said = true
		}
	}
	if !said {
		t.Errorf("with the remembered song gone the page said %v; want a word that it is no longer on the device", got.GoneSaid)
	}
}

// The car: its pause button reached the page as a stop, which ended
// the session the car was showing - no resume, no reconnect without
// the phone in hand. The car's pause is a pause, the car's play
// carries on with the same song, and the car's stop is still the full
// stop.
func TestPhoneCarPauseIsAPauseAndCarStopIsAStop(t *testing.T) {
	var got struct {
		Paused  playerSnap `json:"paused"`
		Resumed playerSnap `json:"resumed"`
		Stopped playerSnap `json:"stopped"`
		Els     []string   `json:"els"`
	}
	phonePlayer(t, `
reload(); running(); bank("A B"); listed("A B");
pfPlay("A");
wait(5).then(function () {
  cur().tick(30);
  msAction("pause");
  out.paused = snap();
  msAction("play");
  return wait(5);
}).then(function () {
  out.resumed = snap();
  cur().tick(5);
  msAction("stop");
  out.stopped = snap();
  out.els = pf.els.map(function (el) { return el ? el.id : ""; });
  finish();
});
`, &got)
	p := got.Paused
	if !p.Active || !p.Paused || p.ID != "A" || p.Src != "blob:A" || p.T != 30 || !p.ElPaused || p.Media != "paused" {
		t.Errorf("the car's pause: %+v (want A kept at 30, paused, the lock screen told paused)", p)
	}
	r := got.Resumed
	if r.Paused || r.ID != "A" || r.ElPaused || r.T != 30 || strings.Join(r.Picks, " ") != "A" || r.Media != "playing" {
		t.Errorf("the car's play: %+v (want A on from 30 with no new pick)", r)
	}
	s := got.Stopped
	if s.Active || s.ID != "" || s.Media != "none" || boolText(s.StPaused) != "false" || boolText(s.StPlay) != "false" {
		t.Errorf("the car's stop: %+v (want the device stopped, nothing playing, the session ended)", s)
	}
	if strings.Join(got.Els, "") != "" {
		t.Errorf("the car's stop left elements %v on the page", got.Els)
	}
	if s.Position == nil || s.Position.ID != "A" || s.Position.T != 35 {
		t.Errorf("the stop forgot the place: %+v; want A at 35 kept for whatever starts the device again", s.Position)
	}
}

// Next while paused means "not this one": the next song plays, the
// pause is over, and the place is the new song's. A song that ends
// takes its place with it. And a pause pressed in the instant between
// picking a song and its first sound is the listener's pause: the
// song stays picked rather than marked as one that would not start.
func TestPhoneSkipAndEndMoveThePlaceAndAnInstantPauseHolds(t *testing.T) {
	var got struct {
		Skipped playerSnap `json:"skipped"`
		Ended   playerSnap `json:"ended"`
		Instant playerSnap `json:"instant"`
		Bad     []string   `json:"bad"`
		Held    playerSnap `json:"held"`
	}
	phonePlayer(t, `
reload(); running(); bank("A B C"); listed("A B C");
pfPlay("A");
wait(5).then(function () {
  cur().tick(20);
  pauseBuffered();
  pfSkip();
  return wait(5);
}).then(function () {
  cur().tick(3);
  out.skipped = snap();
  // B ends on its own.
  cur().onended();
  return wait(5);
}).then(function () {
  out.ended = snap();
  // A fresh page: a song picked and paused before its first sound.
  reload(); running(); bank("A B"); listed("A B");
  pfPlay("A");
  pauseBuffered();
  return wait(5);
}).then(function () {
  out.instant = snap();
  out.bad = Object.keys(pf.bad);
  resumeBuffered();
  return wait(5);
}).then(function () {
  out.held = snap();
  finish();
});
`, &got)
	s := got.Skipped
	if s.Paused || s.ID != "B" || s.ElPaused || s.T != 3 || s.Position == nil || s.Position.ID != "B" || s.Position.T != 3 || boolText(s.StPaused) != "false" {
		t.Errorf("Next while paused: %+v (want B playing, the pause over, the place B's)", s)
	}
	e := got.Ended
	if e.ID != "C" || e.Position == nil || e.Position.ID != "C" {
		t.Errorf("after B ended: %+v (want C playing and the place C's)", e)
	}
	i := got.Instant
	if !i.Paused || i.ID != "A" || !i.ElPaused || len(got.Bad) != 0 {
		t.Errorf("a pause in the instant before the first sound: %+v, bad songs %v (want A still picked and paused, nothing marked bad)", i, got.Bad)
	}
	h := got.Held
	if h.Paused || h.ID != "A" || h.ElPaused || strings.Join(h.Plays, " ") != "A@0" {
		t.Errorf("the resume after an instant pause: %+v (want A playing from the top)", h)
	}
}
