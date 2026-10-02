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
// listing ("A*" marks a taken song), net.tag its ETag, net.hashes
// overrides a song's hash, net.queueDown refuses the listing,
// net.queueHold never answers it, net.bodyFail cuts its body off
// after the headers, net.bodyHold keeps the body back until
// net.release(), net.stateDown refuses the poll, net.trackDown
// refuses song downloads and net.trackHold keeps them back until
// net.release(). playRefuse makes the next play() refuse with it.
// Every url fetched lands in calls, every song the page started
// playing in played, and every record written to the device's store
// in puts. Date.now ticks once per call so two songs are never heard
// in the same instant. Storage (kv) outlives reload(), as the
// browser's does.
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
		"pfNothingNew", "pfNextDownload", "pfNextId", "heardMark", "loadHeard", "heardMerged", "saveHeard",
		"clearHeard", "pfMarkHeard", "pfUnhear", "tossedStored", "loadTossed", "saveTossed", "clearTossed", "pfToss",
		"pfAdoptRecords", "pfAbortQueue", "pfRefreshQueue", "pfTakeListing", "pfEpochChanged",
		"pfEnsureDownloads", "pfPlay", "pfStatus", "poll"} {
		fns = append(fns, jsFunction(t, src, name))
	}
	// The caps on the heard and toss lists are the app's own numbers,
	// not copies.
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
var queueFetchTimeout = 30, trackFetchTimeout = 200, warmDepth = 3, pfStallMs = 8000, firstTrackPollMs = 20;
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
var puts = [];
function idbStore() { return { put: function (rec) { puts.push(rec); return {}; }, get: function () { return {}; }, "delete": function () { return {}; }, getAll: function () { return {}; }, clear: function () { return {}; } }; }
function idbReq() { return Promise.resolve(null); }
var URL = { createObjectURL: function (blob) { return "blob:" + (blob && blob.id || "x"); }, revokeObjectURL: function () {} };
var played = [];
var playRefuse = null;
var els = [];
function pfEl(i) {
  if (!els[i]) els[i] = { src: "", ended: false, currentTime: 0, loop: false, onended: null, ontimeupdate: null,
    play: function () {
      played.push(pf.playingId);
      if (playRefuse) { var e = playRefuse; playRefuse = null; return Promise.reject(e); }
      return Promise.resolve();
    } };
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
var net = { epoch: 1, tag: '"1-1"', rows: [], hashes: {}, queueDown: false, queueHold: false, bodyFail: false, bodyHold: false,
  stateDown: false, trackDown: false, trackHold: false, held: [], lastTag: null };
// release lets every answer held back go out.
net.release = function () { var h = net.held; net.held = []; h.forEach(function (go) { go(); }); };
function hashOf(id) { return net.hashes[id] || "h-" + id; }
var calls = [];
function headersOf(h) { return { get: function (k) { return h[k] || null; } }; }
function reply(status, body, h) {
  return { status: status, ok: status >= 200 && status < 300, headers: headersOf(h || {}),
    json: function () { return Promise.resolve(body); },
    blob: function () { return Promise.resolve({ id: body && body.id, size: 1000 }); } };
}
function refuse() { return Promise.reject(new TypeError("Failed to fetch")); }
// hold answers when net.release() is called, or refuses when the
// request is aborted, whichever comes first.
function hold(signal) {
  return new Promise(function (resolve, reject) {
    net.held.push(resolve);
    if (signal) signal.addEventListener("abort", function () { var e = new Error("aborted"); e.name = "AbortError"; reject(e); });
  });
}
function fetch(url, opts) {
  var u = String(url);
  var signal = opts && opts.signal;
  calls.push(u);
  if (u.indexOf("/state") === 0) {
    return net.stateDown ? refuse() : Promise.resolve(reply(200, { epoch: net.epoch, session: "s", saved_ids: [] }));
  }
  if (u.indexOf("/api/queue") === 0) {
    if (net.queueDown) return refuse();
    if (net.queueHold) return hold(signal);
    net.lastTag = (opts && opts.headers && opts.headers["If-None-Match"]) || null;
    if (net.lastTag && net.lastTag === net.tag) return Promise.resolve(reply(304, null, { ETag: net.tag }));
    var tracks = net.rows.map(function (r) {
      return { id: r.id, title: "Song " + r.id, duration_s: 100, taken: r.taken, url: "/queue/" + r.id + ".mp3", hash: hashOf(r.id) };
    });
    var body = { epoch: net.epoch, tracks: tracks };
    var r = reply(200, body, { ETag: net.tag });
    // The headers landed; the body is what the link does to it.
    if (net.bodyFail) r.json = function () { return Promise.reject(new TypeError("terminated")); };
    else if (net.bodyHold) r.json = function () { return hold(signal).then(function () { return body; }); };
    return Promise.resolve(r);
  }
  if (/\.mp3$/.test(u)) {
    if (net.trackDown) return refuse();
    var song = reply(200, { id: u.replace(/^\/queue\//, "").replace(/\.mp3$/, "") });
    if (net.trackHold) return hold(signal).then(function () { return song; });
    return Promise.resolve(song);
  }
  if (/\.json$/.test(u)) {
    var id = u.replace(/^\/queue\//, "").replace(/\.json$/, "");
    return Promise.resolve(reply(200, { id: id, prompt: "words of " + id, lyrics: "la la", hash: hashOf(id) }));
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
    offline: false, seen: {}, wrapped: false, wantPlay: false, loop: false, storeFull: false,
    tossed: {}, bad: {}, playWhy: "", lastAdvance: 0, nudged: false, skipped: 0, switchOnDownload: false };
  loadTossed();
  pf.seen = loadHeard();
  els = []; playRefuse = null;
  calls = []; played = []; puts = []; statuses = [];
}
function bank(list) {
  list.split(/\s+/).filter(Boolean).forEach(function (id) {
    pf.have[id] = { url: "blob:" + id, title: "Song " + id, dur: 100, hash: hashOf(id), prompt: "" };
  });
}
// records are what the device's store hands back on a reload.
function records(list) {
  return list.split(/\s+/).filter(Boolean).map(function (id) {
    return { id: id, blob: { id: id }, title: "Song " + id, dur: 100, hash: hashOf(id), prompt: "", saved: Date.now() };
  });
}
function downloaded() {
  return calls.filter(function (u) { return /\.mp3$/.test(u); }).map(function (u) { return u.replace(/^\/queue\//, "").replace(/\.mp3$/, ""); });
}
function listingCalls() { return calls.filter(function (u) { return u.indexOf("/api/queue") === 0; }).length; }
function heardStored() { return Object.keys(store.get("iar.heard", {})).sort(); }
function tossedKeys() { return Object.keys(store.get("iar.tossed", {})).sort(); }
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
// taken, and while it is on its way the status line says 0 ahead
// without the "nothing new" note - something new is coming; the
// replay playing is not cut short, the new song is what plays next,
// and the note stays gone.
func TestPhoneReplaysOnlyUntilSomethingNewLands(t *testing.T) {
	var got struct {
		Offline      bool     `json:"offline"`
		Replay       string   `json:"replay"`
		Wrapped      bool     `json:"wrapped"`
		NothingNew   bool     `json:"nothingNew"`
		Status       string   `json:"status"`
		WrappedMid   bool     `json:"wrappedMid"`
		NothingMid   bool     `json:"nothingMid"`
		StatusMid    string   `json:"statusMid"`
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
  // The listing lands and F's download starts; it is held back so
  // the status line can be read while the new song is on its way.
  net.trackHold = true;
  pfRefreshQueue();
  return wait(20);
}).then(function () {
  out.wrappedMid = pf.wrapped;
  out.nothingMid = pfNothingNew();
  statuses = []; pfStatus(); out.statusMid = statuses.join(" | ");
  net.trackHold = false; net.release();
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
	if !got.WrappedMid || got.NothingMid || !strings.Contains(got.StatusMid, "0 ahead") || strings.Contains(got.StatusMid, "nothing new") {
		t.Errorf("with F on its way: wrapped %v, nothing new %v, status %q; want a replay, something new coming, and 0 ahead without the replay note",
			got.WrappedMid, got.NothingMid, got.StatusMid)
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
// every song start is written to the heard list with the song's hash,
// a start the browser refused is taken back off it, a skip and a
// flush take from it, and the device-bank note and the status line
// follow the same rule for the replay note.
func TestPhoneHeardListIsWiredThroughPlaySkipAndFlush(t *testing.T) {
	raw, err := os.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for fn, wants := range map[string][]string{
		"pfPlay":         {"pfMarkHeard(id, rec.hash)", "pfUnhear(id)"},
		"pfToss":         {"pfUnhear(id)"},
		"pfJumpLive":     {"clearHeard()"},
		"pfEpochChanged": {"clearHeard()"},
		"pfShowMinutes":  {"pfNothingNew()"},
		"pfStatus":       {"pfNothingNew()"},
	} {
		for _, want := range wants {
			if !strings.Contains(jsFunction(t, src, fn), want) {
				t.Errorf("%s does not call %s", fn, want)
			}
		}
	}
	for _, fn := range []string{"pfRefreshQueue", "pfTakeListing"} {
		if strings.Contains(jsFunction(t, src, fn), "row.prompt") {
			t.Errorf("%s still reads a prompt off the listing, which no longer carries one", fn)
		}
	}
}

// A listing's tag names the rows it came with, so it is adopted with
// them and not before. The headers of a listing land and the link
// cuts the body off: the device holds no rows, so it holds no tag,
// and the next check asks for the whole listing again. Adopted on the
// headers, the tag had every later check answered "unchanged" over
// rows the device never received - on a fresh tab, none - for as
// long as the store stood still, which with the engine asleep is all
// day. The deadline covers the body too, and the slot is held until
// the body is in: a body that stalls is cut, not left hanging beside
// the next check.
func TestPhoneAdoptsAListingTagOnlyWithItsRows(t *testing.T) {
	var got struct {
		RowsCut      int      `json:"rowsCut"`
		TagCut       string   `json:"tagCut"`
		SentTag      *string  `json:"sentTag"`
		Rows         int      `json:"rows"`
		Tag          string   `json:"tag"`
		Downloaded   []string `json:"downloaded"`
		InFlight     int      `json:"inFlight"`
		HeldCtrl     bool     `json:"heldCtrl"`
		RowsStalled  int      `json:"rowsStalled"`
		TagStalled   string   `json:"tagStalled"`
		SentTagAfter *string  `json:"sentTagAfter"`
		RowsAfter    int      `json:"rowsAfter"`
		TagAfter     string   `json:"tagAfter"`
	}
	phoneBank(t, `
listing("A* B C"); net.tag = '"1-7"';
reload(); depthNow = 1; pollOkAt = Date.now();
// The headers land; the body is cut off.
net.bodyFail = true;
pfRefreshQueue();
wait(20).then(function () {
  out.rowsCut = pf.rows.length;
  out.tagCut = pf.queueTag;
  net.bodyFail = false; calls = []; net.lastTag = "never asked";
  pfRefreshQueue();
  return wait(20);
}).then(function () {
  out.sentTag = net.lastTag;
  out.rows = pf.rows.length;
  out.tag = pf.queueTag;
  out.downloaded = downloaded();
  // The store changes, and the body of the new listing stalls. A
  // second check meanwhile does not start a second listing.
  net.tag = '"1-8"'; listing("A* B C D"); net.bodyHold = true; calls = [];
  pfRefreshQueue();
  pfRefreshQueue();
  return wait(10);
}).then(function () {
  out.inFlight = listingCalls();
  out.heldCtrl = !!pf.queueCtrl;
  // The deadline cuts the stalled body.
  return wait(queueFetchTimeout + 20);
}).then(function () {
  out.rowsStalled = pf.rows.length;
  out.tagStalled = pf.queueTag;
  net.bodyHold = false; net.release(); calls = [];
  pfRefreshQueue();
  return wait(20);
}).then(function () {
  out.sentTagAfter = net.lastTag;
  out.rowsAfter = pf.rows.length;
  out.tagAfter = pf.queueTag;
  finish();
});
`, &got)
	if got.RowsCut != 0 || got.TagCut != "" {
		t.Fatalf("after a listing whose body was cut off: %d rows under tag %q; want none, and no tag to hold", got.RowsCut, got.TagCut)
	}
	if got.SentTag != nil {
		t.Errorf("the check after a cut-off body sent If-None-Match %q; want no tag, so the listing comes whole", *got.SentTag)
	}
	if got.Rows != 3 || got.Tag != `"1-7"` || len(got.Downloaded) == 0 {
		t.Errorf("the check after a cut-off body gave %d rows under %q and took %v; want 3 rows under \"1-7\" and a download", got.Rows, got.Tag, got.Downloaded)
	}
	if got.InFlight != 1 || !got.HeldCtrl {
		t.Errorf("with a listing's body still coming, %d listings were in flight (slot held %v); want 1, held", got.InFlight, got.HeldCtrl)
	}
	if got.RowsStalled != 3 || got.TagStalled != `"1-7"` {
		t.Errorf("after a stalled body was cut: %d rows under %q; want the 3 rows held, under their own tag \"1-7\"", got.RowsStalled, got.TagStalled)
	}
	if got.SentTagAfter == nil || *got.SentTagAfter != `"1-7"` || got.RowsAfter != 4 || got.TagAfter != `"1-8"` {
		sent := "<none>"
		if got.SentTagAfter != nil {
			sent = *got.SentTagAfter
		}
		t.Errorf("the next check sent %s and got %d rows under %q; want the held tag \"1-7\", 4 rows, \"1-8\"", sent, got.RowsAfter, got.TagAfter)
	}
}

// A song the browser refused to start - which it does on a reload
// until the page has seen a tap - made no sound and was not heard.
// The mark set when it was picked comes off, so the tap that follows
// starts that song rather than passing it over for good. A copy that
// would not play is not heard either: the device moves on to the
// next unheard song, and the bad copy stays off the list.
func TestPhoneDoesNotCountARefusedStartAsHeard(t *testing.T) {
	var got struct {
		Heard         []string `json:"heard"`
		Status        string   `json:"status"`
		Next          string   `json:"next"`
		Ready         int      `json:"ready"`
		HeardAfterBad []string `json:"heardAfterBad"`
		Bad           bool     `json:"bad"`
		PlayingAfter  string   `json:"playingAfter"`
	}
	phoneBank(t, `
listing("F* G* H*");
reload(); bank("F G H"); depthNow = 0;
var refused = new Error("play() failed because the user didn't interact with the document first.");
refused.name = "NotAllowedError";
playRefuse = refused;
pfPlay("F");
wait(10).then(function () {
  out.heard = heardStored();
  out.status = statuses.join(" | ");
  // The tap: the bank starts over from what is unheard.
  pf.active = true; pf.playingId = null;
  out.next = pfNextId(null);
  out.ready = pfReady();
  // A copy that will not play: the device moves on to F, the unheard one.
  playRefuse = new Error("The media resource was not suitable.");
  pfPlay("G");
  return wait(10);
}).then(function () {
  out.heardAfterBad = heardStored();
  out.bad = !!pf.bad.G;
  out.playingAfter = pf.playingId;
  finish();
});
`, &got)
	if len(got.Heard) != 0 || !strings.Contains(got.Status, "tap play") {
		t.Errorf("after the browser refused to start F: heard %v, status %q; want nothing heard and a word about the tap", got.Heard, got.Status)
	}
	if got.Next != "F" || got.Ready != 3 {
		t.Errorf("on the tap the bank starts with %q and counts %d ready; want F, the song never heard, and 3", got.Next, got.Ready)
	}
	if join(got.HeardAfterBad) != "F" || !got.Bad || got.PlayingAfter != "F" {
		t.Errorf("after G's copy refused to play: heard %v, G bad %v, playing %q; want only F heard, G marked bad, and F playing", got.HeardAfterBad, got.Bad, got.PlayingAfter)
	}
}

// Two tabs of the page share one heard list. A tab the browser froze
// and later thawed writes with a stale memory; its write adds what it
// heard to what the other tab heard rather than wiping that out, and
// it learns the other's hearings as it goes. A skip holds against the
// skipping tab's own later writes; a steer in one tab empties the
// list for both, and what the other tab heard before the steer stays
// gone when it writes again. The toss list is shared the same way.
func TestPhoneTabsShareOneHeardList(t *testing.T) {
	var got struct {
		BLoaded        []string `json:"bLoaded"`
		Stored         []string `json:"stored"`
		AKnows         []string `json:"aKnows"`
		AfterUnhear    []string `json:"afterUnhear"`
		AfterClear     []string `json:"afterClear"`
		AfterStale     []string `json:"afterStale"`
		Reloaded       []string `json:"reloaded"`
		Tossed         []string `json:"tossed"`
		TossedReloaded []string `json:"tossedReloaded"`
	}
	phoneBank(t, `
// Tab A hears three songs.
reload(); var A = pf;
["s1", "s2", "s3"].forEach(function (id) { pfMarkHeard(id, "h-" + id); });
// Tab B opens, reads them, and hears two more.
reload(); var B = pf;
out.bLoaded = Object.keys(pf.seen).sort();
pfMarkHeard("s4", "h-s4"); pfMarkHeard("s5", "h-s5");
// Tab A, thawed with the memory it froze with, hears another.
pf = A; pfMarkHeard("s6", "h-s6");
out.stored = heardStored();
out.aKnows = Object.keys(pf.seen).sort();
// A skips s2, then hears s7: the skip holds.
pfUnhear("s2"); pfMarkHeard("s7", "h-s7");
out.afterUnhear = heardStored();
// B steers: the list is emptied. A, still holding its old memory, hears s8.
pf = B; pf.epoch = 1; pfEpochChanged(2);
out.afterClear = heardStored();
pf = A; pfMarkHeard("s8", "h-s8");
out.afterStale = heardStored();
// A fresh tab reads the one list.
reload();
out.reloaded = Object.keys(pf.seen).sort();
// Tosses: each tab skips a song of its own.
pf = A; pfToss("x1", "Song x1");
pf = B; pfToss("x2", "Song x2");
out.tossed = tossedKeys();
reload();
out.tossedReloaded = Object.keys(pf.tossed).sort();
finish();
`, &got)
	if join(got.BLoaded) != "s1 s2 s3" {
		t.Fatalf("a second tab read %v as heard, want s1 s2 s3", got.BLoaded)
	}
	if join(got.Stored) != "s1 s2 s3 s4 s5 s6" || join(got.AKnows) != "s1 s2 s3 s4 s5 s6" {
		t.Errorf("after the thawed tab wrote: storage %v, its memory %v; want s1 to s6 in both - nothing the other tab heard is lost", got.Stored, got.AKnows)
	}
	if join(got.AfterUnhear) != "s1 s3 s4 s5 s6 s7" {
		t.Errorf("after the tab unheard s2 and heard s7: %v, want s1 s3 s4 s5 s6 s7", got.AfterUnhear)
	}
	if len(got.AfterClear) != 0 || join(got.AfterStale) != "s8" || join(got.Reloaded) != "s8" {
		t.Errorf("after a steer in one tab: %v; after the other wrote with its old memory: %v; a fresh tab read %v; want nothing, s8, s8",
			got.AfterClear, got.AfterStale, got.Reloaded)
	}
	if join(got.Tossed) != "x1 x2" || join(got.TossedReloaded) != "x1 x2" {
		t.Errorf("two tabs each skipped a song: storage %v, a fresh tab read %v; want x1 x2 in both", got.Tossed, got.TossedReloaded)
	}
}

// A bank from before the page kept a heard list counts as heard: the
// first time such a bank is read, every song in it is marked, so the
// device takes new songs at once rather than replaying the bank from
// the top until enough of it has gone by. The list exists from then
// on, so songs banked afterwards and not yet played are not marked
// the next time the bank is read - with an empty bank as much as a
// full one.
func TestPhoneCountsABankOlderThanItsMemoryAsHeard(t *testing.T) {
	var got struct {
		KeyBefore     bool     `json:"keyBefore"`
		Seeded        []string `json:"seeded"`
		HashKept      string   `json:"hashKept"`
		First         string   `json:"first"`
		Wrapped       bool     `json:"wrapped"`
		Ahead         int      `json:"ahead"`
		Downloaded    []string `json:"downloaded"`
		Later         []string `json:"later"`
		KeyAfterEmpty bool     `json:"keyAfterEmpty"`
		EmptyThen     []string `json:"emptyThen"`
	}
	phoneBank(t, `
listing("A* B* C* D* E* F G H");
reload(); depthNow = 3;
out.keyBefore = "iar.heard" in kv;
pfAdoptRecords(records("A B C D E"));
out.seeded = heardStored();
out.hashKept = pf.seen.A ? pf.seen.A.hash : "";
var first = pfNextId(null);
out.first = first; out.wrapped = pf.wrapped;
pfPlay(first);
pfRefreshQueue();
wait(80).then(function () {
  out.ahead = pfAhead();
  out.downloaded = downloaded();
  // The list exists now: a song banked since and not played is not heard.
  reload(); depthNow = 3;
  pfAdoptRecords(records("A B C D E F"));
  out.later = heardStored();
  // A page whose first bank is empty remembers from then on.
  kv = {}; reload();
  pfAdoptRecords([]);
  out.keyAfterEmpty = "iar.heard" in kv;
  pfAdoptRecords(records("G"));
  out.emptyThen = heardStored();
  finish();
});
`, &got)
	if got.KeyBefore {
		t.Fatal("the heard list existed before the bank was read")
	}
	if join(got.Seeded) != "A B C D E" || got.HashKept != "h-A" {
		t.Errorf("reading a bank older than the heard list marked %v (A's hash %q); want A B C D E, with their hashes", got.Seeded, got.HashKept)
	}
	if !got.Wrapped || got.First != "A" {
		t.Errorf("the first pick was %q (wrapped %v); want a replay of A", got.First, got.Wrapped)
	}
	if got.Ahead != 3 || join(got.Downloaded) != "F G H" {
		t.Errorf("ahead = %d after taking %v; want 3 and F G H - the first new songs, at once", got.Ahead, got.Downloaded)
	}
	if join(got.Later) != "A B C D E" {
		t.Errorf("a later reading of the bank left the heard list at %v, want A B C D E - F was banked unheard", got.Later)
	}
	if !got.KeyAfterEmpty || len(got.EmptyThen) != 0 {
		t.Errorf("a page that first read an empty bank: list kept %v, later marked %v; want the list kept and nothing marked", got.KeyAfterEmpty, got.EmptyThen)
	}
}

// Ids are reissued: a store emptied without a steer starts its count
// over, so a song made next week wears the id of one heard last week.
// The heard list keeps each song's hash, and a listed song whose hash
// is not the one heard under its id is unheard - banked or not - so
// the new song is taken. The songs that are what they were stay heard.
func TestPhoneUnhearsAnIdReissuedToADifferentSong(t *testing.T) {
	var got struct {
		HeardBefore []string `json:"heardBefore"`
		HeardAfter  []string `json:"heardAfter"`
		Downloaded  []string `json:"downloaded"`
		Ahead       int      `json:"ahead"`
		Ready       int      `json:"ready"`
		AHash       string   `json:"aHash"`
	}
	phoneBank(t, `
listing("A* B* C* D* E*");
reload(); bank("A B C D E"); depthNow = 0;
["A", "B", "C", "D", "E"].forEach(function (id) { pfPlay(id); });
wait(10).then(function () {
  // The bank was trimmed to D and E; the store was emptied and
  // refilled, A to C made anew under the same ids, D and E kept.
  reload(); bank("D E"); depthNow = 3;
  net.hashes = { A: "h2-A", B: "h2-B", C: "h2-C" };
  listing("A B C D* E*");
  out.heardBefore = heardStored();
  pfRefreshQueue();
  return wait(80);
}).then(function () {
  out.heardAfter = heardStored();
  out.downloaded = downloaded();
  out.ahead = pfAhead();
  out.ready = pfReady();
  out.aHash = pf.have.A ? pf.have.A.hash : "";
  finish();
});
`, &got)
	if join(got.HeardBefore) != "A B C D E" {
		t.Fatalf("heard before the store was refilled: %v, want A B C D E", got.HeardBefore)
	}
	if join(got.HeardAfter) != "D E" {
		t.Errorf("after the listing reissued A B C to new songs the heard list is %v, want D E", got.HeardAfter)
	}
	if join(got.Downloaded) != "A B C" || got.AHash != "h2-A" {
		t.Errorf("the phone took %v (A's hash %q); want A B C, the new songs, under their new hashes", got.Downloaded, got.AHash)
	}
	if got.Ahead != 3 || got.Ready != 3 {
		t.Errorf("ahead = %d, ready = %d; want 3 and 3 - the new songs count, the heard ones do not", got.Ahead, got.Ready)
	}
}

// A song heard here is never taken again while the radio lists it,
// even once the bank has let it go: the radio's take of a song
// already taken is nothing, and the download would be a repeat. With
// nothing playing the downloader reads the listing from the top,
// which is where the heard-and-trimmed songs are.
func TestPhoneNeverTakesASongItHeardButNoLongerHolds(t *testing.T) {
	var got struct {
		Downloaded []string `json:"downloaded"`
		NewRow     string   `json:"newRow"`
	}
	phoneBank(t, `
listing("A* B* C* D* E* F G");
reload(); bank("A B C D E"); depthNow = 0;
["A", "B", "C", "D", "E"].forEach(function (id) { pfPlay(id); });
wait(10).then(function () {
  // The bank was trimmed to C D E; the heard list outlives the trim.
  reload(); bank("C D E"); depthNow = 3;
  var row = pfNewRow();
  out.newRow = row ? row.id : "";
  pfRefreshQueue();
  return wait(80);
}).then(function () {
  out.downloaded = downloaded();
  finish();
});
`, &got)
	if join(got.Downloaded) != "F G" {
		t.Errorf("with A and B heard but no longer held the phone took %v, want F G and never A or B", got.Downloaded)
	}
}

// A device waiting for the radio's very first song asks for the
// listing every couple of seconds rather than every ten. An empty
// store's listing has a tag like any other, so the fast checks are
// answered "unchanged" - and the next fast check is armed all the
// same, until a song lands and plays.
func TestPhoneKeepsAskingFastForItsFirstSong(t *testing.T) {
	var got struct {
		Armed      bool     `json:"armed"`
		FirstRows  int      `json:"firstRows"`
		Calls      int      `json:"calls"`
		SentTag    string   `json:"sentTag"`
		Rearmed    bool     `json:"rearmed"`
		CallsAfter int      `json:"callsAfter"`
		Rows       int      `json:"rows"`
		Played     []string `json:"played"`
	}
	phoneBank(t, `
listing(""); net.tag = '"1-0"';
reload(); depthNow = 3; pf.wantPlay = true;
pfRefreshQueue();
wait(10).then(function () {
  out.armed = !!pf.fetchTimer;
  out.firstRows = pf.rows.length;
  return wait(firstTrackPollMs + 10);
}).then(function () {
  out.calls = listingCalls();
  out.sentTag = net.lastTag;
  out.rearmed = !!pf.fetchTimer;
  // The radio's first song lands.
  net.tag = '"1-1"'; listing("A");
  return wait(firstTrackPollMs + 40);
}).then(function () {
  out.callsAfter = listingCalls();
  out.rows = pf.rows.length;
  out.played = played.slice();
  finish();
});
`, &got)
	if !got.Armed || got.FirstRows != 0 {
		t.Fatalf("after an empty listing: fast check armed %v, %d rows; want armed and none", got.Armed, got.FirstRows)
	}
	if got.Calls < 2 || got.SentTag != `"1-0"` {
		t.Fatalf("the fast check made %d listings in all and sent %q; want a second one, with the tag held", got.Calls, got.SentTag)
	}
	if !got.Rearmed {
		t.Errorf("after the fast check was answered unchanged no further fast check was armed")
	}
	if got.CallsAfter < 3 || got.Rows != 1 || join(got.Played) != "A" {
		t.Errorf("once a song landed: %d listings, %d rows, played %v; want a third check, the row, and A playing", got.CallsAfter, got.Rows, got.Played)
	}
}
