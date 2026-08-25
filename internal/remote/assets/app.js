(function () {
  "use strict";
  var $ = function (id) { return document.getElementById(id); };
  var store = {
    get: function (k, d) { try { var v = localStorage.getItem(k); return v === null ? d : JSON.parse(v); } catch (e) { return d; } },
    set: function (k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch (e) {} }
  };
  var headers = { "Content-Type": "application/x-www-form-urlencoded", "X-IAR-Remote": "1" };

  // ---- permissions ------------------------------------------------
  var me = null;
  function applyPerms() {
    var perms = { steer: !!me.steer, new_prompt: !!me.new_prompt, save: !!me.save };
    Array.prototype.forEach.call(document.querySelectorAll("[data-perm]"), function (el) {
      el.hidden = !perms[el.getAttribute("data-perm")];
    });
    $("steercard").hidden = !(perms.steer || perms.new_prompt);
  }
  function loggedOut() {
    $("conn").textContent = "logged out";
    if (pf.active) stopBuffered("logged out - reload this page to log in again", "bad");
    else stopStream("logged out - reload this page to log in again", "bad");
  }
  fetch("/me").then(function (r) {
    if (r.status === 401) { loggedOut(); return null; }
    return r.json();
  }).then(function (d) { if (d) { me = d; applyPerms(); loadSessions(); } }).catch(function () {});

  // ---- per-card statuses ------------------------------------------
  function setStatus(el, text, cls) {
    if (!el) return;
    el.textContent = text || "";
    el.className = "cardstatus" + (cls ? " " + cls : "");
    if (el._clear) { clearTimeout(el._clear); el._clear = null; }
    if (text) {
      el._clear = setTimeout(function () {
        el.textContent = "";
        el.className = "cardstatus";
      }, cls === "err" ? 10000 : 6000);
    }
  }

  // act posts one mutating action with honest button feedback: the
  // button disables itself while the request is in flight, an immediate
  // optimistic status appears next to it, failures show in a distinct
  // colour, and double-taps are no-ops.
  function act(btn, statusEl, path, body, pending) {
    if (btn && btn.disabled) { return Promise.resolve(null); }
    if (btn) { btn.disabled = true; btn.setAttribute("aria-busy", "true"); }
    setStatus(statusEl, pending || "working…", "");
    return fetch(path, { method: "POST", headers: headers, body: body || "" })
      .then(function (r) {
        if (r.status === 401) { loggedOut(); throw new Error("logged out"); }
        return r.json().catch(function () { return {}; }).then(function (d) {
          if (!r.ok) { throw new Error(d.error || d.ack || ("error " + r.status)); }
          return d;
        });
      })
      .then(function (d) {
        setStatus(statusEl, d.ack || "done", "ok");
        return d;
      })
      .catch(function (e) {
        setStatus(statusEl, "failed: " + e.message, "err");
        return null;
      })
      .finally(function () {
        if (btn) { btn.disabled = false; btn.removeAttribute("aria-busy"); }
      });
  }

  // ---- mode switch -------------------------------------------------
  var mode = store.get("iar.mode", "live");
  function setMode(m) {
    mode = m;
    store.set("iar.mode", m);
    var live = m === "live";
    $("live").hidden = !live;
    $("saved").hidden = live;
    $("mode-live").classList.toggle("on", live);
    $("mode-saved").classList.toggle("on", !live);
    $("mode-live").setAttribute("aria-selected", live ? "true" : "false");
    $("mode-saved").setAttribute("aria-selected", live ? "false" : "true");
    if (live) {
      savedAudio.pause();
    } else {
      stopListening("stopped (switched to saved chunks)");
      loadChunks();
    }
    syncPrevAction();
  }
  $("mode-live").addEventListener("click", function () { setMode("live"); });
  $("mode-saved").addEventListener("click", function () { setMode("saved"); });

  // ---- live stream with automatic recovery -------------------------
  // wantStream is the user's intent. While it holds, every non-auth
  // failure (element error, clean end from a server-side drop, a stall,
  // a rejected play) reconnects itself with exponential backoff,
  // forever; connectivity changes retry immediately. Only an expired
  // login stops the stream for good.
  var audio = null;
  var wantStream = false;
  var watchdog = null;
  var retryTimer = null;
  var retryDelay = 500; // ms, doubles up to 12s
  var attempts = 0;
  var lastTime = -1;
  var lastAdvance = 0;
  var playBtn = $("play");
  var stateEl = $("streamstate");

  function streamState(text, cls) {
    stateEl.textContent = text;
    stateEl.className = cls || "";
  }

  // ---- system-pause detection (car off, route loss) ----------------
  // Every intentional pause goes through quiet(), which marks the
  // element; a pause event WITHOUT the mark is the system's doing
  // (Bluetooth route gone, car off). Then the element is kept exactly
  // as-is - an intact paused element is what keeps the media
  // notification alive and targetable - and playback resumes on an
  // explicit signal: the car's play command, the page becoming
  // visible, or a tap. Never on a timer (a timed play() would blast
  // the phone speaker in a pocket).
  var resumePending = false;
  var carResume = store.get("iar.carresume", true) !== false;

  function quiet(el) {
    if (el && !el.paused) el._appPause = true;
    if (el) el.pause();
  }

  function onSystemPause(e) {
    var el = e.target;
    if (el._appPause) { el._appPause = false; return; }
    if (el.ended) return;
    if (pf.active) { if (el !== pf.els[pf.cur]) return; }
    else if (el !== audio) return;
    if (!(wantStream || pf.active)) return;
    resumePending = true;
    mediaPlaybackState("paused");
    applyMediaMetadata();
    streamState("paused: audio output disconnected - resumes when your car asks to play, or tap play", "bad");
  }

  // tryResume plays the kept element in place: no re-setup, no rewind,
  // no new timers.
  function tryResume() {
    if (!resumePending) return false;
    var el = pf.active ? pf.els[pf.cur] : audio;
    if (!el) { resumePending = false; return false; }
    resumePending = false;
    el.play().then(function () {
      mediaPlaybackState("playing");
      applyMediaMetadata();
      if (pf.active) pfStatus();
    })["catch"](function () {
      resumePending = true;
      streamState("tap play to resume", "bad");
    });
    return true;
  }

  function teardownAudio() {
    if (watchdog) { clearInterval(watchdog); watchdog = null; }
    if (audio) {
      audio.onerror = null;
      audio.onended = null;
      quiet(audio);
      audio.removeAttribute("src");
      audio.load();
      if (audio.parentNode) audio.parentNode.removeChild(audio);
      audio = null;
    }
  }

  function stopStream(msg, cls) {
    wantStream = false;
    resumePending = false;
    store.set("iar.wasplaying", false);
    if (retryTimer) { clearTimeout(retryTimer); retryTimer = null; }
    retryDelay = 500;
    attempts = 0;
    teardownAudio();
    playBtn.classList.remove("playing");
    playBtn.innerHTML = "&#9654;&#xFE0E; Play the stream";
    streamState(msg || "", cls || "");
    mediaPlaybackState("none");
  }

  // streamFailed classifies a failure: an expired login is terminal,
  // everything else schedules a reconnect.
  function streamFailed(reason) {
    if (!wantStream) return;
    teardownAudio();
    fetch("/state").then(function (r) {
      if (r.status === 401 || r.status === 403) {
        stopStream("session expired - reload this page and log in again", "bad");
        $("conn").textContent = "logged out";
      } else {
        scheduleReconnect(reason);
      }
    }).catch(function () { scheduleReconnect(reason); });
  }

  function scheduleReconnect(reason) {
    if (!wantStream || retryTimer) return;
    attempts++;
    streamState("reconnecting… (attempt " + attempts + (reason ? ", " + reason : "") + ")", "bad");
    retryTimer = setTimeout(function () {
      retryTimer = null;
      connectStream();
    }, retryDelay);
    retryDelay = Math.min(retryDelay * 2, 12000);
  }

  // A connectivity change ends any backoff wait right away, and tears
  // a stalled element down for an immediate reconnect instead of
  // waiting for the watchdog.
  function retryNow() {
    if (!wantStream || resumePending) return;
    if (retryTimer) { clearTimeout(retryTimer); retryTimer = null; }
    if (audio) {
      if (audio.paused) return; // paused is not stalled; never restart over it
      if (Date.now() - lastAdvance < 3000) return; // playback is progressing
      teardownAudio();
    }
    connectStream();
  }
  window.addEventListener("online", retryNow);
  // Becoming visible means the user is looking at the phone: resume a
  // pending system pause (toggle permitting), otherwise treat it as a
  // connectivity nudge.
  function onVisible() {
    if (document.hidden) return;
    if (resumePending && carResume) { tryResume(); return; }
    retryNow();
  }
  document.addEventListener("visibilitychange", onVisible);
  document.addEventListener("resume", onVisible);
  if (navigator.connection && navigator.connection.addEventListener) {
    navigator.connection.addEventListener("change", retryNow);
  }

  function connectStream() {
    if (!wantStream || audio) return;
    streamState(attempts > 0 ? "reconnecting… (attempt " + attempts + ")" : "connecting…", "");
    audio = new Audio("/stream.mp3?t=" + Date.now());
    audio.id = "liveaudio";
    document.body.appendChild(audio);
    audio.onerror = function () { streamFailed("stream error"); };
    // A server-side drop ends the response cleanly; recover from that
    // exactly like an error.
    audio.onended = function () { streamFailed("stream ended"); };
    audio.addEventListener("pause", onSystemPause);
    audio.addEventListener("stalled", function () { streamState("stalled - waiting for data…", "bad"); });
    audio.addEventListener("waiting", function () { streamState("buffering…", ""); });
    audio.addEventListener("playing", function () { streamState("receiving audio…", ""); mediaPlaybackState("playing"); });
    audio.play().then(function () {
      playBtn.classList.add("playing");
      playBtn.innerHTML = "&#9632;&#xFE0E; Stop listening";
      applyMediaMetadata();
      mediaPlaybackState("playing");
    }).catch(function (e) {
      if (e && e.name === "NotAllowedError") {
        stopStream("tap play to start audio", "bad");
        return;
      }
      streamFailed("could not start audio");
    });
    // Honest progress: only report playing while currentTime advances.
    lastTime = -1;
    lastAdvance = Date.now();
    watchdog = setInterval(function () {
      if (!audio) return;
      // A paused element is not "no audio arriving": after a route loss
      // (car off) the phone pauses playback, and tearing down here
      // would restart the stream on the phone's loudspeaker.
      if (audio.paused) return;
      var t = audio.currentTime;
      if (t > lastTime + 0.2) {
        lastTime = t;
        lastAdvance = Date.now();
        retryDelay = 500;
        attempts = 0;
        streamState("playing · " + Math.floor(t) + "s", "good");
      } else if (Date.now() - lastAdvance > 8000) {
        streamFailed("no audio arriving");
      }
    }, 2000);
  }

  function startStream() {
    if (wantStream) return;
    wantStream = true;
    retryDelay = 500;
    attempts = 0;
    connectStream();
  }

  // ---- buffered (prefetched) playback ------------------------------
  // Instead of the realtime stream, download whole upcoming tracks into
  // IndexedDB and play them through two alternating audio elements, so
  // minutes of audio survive a network dead zone. The phone default.
  var idbSupported = typeof indexedDB !== "undefined";
  var phoneLike = false;
  try { phoneLike = window.matchMedia && window.matchMedia("(pointer: coarse)").matches; } catch (e) {}
  var transport = store.get("iar.transport", (phoneLike && idbSupported) ? "buffered" : "direct");
  if (!idbSupported) transport = "direct";

  var pf = {
    active: false,
    db: null,
    epoch: -1,
    rows: [],        // server listing, play order
    have: {},        // id -> {url (object URL), prompt, epoch, dur}
    playingId: null,
    prevId: null,    // the track this device heard before the current one
    els: [null, null],
    cur: 0,
    ctrl: null,      // AbortController of the in-flight download
    fetchTimer: null,
    queueTimer: null,
    offline: false,
    wantPlay: false  // start playback as soon as anything is stored
  };

  // Buffering level (per device): how far ahead to download and how
  // many finished tracks to keep banked.
  var bufLevel = store.get("iar.buflevel", "auto");
  if (bufLevel !== "eco" && bufLevel !== "max") bufLevel = "auto";

  function idbOpen() {
    return new Promise(function (resolve, reject) {
      var req = indexedDB.open("iar-radio", 1);
      req.onupgradeneeded = function () {
        req.result.createObjectStore("tracks", { keyPath: "id" });
      };
      req.onsuccess = function () { resolve(req.result); };
      req.onerror = function () { reject(req.error); };
    });
  }
  function idbReq(r) {
    return new Promise(function (resolve, reject) {
      r.onsuccess = function () { resolve(r.result); };
      r.onerror = function () { reject(r.error); };
    });
  }
  function idbStore(mode) { return pf.db.transaction("tracks", mode).objectStore("tracks"); }

  function pfDepth() {
    if (bufLevel === "eco") return 1;
    if (bufLevel === "max") return 16;
    var c = navigator.connection;
    if (c && c.type === "wifi" && !c.saveData) return 5;
    return 2; // cellular, save-data, or no connection API
  }

  // pfStoreCap bounds the banked tracks on this device.
  function pfStoreCap() {
    if (bufLevel === "eco") return 4;
    if (bufLevel === "max") return 18; // ~45 min at default track length
    return 8;
  }

  // pfMinutes reports how much audio is banked on this device.
  function pfMinutes() {
    var secs = 0;
    Object.keys(pf.have).forEach(function (id) { secs += pf.have[id].dur || 0; });
    return Math.round(secs / 60);
  }

  function pfShowMinutes() {
    var el = $("bufmins");
    if (!el) return;
    var n = Object.keys(pf.have).length;
    el.textContent = n ? "~" + pfMinutes() + " min banked on this device" : "";
  }

  function pfState(text, cls) { if (pf.active) streamState(text, cls); }

  // pfListedPlaying reports whether the playing track is still in the
  // server listing. Once the stream consumes it, every listed row is
  // upcoming from this device's point of view.
  function pfListedPlaying() {
    for (var i = 0; i < pf.rows.length; i++) {
      if (pf.rows[i].id === pf.playingId) return true;
    }
    return false;
  }

  function pfAhead() {
    var n = 0;
    var passed = !pfListedPlaying();
    pf.rows.forEach(function (row) {
      if (row.id === pf.playingId) { passed = true; return; }
      if (passed && pf.have[row.id]) n++;
    });
    return n;
  }

  function startBuffered() {
    if (pf.active) return;
    pf.active = true;
    playBtn.classList.add("playing");
    playBtn.innerHTML = "&#9632;&#xFE0E; Stop listening";
    pfState("preparing buffered playback…", "");
    (pf.db ? Promise.resolve(pf.db) : idbOpen().then(function (db) { pf.db = db; return db; }))
      .then(function () { return idbReq(idbStore("readonly").getAll()); })
      .then(function (recs) {
        (recs || []).forEach(function (rec) {
          if (!pf.have[rec.id]) {
            // epoch left undefined: the first queue listing decides
            // whether the record is still current and restamps it.
            pf.have[rec.id] = { url: URL.createObjectURL(rec.blob), prompt: rec.prompt, title: rec.title, subtitle: rec.subtitle, dur: rec.dur };
          }
        });
        pf.wantPlay = true;
        pfShowMinutes();
        pfRefreshQueue();
        if (pf.queueTimer) clearInterval(pf.queueTimer);
        pf.queueTimer = setInterval(function () { pfRefreshQueue(); }, 10000);
      })
      .catch(function () {
        // No usable storage: fall back to the direct stream.
        pf.active = false;
        transport = "direct";
        store.set("iar.transport", transport);
        syncTransportUI();
        startStream();
      });
  }

  function stopBuffered(msg, cls) {
    pf.active = false;
    resumePending = false;
    store.set("iar.wasplaying", false);
    if (pf.ctrl) { pf.ctrl.abort(); pf.ctrl = null; }
    if (pf.fetchTimer) { clearTimeout(pf.fetchTimer); pf.fetchTimer = null; }
    if (pf.queueTimer) { clearInterval(pf.queueTimer); pf.queueTimer = null; }
    pf.els.forEach(function (el, i) {
      if (el) {
        el.onended = null;
        quiet(el);
        el.removeAttribute("src");
        el.load();
        if (el.parentNode) el.parentNode.removeChild(el);
        pf.els[i] = null;
      }
    });
    pf.playingId = null;
    pf.wantPlay = false;
    playBtn.classList.remove("playing");
    playBtn.innerHTML = "&#9654;&#xFE0E; Play the stream";
    streamState(msg || "", cls || "");
    mediaPlaybackState("none");
  }

  function pfRefreshQueue() {
    fetch("/api/queue").then(function (r) {
      if (r.status === 401 || r.status === 403) {
        stopBuffered("session expired - reload this page and log in again", "bad");
        $("conn").textContent = "logged out";
        return null;
      }
      return r.json();
    }).then(function (q) {
      if (!q || !pf.active) return;
      pf.offline = false;
      var first = pf.epoch < 0;
      if (!first && q.epoch !== pf.epoch) {
        pfEpochChanged(q.epoch);
      }
      pf.epoch = q.epoch;
      pf.rows = q.tracks || [];
      if (first) {
        // Epoch counters are per-run: leftovers from an earlier run can
        // only be trusted if the current listing still names them.
        var listed = {};
        pf.rows.forEach(function (row) { listed[row.id] = true; });
        Object.keys(pf.have).forEach(function (id) {
          if (listed[id] || id === pf.playingId) return;
          try { URL.revokeObjectURL(pf.have[id].url); } catch (e) {}
          delete pf.have[id];
          idbReq(idbStore("readwrite")["delete"](id))["catch"](function () {});
        });
      }
      // Stamp the current epoch onto records that predate knowing it.
      Object.keys(pf.have).forEach(function (id) {
        if (pf.have[id].epoch === undefined) pf.have[id].epoch = q.epoch;
      });
      // Remember durations for the banked-minutes display.
      pf.rows.forEach(function (row) {
        if (pf.have[row.id] && !pf.have[row.id].dur) pf.have[row.id].dur = row.duration_s;
      });
      pfShowMinutes();
      pfEnsureDownloads();
      // While waiting for the very first track, poll the listing much
      // faster than the regular interval.
      if (pf.wantPlay && !pf.playingId && pf.rows.length === 0 && !pf.fetchTimer) {
        pf.fetchTimer = setTimeout(function () {
          pf.fetchTimer = null;
          pfRefreshQueue();
        }, 2000);
      }
    }).catch(function () {
      if (!pf.active) return;
      // Offline: keep playing what is stored; the interval retries.
      pf.offline = true;
      if (!pf.playingId) {
        pf.wantPlay = true;
        var id = pfNextId(null);
        if (id) pfPlay(id);
        else pfState("offline - waiting for stored tracks", "bad");
      } else {
        pfStatus();
      }
    });
  }

  // pfEpochChanged: steering changed what comes next. Abort the
  // in-flight download, drop everything from the old context except the
  // playing track, and switch to the first new-context track once it is
  // downloaded.
  function pfEpochChanged(newEpoch) {
    if (pf.ctrl) { pf.ctrl.abort(); pf.ctrl = null; }
    Object.keys(pf.have).forEach(function (id) {
      if (id === pf.playingId) return;
      if (pf.have[id].epoch !== newEpoch) {
        try { URL.revokeObjectURL(pf.have[id].url); } catch (e) {}
        delete pf.have[id];
        idbReq(idbStore("readwrite")["delete"](id))["catch"](function () {});
      }
    });
    pf.switchOnDownload = true;
  }

  // pfEnsureDownloads keeps the store filled to the level's depth,
  // sequentially, one AbortController per download. It never recurses
  // into playback: pf.wantPlay marks that playback should start as
  // soon as anything is stored, and each completed download honours it.
  function pfEnsureDownloads() {
    if (!pf.active || pf.ctrl) return;
    var starving = pf.wantPlay && !pf.playingId && pfNextId(null) === null;
    // The next row in play order that is not stored yet.
    var next = null;
    var passed = !pfListedPlaying();
    for (var i = 0; i < pf.rows.length; i++) {
      var row = pf.rows[i];
      if (row.id === pf.playingId) { passed = true; continue; }
      if (passed && !pf.have[row.id]) { next = row; break; }
    }
    if (!next || (pfAhead() >= pfDepth() && !starving)) {
      // Nothing (more) to download right now. Start playback from the
      // store when it is wanted; new rows arrive with the next listing.
      if (pf.wantPlay && !pf.playingId) {
        var ready = pfNextId(null);
        if (ready) pfPlay(ready);
      }
      return;
    }
    pf.ctrl = new AbortController();
    var row = next;
    fetch(row.url, { signal: pf.ctrl.signal }).then(function (r) {
      if (r.status === 401 || r.status === 403) { throw { auth: true }; }
      if (!r.ok) { throw new Error("track " + r.status); }
      return r.blob();
    }).then(function (blob) {
      pf.ctrl = null;
      pf.have[row.id] = { url: URL.createObjectURL(blob), prompt: row.prompt, title: row.title, subtitle: row.subtitle, epoch: pf.epoch, dur: row.duration_s };
      idbReq(idbStore("readwrite").put({ id: row.id, prompt: row.prompt, title: row.title, subtitle: row.subtitle, epoch: pf.epoch, dur: row.duration_s, blob: blob, saved: Date.now() }))["catch"](function () {});
      pfTrimStore();
      pfShowMinutes();
      if (pf.switchOnDownload) {
        pf.switchOnDownload = false;
        pfPlay(row.id);
      } else if (pf.wantPlay && !pf.playingId) {
        pfPlay(row.id);
      } else {
        pfPreloadNext();
        pfStatus();
      }
      pfEnsureDownloads();
    })["catch"](function (e) {
      pf.ctrl = null;
      if (e && e.auth) {
        stopBuffered("session expired - reload this page and log in again", "bad");
        return;
      }
      if (!pf.active) return;
      // 404 (evicted/steered away): drop the row and move on. Network
      // errors retry shortly; playback continues from storage.
      if (e && e.message && e.message.indexOf("track 4") === 0) {
        pf.rows = pf.rows.filter(function (r2) { return r2.id !== row.id; });
      }
      if (pf.wantPlay && !pf.playingId) {
        var ready = pfNextId(null);
        if (ready) pfPlay(ready);
      }
      if (!pf.fetchTimer) {
        pf.fetchTimer = setTimeout(function () {
          pf.fetchTimer = null;
          pfEnsureDownloads();
        }, 3000);
      }
    });
  }

  // pfTrimStore caps stored tracks per the buffering level, never
  // evicting the playing one.
  function pfTrimStore() {
    var cap = pfStoreCap();
    idbReq(idbStore("readonly").getAll()).then(function (recs) {
      if (!recs || recs.length <= cap) return;
      recs.sort(function (a, b) { return a.saved - b.saved; });
      recs.slice(0, recs.length - cap).forEach(function (rec) {
        if (rec.id === pf.playingId) return;
        idbReq(idbStore("readwrite")["delete"](rec.id))["catch"](function () {});
        if (pf.have[rec.id]) {
          try { URL.revokeObjectURL(pf.have[rec.id].url); } catch (e) {}
          delete pf.have[rec.id];
        }
      });
    })["catch"](function () {});
  }

  // pfNextId picks the id to play after the given one, in listing
  // order, falling back to any stored track (offline loop).
  function pfNextId(afterId) {
    var ids = [];
    pf.rows.forEach(function (row) { if (pf.have[row.id]) ids.push(row.id); });
    if (!ids.length) ids = Object.keys(pf.have);
    if (!ids.length) return null;
    var at = ids.indexOf(afterId);
    return ids[(at + 1) % ids.length];
  }

  function pfEl(i) {
    if (!pf.els[i]) {
      var el = document.createElement("audio");
      el.id = "bufaudio" + i;
      el.preload = "auto";
      el.addEventListener("pause", onSystemPause);
      document.body.appendChild(el);
      pf.els[i] = el;
    }
    return pf.els[i];
  }

  function pfPlay(id) {
    var rec = pf.have[id];
    if (!rec) {
      pf.playingId = null;
      pf.wantPlay = true;
      pfEnsureDownloads();
      return;
    }
    pf.wantPlay = false;
    var el = pfEl(pf.cur);
    // Exactly one element ever produces audio: silence and disarm the
    // other one before starting, so a skipped track can neither keep
    // playing underneath nor re-fire its ended handler later.
    var other = pf.els[1 - pf.cur];
    if (other && other !== el) {
      other.onended = null;
      quiet(other);
    }
    if (pf.playingId && pf.playingId !== id) pf.prevId = pf.playingId;
    pf.playingId = id;
    if (el.src !== rec.url) {
      el.src = rec.url;
    } else if (el.ended || el.currentTime > 0) {
      // Replaying a staged or finished element needs a rewind.
      try { el.currentTime = 0; } catch (e) {}
    }
    el.onended = function () { pfAdvance(); };
    el.play().then(function () {
      pfStatus();
      pf.played = (pf.played || 0) + 1;
      lastNow = rec.title || rec.prompt || "buffered track";
      msArtist = "Track " + pf.played + (rec.subtitle ? " · " + rec.subtitle : "");
      applyMediaMetadata();
      mediaPlaybackState("playing");
      updateSaveButtons(null);
      pfPreloadNext();
      pfEnsureDownloads();
    })["catch"](function (e) {
      pfState("could not start audio: " + (e && e.message ? e.message : e), "bad");
    });
  }

  // pfPreloadNext stages the following track on the idle element BEFORE
  // the current one ends, so the swap is immediate.
  function pfPreloadNext() {
    var nextId = pfNextId(pf.playingId);
    if (!nextId || nextId === pf.playingId) return;
    var idle = 1 - pf.cur;
    var el = pfEl(idle);
    if (el.src !== pf.have[nextId].url) {
      el.src = pf.have[nextId].url;
      el.load();
    }
  }

  // pfAdvance moves to the next track (natural end or a manual skip).
  // The outgoing element is silenced and disarmed FIRST, whatever
  // happens next, so no stale handler can ever fire a second advance.
  function pfAdvance() {
    var out = pf.els[pf.cur];
    if (out) {
      out.onended = null;
      quiet(out);
    }
    var nextId = pfNextId(pf.playingId);
    if (!nextId) {
      pfState(pf.offline ? "offline - waiting for stored tracks" : "buffering next track…", "bad");
      pf.playingId = null;
      pf.wantPlay = true;
      pfEnsureDownloads();
      return;
    }
    // With a single stored track, pfPlay rewinds and replays it on the
    // other element; with two or more, the staged element takes over.
    pf.cur = 1 - pf.cur;
    pfPlay(nextId);
  }

  // pfSkip is the manual, debounced skip. It changes playback only on
  // this device; the stream and other listeners keep their position.
  var lastManualSkip = 0;
  function pfSkip() {
    var now = Date.now();
    if (now - lastManualSkip < 700 || !pf.active) return;
    lastManualSkip = now;
    setStatus($("steerstatus"), "skipped on this device only - the stream and other listeners keep their own position", "ok");
    pfAdvance();
  }

  function pfStatus() {
    var extra = pf.offline ? " · offline, playing banked tracks" : "";
    pfState("playing (buffered) · " + pfAhead() + " ahead" + extra, pf.offline ? "bad" : "good");
    pfShowMinutes();
  }

  // ---- transport choice and the play button ------------------------
  function syncTransportUI() {
    $("buffered").checked = transport === "buffered";
    $("bufopts").hidden = transport !== "buffered";
    $("buflevel").value = bufLevel;
    pfShowMinutes();
  }
  $("buffered").addEventListener("change", function () {
    transport = $("buffered").checked ? "buffered" : "direct";
    store.set("iar.transport", transport);
    syncTransportUI();
    if (wantStream || pf.active) {
      stopListening("");
      startListening();
    }
  });
  $("buflevel").addEventListener("change", function () {
    bufLevel = $("buflevel").value;
    store.set("iar.buflevel", bufLevel);
    if (pf.active) {
      pfTrimStore();
      pfEnsureDownloads();
      pfStatus();
    }
  });

  function startListening() {
    store.set("iar.wasplaying", true);
    if (transport === "buffered" && idbSupported) startBuffered(); else startStream();
  }
  function stopListening(msg) {
    if (pf.active) stopBuffered(msg === undefined ? "stopped" : msg, "");
    if (wantStream) stopStream(msg === undefined ? "stopped" : msg, "");
  }

  playBtn.addEventListener("click", function () {
    if (resumePending) { tryResume(); return; }
    if (wantStream || pf.active) {
      stopListening("stopped");
      return;
    }
    startListening();
  });

  $("next").addEventListener("click", function () {
    if (pf.active) {
      pfSkip();
      return;
    }
    act($("next"), $("steerstatus"), "/next", "", "skipping…");
  });

  // ---- media session (lock screen, car displays) -------------------
  // lastNow is the playing track's short title (the laptop's track in
  // direct mode, this device's track in buffered mode, the chunk in
  // saved mode); msArtist carries "Track N" plus the genre/mood
  // subtitle (or the chunk's tag). msFlash briefly overrides the title
  // (the car "Saved" confirmation) and always restores through this
  // same function, so a stale string can never stick.
  var lastNow = "";
  var msArtist = "";
  var msFlash = "";
  var msFlashTimer = null;
  function applyMediaMetadata() {
    if (!("mediaSession" in navigator)) return;
    try {
      navigator.mediaSession.metadata = new MediaMetadata({
        title: msFlash || lastNow || "Infinite AI Radio",
        artist: msArtist || "AI-generated stream",
        album: "Infinite AI Radio",
        artwork: [
          { src: "/icons/icon-192.png", sizes: "192x192", type: "image/png" },
          { src: "/icons/icon-512.png", sizes: "512x512", type: "image/png" }
        ]
      });
    } catch (e) {}
  }
  // flashMetadata shows a short confirmation on the car screen, then
  // re-applies whatever is current (even if the track changed).
  function flashMetadata(text) {
    msFlash = text;
    if (msFlashTimer) clearTimeout(msFlashTimer);
    msFlashTimer = setTimeout(function () {
      msFlash = "";
      msFlashTimer = null;
      applyMediaMetadata();
    }, 2000);
    applyMediaMetadata();
  }
  function mediaPlaybackState(state) {
    if (!("mediaSession" in navigator)) return;
    try { navigator.mediaSession.playbackState = state; } catch (e) {}
  }

  // Car ⏮ = save: an endless generated stream has no meaningful
  // "previous track", so in live mode the slot doubles as Save (the
  // only extra control a web page can put on a car display). Saved
  // mode keeps the real previous behaviour. Persisted per device.
  var carSave = store.get("iar.carsave", true) !== false;
  var lastCarSave = 0;
  function carSaveAction() {
    var now = Date.now();
    if (now - lastCarSave < 700) return; // car-button spam guard
    lastCarSave = now;
    if (!(me && me.save)) return;
    var saveTitle = lastNow || "this track";
    doSave($("save"), false).then(function (d) {
      // Flash only on success (including the idempotent no-op); a
      // failed save leaves the honest metadata alone.
      if (d) flashMetadata("Saved: " + saveTitle);
    });
  }

  // msAction routes every media-session action; the same dispatch is
  // reachable through the "iar:msaction" DOM event so scripted clients
  // can drive the car controls.
  function msAction(action) {
    switch (action) {
      case "play":
        if (mode === "live") { if (!tryResume()) startListening(); } else savedAudio.play();
        break;
      case "pause":
        if (mode === "live") stopListening("stopped"); else savedAudio.pause();
        break;
      case "stop":
        if (mode === "live") stopListening("stopped"); else savedAudio.pause();
        break;
      case "nexttrack":
        if (mode === "live") {
          // Same debounce/double-fire guards as the on-page controls.
          if (pf.active) { pfSkip(); return; }
          if (me && me.steer) act($("next"), $("steerstatus"), "/next", "", "skipping…");
        } else {
          repeatOne = null; updateLoopState(); step(1);
        }
        break;
      case "previoustrack":
        if (mode === "live") { if (carSave) carSaveAction(); } else { repeatOne = null; updateLoopState(); step(-1); }
        break;
    }
  }
  function msHandler(action, fn) {
    if (!("mediaSession" in navigator)) return;
    try { navigator.mediaSession.setActionHandler(action, fn); } catch (e) {}
  }
  ["play", "pause", "stop", "nexttrack"].forEach(function (a) {
    msHandler(a, function () { msAction(a); });
  });
  // The previous-track slot registers only while it has a job (saved
  // mode, or live mode with the car-save toggle on), so a switched-off
  // toggle removes the dead ⏮ from the car instead of ignoring it.
  function syncPrevAction() {
    var active = mode === "saved" || carSave;
    msHandler("previoustrack", active ? function () { msAction("previoustrack"); } : null);
  }
  syncPrevAction();
  document.addEventListener("iar:msaction", function (e) { msAction(e.detail); });
  $("carsave").checked = carSave;
  $("carsave").addEventListener("change", function () {
    carSave = $("carsave").checked;
    store.set("iar.carsave", carSave);
    syncPrevAction();
  });
  $("carresume").checked = carResume;
  $("carresume").addEventListener("change", function () {
    carResume = $("carresume").checked;
    store.set("iar.carresume", carResume);
  });

  // ---- steer / new / save ------------------------------------------
  $("steer").addEventListener("click", function () {
    var t = $("text").value.trim();
    if (!t) { setStatus($("steerstatus"), "type what you want first", "err"); return; }
    act($("steer"), $("steerstatus"), "/steer", "text=" + encodeURIComponent(t), "steering…").then(function (d) {
      if (d) $("text").value = "";
    });
  });
  $("fresh").addEventListener("click", function () {
    var t = $("text").value.trim();
    if (!t) { setStatus($("steerstatus"), "type a prompt first, then Start fresh", "err"); return; }
    act($("fresh"), $("steerstatus"), "/new", "prompt=" + encodeURIComponent(t), "starting fresh…").then(function (d) {
      if (d) $("text").value = "";
    });
  });
  // ---- saved-track state ------------------------------------------
  // The server remembers which track ids were saved; the page greys
  // the save buttons for whatever THIS device is hearing (the laptop's
  // track in direct mode, this device's own track in buffered mode).
  var lastTrack = null;   // /state track object (direct mode)
  var lastPrev = null;    // /state prev object
  var savedIds = {};      // server-confirmed saved ids
  var localSaved = {};    // optimistic marks while the encode runs

  function saveTargets() {
    if (pf.active) return { cur: pf.playingId, prev: pf.prevId };
    return { cur: lastTrack && lastTrack.id, prev: lastPrev && lastPrev.id };
  }
  function isSaved(id) { return !!id && (savedIds[id] || localSaved[id]); }
  function setSavedClass(btn, id) {
    if (!btn) return;
    var on = isSaved(id);
    btn.classList.toggle("saved", on);
    if (on) btn.setAttribute("aria-disabled", "true");
    else btn.removeAttribute("aria-disabled");
  }
  // updateSaveButtons re-derives the greyed state; called on every
  // /state poll and after local saves. s may be null to reuse the last
  // known server state.
  function updateSaveButtons(s) {
    if (s) {
      lastTrack = s.track || null;
      lastPrev = s.prev || null;
      savedIds = {};
      (s.saved_ids || []).forEach(function (id) { savedIds[id] = true; });
      if (s.track && s.track.saved) savedIds[s.track.id] = true;
      if (s.prev && s.prev.saved) savedIds[s.prev.id] = true;
    }
    var ids = saveTargets();
    setSavedClass($("save"), ids.cur);
    setSavedClass($("saveprev"), ids.prev);
  }

  // doSave saves what the listener is hearing: the buffered player's
  // own track ids on the phone, the laptop's current/previous track in
  // direct mode. Saving an already-saved track is a friendly no-op.
  function doSave(btn, wantPrev) {
    var ids = saveTargets();
    var id = wantPrev ? ids.prev : ids.cur;
    if (isSaved(id)) {
      setStatus($("savestatus"), "already saved", "ok");
      return Promise.resolve({ ack: "already saved" });
    }
    var which = "";
    if (pf.active) {
      if (!id) {
        setStatus($("savestatus"), wantPrev ? "no previous track on this device yet" : "nothing is playing on this device yet", "err");
        return Promise.resolve(null);
      }
      which = id;
    } else if (wantPrev) {
      which = "prev";
    }
    var body = "tag=" + encodeURIComponent($("tag").value.trim());
    if (which) body += "&which=" + encodeURIComponent(which);
    return act(btn, $("savestatus"), "/save", body,
      wantPrev ? "saving the previous track…" : "saving this track…").then(function (d) {
      if (!d) return d;
      if (id && d.ack && (d.ack.indexOf("saving") >= 0 || d.ack.indexOf("already saved") >= 0)) {
        localSaved[id] = true;
        updateSaveButtons(null);
      }
      if (mode === "saved") loadChunks();
      return d;
    });
  }
  $("save").addEventListener("click", function () { doSave($("save"), false); });
  $("saveprev").addEventListener("click", function () { doSave($("saveprev"), true); });

  // ---- sessions ---------------------------------------------------
  $("sesssave").addEventListener("click", function () {
    var n = $("sessname").value.trim();
    if (!n) { setStatus($("sessstatus"), "type a name for this session first", "err"); return; }
    act($("sesssave"), $("sessstatus"), "/sessions/save", "name=" + encodeURIComponent(n), "saving session…").then(function (d) {
      if (d) { $("sessname").value = ""; loadSessions(); }
    });
  });

  function sessionRow(item, group) {
    var row = document.createElement("div");
    row.className = "chunk sess" + (item.current ? " playing" : "");
    var title = document.createElement("div");
    title.className = "ctitle";
    title.textContent = item.name + (item.current ? "  (playing)" : "");
    var meta = document.createElement("div");
    meta.className = "cmeta";
    meta.textContent = item.summary + (item.played ? " · " + item.played : "");
    row.appendChild(title);
    row.appendChild(meta);
    var canLoad = me && me.new_prompt && !item.current;
    var canDelete = me && me.admin && !item.current;
    if (canLoad || canDelete) {
      var buttons = document.createElement("div");
      buttons.className = "row";
      if (canLoad) {
        var load = document.createElement("button");
        load.className = "action";
        load.textContent = group === "presets" ? "Start preset" : "Load";
        load.addEventListener("click", function () {
          act(load, $("sessstatus"), "/sessions/load", "name=" + encodeURIComponent(item.name), "loading…").then(function (d) {
            if (d) loadSessions();
          });
        });
        buttons.appendChild(load);
      }
      if (canDelete) {
        var del = document.createElement("button");
        del.className = "action danger";
        del.textContent = "Delete";
        del.addEventListener("click", function () {
          var what = group === "presets" ? "Hide preset " : "Delete session ";
          if (!window.confirm(what + item.name + "?")) return;
          act(del, $("sessstatus"), "/sessions/delete", "name=" + encodeURIComponent(item.name), "deleting…").then(function (d) {
            if (d) loadSessions();
          });
        });
        buttons.appendChild(del);
      }
      row.appendChild(buttons);
    }
    return row;
  }

  function sessionGroup(label, items, group, collapsed) {
    var box;
    if (collapsed) {
      box = document.createElement("details");
      var sum = document.createElement("summary");
      sum.className = "grouplabel";
      sum.textContent = label + " (" + items.length + ")";
      box.appendChild(sum);
    } else {
      box = document.createElement("div");
      var h = document.createElement("div");
      h.className = "grouplabel";
      h.textContent = label;
      box.appendChild(h);
    }
    if (!items.length) {
      var empty = document.createElement("div");
      empty.className = "empty";
      empty.textContent = group === "named" ? "none yet - save this session under a name" : "none";
      box.appendChild(empty);
    }
    items.forEach(function (item) { box.appendChild(sessionRow(item, group)); });
    return box;
  }

  // presetGroups renders the presets as collapsible energy groups, the
  // playing preset's group open, the rest collapsed.
  function presetGroups(presets, currentPreset) {
    var box = document.createElement("div");
    var head = document.createElement("div");
    head.className = "grouplabel";
    head.textContent = "Presets";
    box.appendChild(head);
    if (!presets.length) {
      var empty = document.createElement("div");
      empty.className = "empty";
      empty.textContent = "none";
      box.appendChild(empty);
      return box;
    }
    var openGroup = "";
    presets.forEach(function (p) { if (p.name === currentPreset) openGroup = p.group || "other"; });
    var byGroup = [];
    presets.forEach(function (p) {
      var g = p.group || "other";
      if (!byGroup.length || byGroup[byGroup.length - 1].name !== g) {
        byGroup.push({ name: g, items: [] });
      }
      byGroup[byGroup.length - 1].items.push(p);
    });
    byGroup.forEach(function (g) {
      var det = document.createElement("details");
      det.className = "presetgroup";
      if (g.name === openGroup) det.open = true;
      var sum = document.createElement("summary");
      sum.className = "grouplabel";
      sum.textContent = g.name + " (" + g.items.length + ")";
      det.appendChild(sum);
      g.items.forEach(function (item) { det.appendChild(sessionRow(item, "presets")); });
      box.appendChild(det);
    });
    return box;
  }

  function loadSessions() {
    fetch("/api/sessions").then(function (r) {
      if (r.status === 401) { loggedOut(); return null; }
      return r.json();
    }).then(function (d) {
      if (!d) return;
      var box = $("sessions");
      box.innerHTML = "";
      box.appendChild(sessionGroup("Your sessions", d.named, "named", false));
      box.appendChild(presetGroups(d.presets, d.current_preset));
      box.appendChild(sessionGroup("Auto-saved sessions", d.auto, "auto", true));
    }).catch(function () {});
  }
  setInterval(loadSessions, 30000);

  // renderSound shows the shared steering state: the base prompt plus
  // the ordered tweak chips. It is the session's context ("Steering
  // now"), distinct from the playing track's prompt above it.
  var soundSig = "";
  function renderSound(s) {
    var sig = JSON.stringify([s.base_prompt, s.vocals, s.tweaks]);
    if (sig === soundSig) return;
    soundSig = sig;
    $("baseprompt").textContent = (s.base_prompt || "…") + (s.vocals ? "  ·  vocals on" : "");
    var box = $("tweaks");
    box.innerHTML = "";
    (s.tweaks || []).forEach(function (tw) {
      var chip = document.createElement("span");
      chip.className = "chip tweak";
      chip.textContent = tw.raw;
      if (tw.interpreted) chip.title = tw.interpreted;
      box.appendChild(chip);
    });
  }

  function poll() {
    fetch("/state").then(function (r) {
      if (r.status === 401) { loggedOut(); return null; }
      return r.json();
    }).then(function (s) {
      if (!s) return;
      $("conn").textContent = "connected";
      var t = s.track;
      $("now").textContent = (t && (t.title || t.prompt)) || s.source || s.state || "...";
      $("nowprompt").textContent = (t && t.title && t.prompt) || "";
      var meta = t && t.number ? "track " + t.number : "";
      if (s.session) meta += (meta ? "  ·  " : "") + "session " + s.session;
      if (s.elapsed) meta += (meta ? "  ·  " : "") + s.elapsed + " / " + s.duration;
      meta += (meta ? "  ·  " : "") + s.queued + " ready" + (s.generating ? " · generating" : "");
      $("meta").textContent = meta;
      $("phase").textContent =
        s.phase ? s.phase + " (" + s.phase_info + ")" : (s.paused ? "paused at the machine" : "");
      renderSound(s);
      updateSaveButtons(s);
      if (pf.active && pf.epoch >= 0 && s.epoch !== pf.epoch) {
        pfRefreshQueue();
      }
      // Keep the media session honest in every mode: the title is the
      // track's short name and the artist carries "Track N" plus the
      // genre/mood subtitle. Direct mode follows the laptop's track
      // here; buffered mode plays this device's own track and sets its
      // metadata in pfPlay.
      var changed = false;
      if (mode === "live" && !pf.active) {
        var artist = t && t.number
          ? "Track " + t.number + (t.subtitle ? " · " + t.subtitle : "")
          : (s.session_desc || s.session || "");
        if (artist !== msArtist) {
          msArtist = artist;
          changed = true;
        }
        var now = (t && (t.title || t.prompt)) || s.source || s.state || "";
        if (now !== lastNow) {
          lastNow = now;
          changed = true;
        }
      }
      if (changed && (wantStream || pf.active)) applyMediaMetadata();
    }).catch(function () {
      $("conn").textContent = "disconnected";
    });
  }
  poll();
  setInterval(poll, 2000);

  // ---- saved chunks ------------------------------------------------
  var savedAudio = $("savedaudio");
  var chunks = [];          // everything the server has
  var tagsOn = store.get("iar.tags", {}); // tag -> selected; unknown tags default to on
  var playlist = [];        // chunks in the selected tags
  var current = null;       // chunk playing in saved mode
  var repeatOne = null;     // chunk looped on its own

  function tagOn(tag) { return tagsOn[tag] !== false; }

  function fmtSecs(s) {
    s = Math.round(s || 0);
    return Math.floor(s / 60) + ":" + ("0" + (s % 60)).slice(-2);
  }
  function fmtDate(iso) {
    var d = new Date(iso);
    if (isNaN(d.getTime())) return "";
    return d.toLocaleDateString(undefined, { month: "short", day: "numeric" }) + " " +
      d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  }

  function rebuildPlaylist() {
    playlist = chunks.filter(function (c) { return tagOn(c.tag); });
  }

  function renderTags(tags) {
    var box = $("tags");
    box.innerHTML = "";
    if (!tags.length) {
      box.innerHTML = '<span class="empty">no saved chunks yet</span>';
      return;
    }
    tags.forEach(function (tag) {
      var label = document.createElement("label");
      label.className = "chip" + (tagOn(tag) ? " on" : "");
      var cb = document.createElement("input");
      cb.type = "checkbox";
      cb.checked = tagOn(tag);
      cb.addEventListener("change", function () {
        tagsOn[tag] = cb.checked;
        store.set("iar.tags", tagsOn);
        label.classList.toggle("on", cb.checked);
        rebuildPlaylist();
        renderChunks();
      });
      label.appendChild(cb);
      label.appendChild(document.createTextNode(tag));
      box.appendChild(label);
    });
  }

  function renderChunks() {
    var box = $("chunks");
    box.innerHTML = "";
    if (!chunks.length) {
      box.innerHTML = '<span class="empty">nothing saved yet - save a track from the live stream</span>';
      return;
    }
    chunks.forEach(function (c) {
      if (!tagOn(c.tag)) return;
      var row = document.createElement("div");
      row.className = "chunk" + (current && current.url === c.url ? " playing" : "");
      var title = document.createElement("div");
      title.className = "ctitle";
      title.textContent = c.title;
      var meta = document.createElement("div");
      meta.className = "cmeta";
      meta.textContent = (c.subtitle ? c.subtitle + " · " : "") + c.tag + " · " + fmtSecs(c.seconds) + " · " + fmtDate(c.saved);
      var row2 = document.createElement("div");
      row2.className = "row";
      var play = document.createElement("button");
      play.className = "action";
      play.textContent = "Play";
      play.addEventListener("click", function () { playChunk(c); });
      var loop = document.createElement("button");
      loop.className = "action" + (repeatOne && repeatOne.url === c.url ? " looping" : "");
      loop.textContent = repeatOne && repeatOne.url === c.url ? "Looping this one" : "Loop this one";
      loop.addEventListener("click", function () { loopOne(c); });
      row2.appendChild(play);
      row2.appendChild(loop);
      row.appendChild(title);
      row.appendChild(meta);
      row.appendChild(row2);
      box.appendChild(row);
    });
  }

  function updateLoopState() {
    var el = $("loopstate");
    if (repeatOne) {
      el.textContent = "Looping one chunk: " + repeatOne.title;
      el.className = "one";
      $("backloop").hidden = false;
    } else {
      el.textContent = "Looping every chunk in the selected tags";
      el.className = "";
      $("backloop").hidden = true;
    }
    savedAudio.loop = !!repeatOne;
  }

  function playChunk(c) {
    current = c;
    savedAudio.src = c.url;
    savedAudio.play().catch(function (e) { $("savednow").textContent = "could not play: " + e.message; });
    $("savednow").className = "";
    $("savednow").textContent = c.title + "  (" + c.tag + ")";
    lastNow = c.title;
    msArtist = c.subtitle ? c.subtitle + " · " + c.tag : c.tag;
    applyMediaMetadata();
    renderChunks();
  }

  function loopOne(c) {
    repeatOne = c;
    updateLoopState();
    if (!current || current.url !== c.url) playChunk(c); else renderChunks();
  }

  $("backloop").addEventListener("click", function () {
    repeatOne = null;
    updateLoopState();
    renderChunks();
  });

  function step(dir) {
    if (!playlist.length) return;
    var i = -1;
    if (current) {
      playlist.forEach(function (c, k) { if (c.url === current.url) i = k; });
    }
    i = (i + dir + playlist.length) % playlist.length;
    playChunk(playlist[i]);
  }
  $("snext").addEventListener("click", function () { repeatOne = null; updateLoopState(); step(1); });
  $("sprev").addEventListener("click", function () { repeatOne = null; updateLoopState(); step(-1); });
  $("splay").addEventListener("click", function () {
    if (current && savedAudio.paused && savedAudio.src) { savedAudio.play(); return; }
    if (current && !savedAudio.paused) { savedAudio.pause(); return; }
    step(1);
  });
  savedAudio.addEventListener("ended", function () { if (!repeatOne) step(1); });
  savedAudio.addEventListener("play", function () { $("splay").innerHTML = "&#10074;&#10074;&#xFE0E; Pause"; mediaPlaybackState("playing"); });
  savedAudio.addEventListener("pause", function () { $("splay").innerHTML = "&#9654;&#xFE0E; Play"; mediaPlaybackState("paused"); });

  function loadChunks() {
    fetch("/api/chunks").then(function (r) {
      if (r.status === 401) { loggedOut(); return null; }
      return r.json();
    }).then(function (d) {
      if (!d) return;
      chunks = d.chunks || [];
      renderTags(d.tags || []);
      rebuildPlaylist();
      renderChunks();
      updateLoopState();
    }).catch(function () {});
  }
  setInterval(function () { if (mode === "saved") loadChunks(); }, 30000);

  syncTransportUI();
  setMode(mode);
  // Cold start: a page load while listening was on (a reload, or Chrome
  // reopened in the car) tries to pick playback straight back up; a
  // blocked autoplay degrades to the one-tap play button.
  if (carResume && mode === "live" && store.get("iar.wasplaying", false)) {
    startListening();
  }
})();
