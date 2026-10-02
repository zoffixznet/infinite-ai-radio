package remote

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// phoneBank runs script against the phone's own bank code - the
// picker, the downloader, the listing check, the heard list and the
// poll - lifted from app.js and run in node with the page, the
// storage, the audio elements and the network stubbed, and decodes
// the JSON the script prints into out.
//
// The network is what the test makes it: net.rows is the radio's
// listing ("A*" marks a taken song), net.tag its ETag, net.queueDown
// refuses the listing, net.stateDown refuses the poll, net.trackDown
// refuses song downloads. Every url fetched lands in calls, every
// song the page started playing in played, and every record written
// to the device's store in puts. Date.now ticks once per call so two
// songs are never heard in the same instant.
func phoneBank(t *testing.T, script string, out any) {
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
	for _, name := range []string{"pfTossed", "pfPlayable", "pfListed", "pfAhead", "pfReady", "pfNewRow",
		"pfNothingNew", "pfNextDownload", "pfNextId", "loadHeard", "saveHeard", "clearHeard", "pfMarkHeard",
		"pfUnhear", "pfAbortQueue", "pfRefreshQueue", "pfEpochChanged", "pfEnsureDownloads", "pfPlay",
		"pfStatus", "poll"} {
		fns = append(fns, jsFunction(t, src, name))
	}
	// The cap on the heard list is the app's own number, not a copy.
	capLine := regexp.MustCompile(`var maxHeard = \d+;`).FindString(src)
	if capLine == "" {
		t.Fatal("app.js does not declare maxHeard")
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
var queueFetchTimeout = 30, trackFetchTimeout = 200, warmDepth = 3, pfStallMs = 8000;
var pollFails = 0, pollOkAt = 0, pollStaleMs = 5000;
var depthNow = 3;
var resumePending = false, autoStarting = false, pfPlayFails = 0, maxPlayFails = 3;
var lastNow = "", msArtist = "", lastSession = "", sessionsLoaded = false, saveQueue = [];
var statuses = [];
function $() { return { textContent: "" }; }
function setText() {}
function pfState(text, cls) { statuses.push(text); }
function pfShowMinutes() {}
function pfLive() { return pf.active || pf.warm; }
function pfDepth() { return depthNow; }
function pfForget(id) { delete pf.have[id]; }
function clearTossed() { pf.tossed = {}; store.set("iar.tossed", {}); }
var puts = [];
function idbStore() { return { put: function (rec) { puts.push(rec); return {}; }, get: function () { return {}; }, "delete": function () { return {}; }, getAll: function () { return {}; }, clear: function () { return {}; } }; }
function idbReq() { return Promise.resolve(null); }
var URL = { createObjectURL: function (blob) { return "blob:" + (blob && blob.id || "x"); }, revokeObjectURL: function () {} };
var played = [];
var els = [];
function pfEl(i) {
  if (!els[i]) els[i] = { src: "", ended: false, currentTime: 0, loop: false, onended: null, ontimeupdate: null,
    play: function () { played.push(pf.playingId); return Promise.resolve(); } };
  return els[i];
}
function quiet() {}
function updateSeek() {}
function paintNow() {}
function applyMediaMetadata() {}
function mediaPlaybackState() {}
function updateSaveButtons() {}
function pfPreloadNext() {}
function pfTrimStore() {}
function disarmGestureStart() {}
function armGestureStart() {}
function stopBuffered(msg) { pf.active = false; statuses.push("stopped: " + msg); }
function loggedOut() {}
function flushSaveQueue() {}
function loadSessions() {}
function paintLoop() {}
function renderSound() {}
function renderLyrics() {}
function renderLyricsGen() {}
function renderLanguages() {}
function pfSetLoop() { pf.loop = false; }
var net = { epoch: 1, tag: '"1-1"', rows: [], queueDown: false, queueHold: false, stateDown: false, trackDown: false, lastTag: null };
var calls = [];
function headersOf(h) { return { get: function (k) { return h[k] || null; } }; }
function reply(status, body, h) {
  return { status: status, ok: status >= 200 && status < 300, headers: headersOf(h || {}),
    json: function () { return Promise.resolve(body); },
    blob: function () { return Promise.resolve({ id: body && body.id, size: 1000 }); } };
}
function refuse() { return Promise.reject(new TypeError("Failed to fetch")); }
function fetch(url, opts) {
  var u = String(url);
  calls.push(u);
  if (u.indexOf("/state") === 0) {
    return net.stateDown ? refuse() : Promise.resolve(reply(200, { epoch: net.epoch, session: "s", saved_ids: [] }));
  }
  if (u.indexOf("/api/queue") === 0) {
    if (net.queueDown) return refuse();
    if (net.queueHold) return new Promise(function (resolve, reject) {
      opts.signal.addEventListener("abort", function () { var e = new Error("aborted"); e.name = "AbortError"; reject(e); });
    });
    net.lastTag = (opts && opts.headers && opts.headers["If-None-Match"]) || null;
    if (net.lastTag && net.lastTag === net.tag) return Promise.resolve(reply(304, null, { ETag: net.tag }));
    var tracks = net.rows.map(function (r) {
      return { id: r.id, title: "Song " + r.id, duration_s: 100, taken: r.taken, url: "/queue/" + r.id + ".mp3", hash: "h-" + r.id };
    });
    return Promise.resolve(reply(200, { epoch: net.epoch, tracks: tracks }, { ETag: net.tag }));
  }
  if (/\.mp3$/.test(u)) {
    if (net.trackDown) return refuse();
    return Promise.resolve(reply(200, { id: u.replace(/^\/queue\//, "").replace(/\.mp3$/, "") }));
  }
  if (/\.json$/.test(u)) {
    var id = u.replace(/^\/queue\//, "").replace(/\.json$/, "");
    return Promise.resolve(reply(200, { id: id, prompt: "words of " + id, lyrics: "la la", hash: "h-" + id }));
  }
  return refuse();
}
// listing sets the radio's store: "A* B* C" is A and B taken, C not.
function listing(spec) {
  net.rows = spec.trim().split(/\s+/).filter(Boolean).map(function (s) {
    return { id: s.replace(/\*$/, ""), taken: /\*$/.test(s) };
  });
}
var pf;
// reload is what opening the page does to the bank's state: every
// field starts over, and only what was written to storage comes back.
function reload() {
  pf = { active: true, warm: false, db: null, epoch: -1, rows: [], have: {}, playingId: null, prevId: null,
    els: [null, null], cur: 0, ctrl: null, queueCtrl: null, queueTag: "", fetchTimer: null, queueTimer: null,
    offline: false, seen: loadHeard(), wrapped: false, wantPlay: false, loop: false, storeFull: false,
    tossed: {}, bad: {}, playWhy: "", lastAdvance: 0, nudged: false, skipped: 0, switchOnDownload: false };
  els = [];
  calls = []; played = []; puts = []; statuses = [];
}
function bank(list) {
  list.split(/\s+/).filter(Boolean).forEach(function (id) {
    pf.have[id] = { url: "blob:" + id, title: "Song " + id, dur: 100, hash: "h-" + id, prompt: "" };
  });
}
function downloaded() {
  return calls.filter(function (u) { return /\.mp3$/.test(u); }).map(function (u) { return u.replace(/^\/queue\//, "").replace(/\.mp3$/, ""); });
}
function heardStored() { return Object.keys(store.get("iar.heard", {})).sort(); }
function wait(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
` + strings.Join(fns, "\n") + `
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
		t.Fatalf("the bank never settled; seen so far: %s", outRaw)
	}
	if err := json.Unmarshal(outRaw, out); err != nil {
		t.Fatalf("harness output %q: %v", outRaw, err)
	}
}

// The owner's phone: a bank of songs all heard days ago, the radio's
// listing with those taken songs first and a hundred untaken ones
// after them, and a reload - which the phone's browser forces when it
// puts a tab to sleep. The heard list comes back from storage, the
// bank counts as nothing ahead, and the first untaken song is taken
// at once; the song replayed meanwhile finishes, and the new song is
// what plays next. Before this the phone forgot what it had heard on
// every reload, counted the heard bank as 56 songs ahead, took
// nothing, and replayed the saved songs from the top.
func TestPhoneRemembersWhatItHeardAndTopsUpAfterAReload(t *testing.T) {
	var got struct {
		Stored         []string `json:"stored"`
		Reloaded       []string `json:"reloaded"`
		Replayed       string   `json:"replayed"`
		Downloaded     []string `json:"downloaded"`
		Ahead          int      `json:"ahead"`
		Ready          int      `json:"ready"`
		Next           string   `json:"next"`
		Wrapped        bool     `json:"wrapped"`
		Played         []string `json:"played"`
		Prompt         string   `json:"prompt"`
		PutPrompt      string   `json:"putPrompt"`
		Then           string   `json:"then"`
		HeardNow       []string `json:"heardNow"`
		AheadThen      int      `json:"aheadThen"`
		DownloadedThen []string `json:"downloadedThen"`
	}
	phoneBank(t, `
listing("A* B* C* D* E* F G H I");
// Session one: the phone heard A to E, the taken part of the listing,
// with its downloader held off so only the hearing is on record.
reload(); bank("A B C D E"); depthNow = 0;
["A", "B", "C", "D", "E"].forEach(function (id) { pfPlay(id); });
wait(10).then(function () {
  out.stored = heardStored();
  // The reload: a fresh page, the bank still on the device.
  reload(); bank("A B C D E"); depthNow = 3;
  out.reloaded = Object.keys(pf.seen).sort();
  // What startBuffered does: sound from the bank first, then the
  // listing.
  var ready = pfNextId(null);
  out.replayed = ready;
  if (ready) pfPlay(ready);
  pfRefreshQueue();
  return wait(100);
}).then(function () {
  out.downloaded = downloaded();
  out.ahead = pfAhead();
  out.ready = pfReady();
  out.next = pfNextId(pf.playingId);
  out.wrapped = pf.wrapped;
  out.played = played.slice();
  out.prompt = pf.have.F ? pf.have.F.prompt : "";
  out.putPrompt = puts.length ? puts[0].prompt : "";
  // The replayed song ends: the next one is the new song, and the
  // bank tops up behind it.
  pfPlay(pfNextId(pf.playingId));
  return wait(50);
}).then(function () {
  out.then = pf.playingId;
  out.heardNow = heardStored();
  out.aheadThen = pfAhead();
  out.downloadedThen = downloaded();
  finish();
});
`, &got)
	if join(got.Stored) != "A B C D E" {
		t.Fatalf("after hearing five songs the device stored %v as heard, want A B C D E", got.Stored)
	}
	if join(got.Reloaded) != "A B C D E" {
		t.Fatalf("a reloaded page read %v back as heard, want A B C D E", got.Reloaded)
	}
	if got.Ahead != 3 || got.Ready != 3 {
		t.Errorf("with five heard songs banked and three new ones taken, ahead = %d and ready = %d; want 3 and 3 - heard songs are not ahead",
			got.Ahead, got.Ready)
	}
	if join(got.Downloaded) != "F G H" {
		t.Errorf("after the reload the phone took %v; want F G H - the first untaken songs, at once", got.Downloaded)
	}
	if got.Next != "F" || got.Wrapped {
		t.Errorf("with F banked the next song is %q (wrapped %v); want F, the new song, and no replay", got.Next, got.Wrapped)
	}
	if join(got.Played) != got.Replayed {
		t.Errorf("the download landing started %v; want only the replay %q to have started, and to finish first", got.Played, got.Replayed)
	}
	if got.Then != "F" {
		t.Errorf("when the replayed song ended the phone played %q, want F", got.Then)
	}
	if got.Prompt != "words of F" || got.PutPrompt != "words of F" {
		t.Errorf("the song's description came from the listing (%q in memory, %q stored); want it from the song's own JSON", got.Prompt, got.PutPrompt)
	}
	if join(got.HeardNow) != "A B C D E F" {
		t.Errorf("heard list after playing F = %v, want A B C D E F", got.HeardNow)
	}
	if got.AheadThen != 3 || join(got.DownloadedThen) != "F G H I" {
		t.Errorf("playing F at depth 3, ahead = %d after taking %v; want 3 and F G H I - the bank tops up behind the new song",
			got.AheadThen, got.DownloadedThen)
	}
}

// Out of reach of the radio a phone whose bank is all heard replays
// it, and says so. The moment the radio is back the first new song is
// taken, the replay playing is not cut short, the new song is what
// plays next, and the "nothing new" note goes.
func TestPhoneReplaysOnlyUntilSomethingNewLands(t *testing.T) {
	var got struct {
		Offline      bool     `json:"offline"`
		Replay       string   `json:"replay"`
		Wrapped      bool     `json:"wrapped"`
		NothingNew   bool     `json:"nothingNew"`
		Status       string   `json:"status"`
		Downloaded   []string `json:"downloaded"`
		Played       []string `json:"played"`
		Next         string   `json:"next"`
		WrappedAfter bool     `json:"wrappedAfter"`
		OfflineAfter bool     `json:"offlineAfter"`
		NothingAfter bool     `json:"nothingAfter"`
		StatusAfter  string   `json:"statusAfter"`
	}
	phoneBank(t, `
listing("A* B* C* F");
reload(); bank("A B C"); depthNow = 2;
["A", "B", "C"].forEach(function (id) { pfMarkHeard(id); });
net.queueDown = true; net.stateDown = true; pollOkAt = 0;
var first = pfNextId(null);
out.replay = first;
pfPlay(first);
pfRefreshQueue();
wait(20).then(function () {
  out.offline = pf.offline;
  out.wrapped = pf.wrapped;
  out.nothingNew = pfNothingNew();
  statuses = []; pfStatus(); out.status = statuses.join(" | ");
  // The radio is back.
  net.queueDown = false; net.stateDown = false;
  poll();
  return wait(10);
}).then(function () {
  pfRefreshQueue();
  return wait(60);
}).then(function () {
  out.downloaded = downloaded();
  out.played = played.slice();
  out.next = pfNextId(pf.playingId);
  out.wrappedAfter = pf.wrapped;
  out.offlineAfter = pf.offline;
  out.nothingAfter = pfNothingNew();
  statuses = []; pfStatus(); out.statusAfter = statuses.join(" | ");
  finish();
});
`, &got)
	if !got.Offline || !got.Wrapped || !got.NothingNew {
		t.Errorf("out of reach with an all-heard bank: offline %v, wrapped %v, nothing new %v; want all true", got.Offline, got.Wrapped, got.NothingNew)
	}
	if !strings.Contains(got.Status, "offline") {
		t.Errorf("status out of reach = %q, want it to say offline", got.Status)
	}
	if join(got.Downloaded) != "F" {
		t.Errorf("with the radio back the phone took %v, want F", got.Downloaded)
	}
	if join(got.Played) != got.Replay {
		t.Errorf("the download landing started %v; want the replay %q left to finish", got.Played, got.Replay)
	}
	if got.Next != "F" || got.WrappedAfter || got.NothingAfter || got.OfflineAfter {
		t.Errorf("after F landed: next %q, wrapped %v, nothing new %v, offline %v; want F, false, false, false",
			got.Next, got.WrappedAfter, got.NothingAfter, got.OfflineAfter)
	}
	if strings.Contains(got.StatusAfter, "nothing new") || strings.Contains(got.StatusAfter, "offline") || !strings.Contains(got.StatusAfter, "1 ahead") {
		t.Errorf("status after F landed = %q, want playing with 1 ahead and no replay or offline note", got.StatusAfter)
	}
}

// The heard list follows the store: a steer empties the radio's store
// and starts a new context whose songs can never be ones already
// heard, so the list is emptied with it. It is bounded all the same,
// oldest first.
func TestPhoneHeardListIsClearedOnASteerAndCapped(t *testing.T) {
	var got struct {
		Before     int  `json:"before"`
		AfterEpoch int  `json:"afterEpoch"`
		Stored     int  `json:"stored"`
		Capped     int  `json:"capped"`
		Max        int  `json:"max"`
		OldestGone bool `json:"oldestGone"`
		NewestKept bool `json:"newestKept"`
		StoredCap  int  `json:"storedCap"`
	}
	phoneBank(t, `
reload();
pfMarkHeard("A"); pfMarkHeard("B");
out.before = Object.keys(pf.seen).length;
pf.epoch = 1;
pfEpochChanged(2);
out.afterEpoch = Object.keys(pf.seen).length;
out.stored = heardStored().length;
for (var i = 0; i < maxHeard + 50; i++) pfMarkHeard("s" + i);
out.capped = Object.keys(pf.seen).length;
out.max = maxHeard;
out.oldestGone = !pf.seen["s0"] && !pf.seen["s49"] && !!pf.seen["s50"];
out.newestKept = !!pf.seen["s" + (maxHeard + 49)];
out.storedCap = heardStored().length;
finish();
`, &got)
	if got.Before != 2 || got.AfterEpoch != 0 || got.Stored != 0 {
		t.Errorf("heard list: %d before the steer, %d after, %d in storage; want 2, 0, 0", got.Before, got.AfterEpoch, got.Stored)
	}
	if got.Capped != got.Max || got.StoredCap != got.Max || !got.OldestGone || !got.NewestKept {
		t.Errorf("after %d songs the list holds %d (%d stored), oldest gone %v, newest kept %v; want the cap of %d, oldest first",
			got.Max+50, got.Capped, got.StoredCap, got.OldestGone, got.NewestKept, got.Max)
	}
}

// A listing the radio answers "unchanged" to is a listing: the rows
// stay, the contact counts, and the device is not offline. A changed
// store comes down whole under its new tag.
func TestPhoneKeepsItsRowsOnAnUnchangedListing(t *testing.T) {
	var got struct {
		Rows        int      `json:"rows"`
		Tag         string   `json:"tag"`
		SentTag     string   `json:"sentTag"`
		RowsAfter   int      `json:"rowsAfter"`
		Offline     bool     `json:"offline"`
		Skipped     int      `json:"skipped"`
		Downloaded  []string `json:"downloaded"`
		RowsChanged int      `json:"rowsChanged"`
		TagChanged  string   `json:"tagChanged"`
	}
	phoneBank(t, `
listing("A* B C"); net.tag = '"1-7"';
reload(); depthNow = 1;
pfRefreshQueue();
wait(20).then(function () {
  out.rows = pf.rows.length;
  out.tag = pf.queueTag;
  // The listener deepens the bank, so the unchanged listing has a
  // song to take off the rows it kept.
  depthNow = 2;
  pf.offline = true; pf.skipped = 90;
  calls = [];
  pfRefreshQueue();
  return wait(60);
}).then(function () {
  out.sentTag = net.lastTag;
  out.rowsAfter = pf.rows.length;
  out.offline = pf.offline;
  out.skipped = pf.skipped;
  out.downloaded = downloaded();
  net.tag = '"1-8"'; listing("A* B C D");
  pfRefreshQueue();
  return wait(20);
}).then(function () {
  out.rowsChanged = pf.rows.length;
  out.tagChanged = pf.queueTag;
  finish();
});
`, &got)
	if got.Rows != 3 || got.Tag != `"1-7"` {
		t.Fatalf("the first listing gave %d rows under tag %q, want 3 under \"1-7\"", got.Rows, got.Tag)
	}
	if got.SentTag != `"1-7"` {
		t.Errorf("the next check sent If-None-Match %q, want the tag it holds", got.SentTag)
	}
	if got.RowsAfter != 3 || got.Offline || got.Skipped != 0 {
		t.Errorf("after an unchanged answer: %d rows, offline %v, skipped %d; want 3, false, 0 - the contact counts", got.RowsAfter, got.Offline, got.Skipped)
	}
	if join(got.Downloaded) != "B" {
		t.Errorf("after an unchanged listing the phone took %v, want B off the rows it kept", got.Downloaded)
	}
	if got.RowsChanged != 4 || got.TagChanged != `"1-8"` {
		t.Errorf("a changed store gave %d rows under %q, want 4 under \"1-8\"", got.RowsChanged, got.TagChanged)
	}
}

// Offline means the radio cannot be reached. A poll it answers clears
// the mark; a listing that fails while the polls are being answered
// does not set it; one that fails with nothing answered for a while
// does. With no listing yet and the radio answering, the status says
// the song list is loading rather than that the radio is away.
func TestPhoneIsOfflineOnlyWhenTheRadioIsOutOfReach(t *testing.T) {
	var got struct {
		Offline      bool   `json:"offline"`
		Fails        int    `json:"fails"`
		StillOnline  bool   `json:"stillOnline"`
		StatusOnline string `json:"statusOnline"`
		WhenStale    bool   `json:"whenStale"`
		StatusStale  string `json:"statusStale"`
	}
	phoneBank(t, `
reload(); depthNow = 0;
pf.offline = true; pollFails = 2;
poll();
wait(10).then(function () {
  out.offline = pf.offline;
  out.fails = pollFails;
  net.queueDown = true;
  pfRefreshQueue();
  return wait(10);
}).then(function () {
  out.stillOnline = !pf.offline;
  statuses = []; pfStatus(); out.statusOnline = statuses.join(" | ");
  pollOkAt = Date.now() - pollStaleMs - 10;
  pfRefreshQueue();
  return wait(10);
}).then(function () {
  out.whenStale = pf.offline;
  statuses = []; pfStatus(); out.statusStale = statuses.join(" | ");
  finish();
});
`, &got)
	if got.Offline || got.Fails != 0 {
		t.Errorf("after an answered poll: offline %v, failed polls %d; want false and 0", got.Offline, got.Fails)
	}
	if !got.StillOnline {
		t.Errorf("a listing that failed while the polls were answered marked the device offline")
	}
	if !strings.Contains(got.StatusOnline, "song list") || strings.Contains(got.StatusOnline, "unreachable") {
		t.Errorf("status with no listing and the radio answering = %q, want it to say the song list is loading", got.StatusOnline)
	}
	if !got.WhenStale || !strings.Contains(got.StatusStale, "unreachable") {
		t.Errorf("a listing that failed with nothing answered lately: offline %v, status %q; want offline and said so", got.WhenStale, got.StatusStale)
	}
}

// The pieces that are not lifted into node are checked at the seams:
// every song start is written to the heard list, a skip and a flush
// take from it, and the device-bank note follows the same rule as the
// status line.
func TestPhoneHeardListIsWiredThroughPlaySkipAndFlush(t *testing.T) {
	raw, err := os.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for fn, want := range map[string]string{
		"pfPlay":         "pfMarkHeard(id)",
		"pfToss":         "pfUnhear(id)",
		"pfJumpLive":     "clearHeard()",
		"pfEpochChanged": "clearHeard()",
		"pfShowMinutes":  "pfNothingNew()",
	} {
		if !strings.Contains(jsFunction(t, src, fn), want) {
			t.Errorf("%s does not call %s", fn, want)
		}
	}
	if strings.Contains(jsFunction(t, src, "pfRefreshQueue"), "row.prompt") {
		t.Errorf("pfRefreshQueue still reads a prompt off the listing, which no longer carries one")
	}
}
