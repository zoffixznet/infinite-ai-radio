package remote

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// phoneSaves runs script against the phone's own save-queue code,
// lifted from app.js and run in node with the page, the storage and
// the network stubbed, and decodes the JSON the script prints into out.
//
// The network is what the test makes it: net.down refuses every post,
// net.hole answers nothing ever, net.held keeps an answer back until
// net.release() is called, and net.answer(body) chooses the answer to
// a post. A song's copy goes out over net.link, so many bytes a tick,
// stopping for good past net.link.stallAt when that is set. The
// deadlines are shrunk to milliseconds so a run takes well under a
// second, and a run that hangs reports what it saw instead.
func phoneSaves(t *testing.T, script string, out any) {
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
	for _, name := range []string{"saveAnswer", "noRadio", "savePost", "sendCopy", "postSave", "queueSave",
		"saveSettled", "scheduleSaveFlush", "persistSaveQueue", "dropQueuedSave", "flushSaveQueue",
		"loadSaveQueue", "updateSaveButtons"} {
		fns = append(fns, jsFunction(t, src, name))
	}
	harness := `
var window = { AbortController: AbortController };
var headers = {};
var actTimeout = 30;
var uploadStallMs = 60;
var kv = {};
var store = {
  get: function (k, d) { return k in kv ? JSON.parse(kv[k]) : d; },
  set: function (k, v) { kv[k] = JSON.stringify(v); }
};
var saveQueue = [], savingIds = {}, localSaved = {}, savedIds = {};
var saveQueueMaxAge = 24 * 60 * 60 * 1000, saveRetryMs = 5;
var saveFlushing = false, saveRetryTimer = null;
var mode = "live", pf = { have: {} }, stateEl = null;
var statuses = [];
function $() { return null; }
function setStatus(el, text, kind) { statuses.push((kind === "err" ? "err: " : "") + text); }
function say() {}
function loadChunks() {}
function loggedOut() {}
function saveNews() { return []; }
function saveTargets() { return { cur: null, prev: null }; }
function setSavedClass() {}
function paintSaveMark() {}
function applyMediaMetadata() {}
var bank = {};
function bankedBlob(id) { return Promise.resolve(bank[id] || null); }
var net = { down: true, hole: false, held: false, release: null, answer: null,
  link: { rate: 1000, tick: 1, stallAt: -1, status: 200, body: null } };
var posted = [];
function reply(status, body) {
  return { status: status, ok: status >= 200 && status < 300, json: function () { return Promise.resolve(body); } };
}
function saving(file) { return reply(200, { ok: true, ack: "saving this track to gym/" + file, saved: true }); }
function fetch(url, opts) {
  if (net.hole) {
    return new Promise(function (resolve, reject) {
      if (opts.signal) opts.signal.addEventListener("abort", function () {
        var e = new Error("aborted"); e.name = "AbortError"; reject(e);
      });
    });
  }
  if (net.down) return Promise.reject(new TypeError("Failed to fetch"));
  var body = decodeURIComponent(String(opts.body));
  posted.push(body);
  var r = net.answer ? net.answer(body) : saving("x.mp3");
  if (net.held) return new Promise(function (resolve) { net.release = function () { resolve(r); }; });
  return Promise.resolve(r);
}
var uploads = [];
function XMLHttpRequest() { this.upload = {}; this.status = 0; this.responseText = ""; }
XMLHttpRequest.prototype.open = function (method, url) { this.url = url; };
XMLHttpRequest.prototype.setRequestHeader = function () {};
XMLHttpRequest.prototype.abort = function () {
  if (this.rec.outcome) return;
  clearInterval(this.tick);
  this.rec.outcome = "aborted";
  this.rec.ms = Date.now() - this.rec.started;
  if (this.onabort) this.onabort();
};
XMLHttpRequest.prototype.send = function (blob) {
  var xhr = this, link = net.link, sent = 0;
  xhr.rec = { url: xhr.url, size: blob.size, sent: 0, outcome: "", started: Date.now(), ms: 0 };
  uploads.push(xhr.rec);
  xhr.tick = setInterval(function () {
    if (link.stallAt >= 0 && sent >= link.stallAt) return;
    sent = Math.min(blob.size, sent + link.rate);
    xhr.rec.sent = sent;
    if (xhr.upload.onprogress) xhr.upload.onprogress({ lengthComputable: true, loaded: sent, total: blob.size });
    if (sent < blob.size) return;
    clearInterval(xhr.tick);
    xhr.rec.outcome = "sent";
    xhr.rec.ms = Date.now() - xhr.rec.started;
    xhr.status = link.status;
    xhr.responseText = JSON.stringify(link.body || { ok: true, ack: "track saved: gym/" + "x.mp3", saved: true });
    if (xhr.onload) xhr.onload();
  }, link.tick);
};
` + strings.Join(fns, "\n") + `
function wait(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
// tap is what a Save tap does with a post that found no radio.
function tap(id) {
  return postSave(id, "gym").then(function () { return null; }, function (e) {
    if (e && e.offline) queueSave(id, "gym", "Song " + id);
  });
}
function queued() { return (store.get("iar.savequeue", [])).map(function (i) { return i.id; }); }
function saved() { return Object.keys(localSaved).sort(); }
function stillSaving() { return Object.keys(savingIds).sort(); }
var out = {};
function finish() { console.log(JSON.stringify(out)); process.exit(0); }
// A save that hangs would hang the run; report what was seen instead.
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
		t.Fatalf("a save never settled; seen so far: %s", outRaw)
	}
	if err := json.Unmarshal(outRaw, out); err != nil {
		t.Fatalf("harness output %q: %v", outRaw, err)
	}
}

func join(s []string) string { return strings.Join(s, " ") }

// A listener off the network hears two songs and taps Save on both.
// Each tap fails to reach the radio and is queued on the device, in
// order, in a store a page reload reads back. While the network stays
// down the queue is tried and kept; when it is back both saves are
// posted, oldest first, and both settle as saved. A post that goes
// into a black hole - packets dropped, nothing ever answered - is
// given up after its deadline as no connection, so the queue behind
// it is not held for the browser's own timeout.
func TestPhoneQueuesSavesUntilTheRadioIsBack(t *testing.T) {
	var got struct {
		Queued          []string `json:"queued"`
		Saving          []string `json:"saving"`
		Reloaded        []string `json:"reloaded"`
		HeldWhileDown   []string `json:"heldWhileDown"`
		PostedWhileDown int      `json:"postedWhileDown"`
		Posted          []string `json:"posted"`
		Left            []string `json:"left"`
		Saved           []string `json:"saved"`
		StillSaving     []string `json:"stillSaving"`
		HoleOffline     bool     `json:"holeOffline"`
		HoleMs          int      `json:"holeMs"`
	}
	phoneSaves(t, `
net.answer = function () { return posted.length === 1 ? saving("a.mp3") : reply(200, { ok: true, ack: "saving this track to gym/b.mp3, behind 1 other save", saved: true }); };
tap("A").then(function () { return tap("B"); }).then(function () {
  out.queued = queued();
  out.saving = stillSaving();
  // A page reloaded off the network reads the same queue back.
  saveQueue = []; savingIds = {};
  loadSaveQueue();
  out.reloaded = saveQueue.map(function (i) { return i.id; });
  return wait(40);
}).then(function () {
  out.heldWhileDown = queued();
  out.postedWhileDown = posted.length;
  net.down = false;
  flushSaveQueue(); // what the browser's "online" event does
  return wait(60);
}).then(function () {
  out.posted = posted;
  out.left = queued();
  out.saved = saved();
  out.stillSaving = stillSaving();
  net.hole = true;
  var t0 = Date.now();
  return postSave("C", "gym").then(function () { out.holeOffline = false; }, function (e) {
    out.holeOffline = !!(e && e.offline);
    out.holeMs = Date.now() - t0;
  });
}).then(finish);
`, &got)
	if join(got.Queued) != "A B" || join(got.Saving) != "A B" {
		t.Errorf("after two taps off the network the store holds %v and %v are saving; want A B and A B", got.Queued, got.Saving)
	}
	if join(got.Reloaded) != "A B" {
		t.Errorf("a reloaded page read %v back from the store, want A B", got.Reloaded)
	}
	if join(got.HeldWhileDown) != "A B" || got.PostedWhileDown != 0 {
		t.Errorf("while the network stayed down the queue became %v with %d posts; want A B and none",
			got.HeldWhileDown, got.PostedWhileDown)
	}
	if join(got.Posted) != "tag=gym&which=A tag=gym&which=B" {
		t.Errorf("with the network back the phone posted %v; want A then B", got.Posted)
	}
	if len(got.Left) != 0 || join(got.Saved) != "A B" || len(got.StillSaving) != 0 {
		t.Errorf("after the flush %v are still queued, %v saved, %v saving; want none, A B, none",
			got.Left, got.Saved, got.StillSaving)
	}
	if !got.HoleOffline {
		t.Errorf("a post nothing answers did not end as no connection")
	}
	if got.HoleMs < 20 || got.HoleMs > 1000 {
		t.Errorf("a post nothing answers was given up after %d ms, want about its 30 ms deadline", got.HoleMs)
	}
}

// The first queued save is posted and its answer is slow coming back;
// meanwhile a poll learns the radio already has that song and takes
// the entry out of the queue. When the answer lands it settles the
// song it was for, and the next queued song is still posted - it used
// to be shifted off the queue in the first one's place, unposted and
// gone from the store.
func TestPhoneKeepsTheNextSaveWhenAPollSettlesTheOneInFlight(t *testing.T) {
	var got struct {
		PostedBeforePoll []string `json:"postedBeforePoll"`
		QueuedAfterPoll  []string `json:"queuedAfterPoll"`
		Posted           []string `json:"posted"`
		Left             []string `json:"left"`
		Saved            []string `json:"saved"`
		StillSaving      []string `json:"stillSaving"`
	}
	phoneSaves(t, `
tap("A").then(function () { return tap("B"); }).then(function () {
  net.down = false; net.held = true;
  flushSaveQueue(); // posts A; the answer is kept back on its way home
  return wait(10);
}).then(function () {
  out.postedBeforePoll = posted.slice();
  // The next poll: a flush first (a no-op while one is in flight),
  // then the radio's word that it has A already.
  flushSaveQueue();
  updateSaveButtons({ saved_ids: ["A"] });
  out.queuedAfterPoll = queued();
  net.held = false;
  net.release(); // A's answer lands now
  return wait(60);
}).then(function () {
  out.posted = posted.slice();
  out.left = queued();
  out.saved = saved();
  out.stillSaving = stillSaving();
  finish();
});
`, &got)
	if join(got.PostedBeforePoll) != "tag=gym&which=A" {
		t.Fatalf("before the poll the phone had posted %v, want A alone", got.PostedBeforePoll)
	}
	if join(got.QueuedAfterPoll) != "B" {
		t.Fatalf("after the poll took A out the store holds %v, want B", got.QueuedAfterPoll)
	}
	if join(got.Posted) != "tag=gym&which=A tag=gym&which=B" {
		t.Errorf("the phone posted %v; want A then B", got.Posted)
	}
	if len(got.Left) != 0 || join(got.Saved) != "A B" || len(got.StillSaving) != 0 {
		t.Errorf("after the flush %v are still queued, %v saved, %v saving; want none, A B, none",
			got.Left, got.Saved, got.StillSaving)
	}
}

// The radio answers a queued save with "shutting down": it is going
// away for a moment, not refusing the song. The phone keeps the entry
// and tries again, and delivers it once the radio is back.
func TestPhoneKeepsASaveTheRadioWasShuttingDownFor(t *testing.T) {
	var got struct {
		PostedWhileClosing []string `json:"postedWhileClosing"`
		KeptWhileClosing   []string `json:"keptWhileClosing"`
		SavingWhileClosing []string `json:"savingWhileClosing"`
		Posted             []string `json:"posted"`
		Left               []string `json:"left"`
		Saved              []string `json:"saved"`
		Statuses           []string `json:"statuses"`
	}
	phoneSaves(t, `
net.down = false;
var closing = true;
net.answer = function () {
  if (closing) return reply(503, { ok: true, ack: "the radio is shutting down; that track was not saved" });
  return saving("x.mp3");
};
// Two saves queued earlier, off the network.
queueSave("A", "gym", "Song A");
queueSave("B", "gym", "Song B");
flushSaveQueue();
wait(40).then(function () {
  out.postedWhileClosing = posted.slice();
  out.keptWhileClosing = queued();
  out.savingWhileClosing = stillSaving();
  closing = false; // the radio is back
  return wait(60);
}).then(function () {
  out.posted = posted.slice();
  out.left = queued();
  out.saved = saved();
  out.statuses = statuses;
  finish();
});
`, &got)
	if len(got.PostedWhileClosing) == 0 {
		t.Fatal("nothing was posted while the radio was shutting down")
	}
	for _, p := range got.PostedWhileClosing {
		if p != "tag=gym&which=A" {
			t.Fatalf("while the radio was shutting down the phone posted %v; want A alone, tried again", got.PostedWhileClosing)
		}
	}
	if join(got.KeptWhileClosing) != "A B" || join(got.SavingWhileClosing) != "A B" {
		t.Fatalf("while the radio was shutting down the store held %v and %v were saving; want A B and A B",
			got.KeptWhileClosing, got.SavingWhileClosing)
	}
	if n := len(got.Posted); n < 2 || got.Posted[n-2] != "tag=gym&which=A" || got.Posted[n-1] != "tag=gym&which=B" {
		t.Errorf("with the radio back the phone posted %v; want A then B at the end", got.Posted)
	}
	if len(got.Left) != 0 || join(got.Saved) != "A B" {
		t.Errorf("after the radio came back %v are still queued and %v saved; want none and A B", got.Left, got.Saved)
	}
	for _, s := range got.Statuses {
		if strings.HasPrefix(s, "err:") {
			t.Errorf("the phone reported %q for a save the radio only postponed", s)
		}
	}
}

// The radio has let a song go, so the phone sends its own copy. Over a
// slow link the copy takes longer than any fixed deadline would allow,
// and gets through, because the deadline moves with every byte that
// does. A link that stops moving is given up as no connection after
// the stall deadline, and the entry waits for the signal - to be sent
// again from the start, not settled as failed.
func TestPhoneGivesASlowCopyItsTimeAndGivesUpOnlyAStalledOne(t *testing.T) {
	var got struct {
		Slow struct {
			Outcome string   `json:"outcome"`
			Sent    int      `json:"sent"`
			Ms      int      `json:"ms"`
			Left    []string `json:"left"`
			Saved   []string `json:"saved"`
		} `json:"slow"`
		Stalled struct {
			Outcome  string   `json:"outcome"`
			Sent     int      `json:"sent"`
			Ms       int      `json:"ms"`
			Attempts int      `json:"attempts"`
			Left     []string `json:"left"`
			Saving   []string `json:"saving"`
			Saved    []string `json:"saved"`
			Statuses []string `json:"statuses"`
		} `json:"stalled"`
	}
	phoneSaves(t, `
net.down = false;
net.answer = function () { return reply(200, { ok: true, ack: "the radio no longer has that song; send the copy on this device", upload: true }); };
bank["C"] = { size: 1000 };
bank["D"] = { size: 1000 };
// 100 bytes every 10 ms: the whole copy takes 100 ms, past the 60 ms
// stall deadline, but it never stands still.
net.link = { rate: 100, tick: 10, stallAt: -1, status: 200, body: null };
queueSave("C", "gym", "Song C");
flushSaveQueue();
wait(250).then(function () {
  out.slow = { outcome: uploads[0].outcome, sent: uploads[0].sent, ms: uploads[0].ms, left: queued(), saved: saved() };
  // The link dies with the copy a third of the way out: nothing more
  // ever moves.
  net.link = { rate: 100, tick: 10, stallAt: 300, status: 200, body: null };
  queueSave("D", "gym", "Song D");
  flushSaveQueue();
  return wait(250);
}).then(function () {
  out.stalled = { outcome: uploads[1].outcome, sent: uploads[1].sent, ms: uploads[1].ms, attempts: uploads.length - 1,
    left: queued(), saving: stillSaving(), saved: saved(), statuses: statuses };
  finish();
});
`, &got)
	if got.Slow.Outcome != "sent" || got.Slow.Sent != 1000 {
		t.Fatalf("the slow copy ended %q with %d bytes out after %d ms; want it sent whole", got.Slow.Outcome, got.Slow.Sent, got.Slow.Ms)
	}
	if got.Slow.Ms < 60 {
		t.Errorf("the slow copy went out in %d ms, which does not outlast the 60 ms stall deadline it is meant to", got.Slow.Ms)
	}
	if len(got.Slow.Left) != 0 || join(got.Slow.Saved) != "C" {
		t.Errorf("after the slow copy %v are still queued and %v saved; want none and C", got.Slow.Left, got.Slow.Saved)
	}
	if got.Stalled.Outcome != "aborted" || got.Stalled.Sent != 300 {
		t.Fatalf("the stalled copy ended %q with %d bytes out; want it given up at 300", got.Stalled.Outcome, got.Stalled.Sent)
	}
	if got.Stalled.Ms < 50 || got.Stalled.Ms > 1000 {
		t.Errorf("the stalled copy was given up after %d ms, want about the 60 ms stall deadline", got.Stalled.Ms)
	}
	if got.Stalled.Attempts < 2 {
		t.Errorf("the stalled copy was tried %d time(s); want it tried again after being given up", got.Stalled.Attempts)
	}
	if join(got.Stalled.Left) != "D" || join(got.Stalled.Saving) != "D" || join(got.Stalled.Saved) != "C" {
		t.Errorf("after the stalled copy %v are queued, %v saving, %v saved; want D, D, C",
			got.Stalled.Left, got.Stalled.Saving, got.Stalled.Saved)
	}
	for _, s := range got.Stalled.Statuses {
		if strings.HasPrefix(s, "err:") {
			t.Errorf("the phone reported %q for a copy it is still to send", s)
		}
	}
}
