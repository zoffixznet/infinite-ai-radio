package remote

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A listener off the network hears two songs and taps Save on both.
// Each tap fails to reach the radio and is queued on the device, in
// order, in a store a page reload reads back. While the network stays
// down the queue is tried and kept; when it is back both saves are
// posted, oldest first, and both settle as saved. A post that goes
// into a black hole - packets dropped, nothing ever answered - is
// given up after its deadline as no connection, so the queue behind
// it is not held for the browser's own timeout.
func TestPhoneQueuesSavesUntilTheRadioIsBack(t *testing.T) {
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
	for _, name := range []string{"saveAnswer", "noRadio", "savePost", "postSave", "queueSave", "saveSettled",
		"scheduleSaveFlush", "persistSaveQueue", "flushSaveQueue", "loadSaveQueue"} {
		fns = append(fns, jsFunction(t, src, name))
	}
	harness := `
var window = { AbortController: AbortController };
var headers = {};
var actTimeout = 30;
var uploadTimeout = 30;
var kv = {};
var store = {
  get: function (k, d) { return k in kv ? JSON.parse(kv[k]) : d; },
  set: function (k, v) { kv[k] = JSON.stringify(v); }
};
var saveQueue = [], savingIds = {}, localSaved = {};
var saveQueueMaxAge = 24 * 60 * 60 * 1000, saveRetryMs = 5;
var saveFlushing = false, saveRetryTimer = null;
var mode = "live", pf = { have: {} }, stateEl = null;
function $() { return null; }
function setStatus() {}
function say() {}
function loadChunks() {}
function loggedOut() {}
function updateSaveButtons() {}
function saveNews() { return []; }
function bankedBlob() { return Promise.resolve(null); }
var net = { down: true, hole: false };
var posted = [];
function fetch(url, opts) {
  if (net.hole) {
    return new Promise(function (resolve, reject) {
      if (opts.signal) opts.signal.addEventListener("abort", function () {
        var e = new Error("aborted"); e.name = "AbortError"; reject(e);
      });
    });
  }
  if (net.down) return Promise.reject(new TypeError("Failed to fetch"));
  posted.push(decodeURIComponent(String(opts.body)));
  var ack = posted.length === 1 ? "saving this track to gym/a.mp3" : "saving this track to gym/b.mp3, behind 1 other save";
  return Promise.resolve({ status: 200, ok: true, json: function () { return Promise.resolve({ ok: true, ack: ack, saved: true }); } });
}
` + strings.Join(fns, "\n") + `
function wait(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
// tap is what a Save tap does with a post that found no radio.
function tap(id) {
  return postSave(id, "gym").then(function () { return null; }, function (e) {
    if (e && e.offline) queueSave(id, "gym", "Song " + id);
  });
}
function queued() { return (store.get("iar.savequeue", [])).map(function (i) { return i.id; }); }
var out = {};
// A save that hangs would hang the run; report what was seen instead.
setTimeout(function () { out.hung = true; console.log(JSON.stringify(out)); process.exit(0); }, 3000);
tap("A").then(function () { return tap("B"); }).then(function () {
  out.queued = queued();
  out.saving = Object.keys(savingIds).sort();
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
  out.saved = Object.keys(localSaved).sort();
  out.stillSaving = Object.keys(savingIds);
  net.hole = true;
  var t0 = Date.now();
  return postSave("C", "gym").then(function () { out.holeOffline = false; }, function (e) {
    out.holeOffline = !!(e && e.offline);
    out.holeMs = Date.now() - t0;
  });
}).then(function () {
  console.log(JSON.stringify(out));
  process.exit(0);
});
`
	cmd := exec.Command(node, "-e", harness)
	outRaw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, outRaw)
	}
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
		Hung            bool     `json:"hung"`
	}
	if err := json.Unmarshal(outRaw, &got); err != nil {
		t.Fatalf("harness output %q: %v", outRaw, err)
	}
	if got.Hung {
		t.Fatalf("a save never settled; seen so far: %s", outRaw)
	}
	join := func(s []string) string { return strings.Join(s, " ") }
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
