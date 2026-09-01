(function () {
  "use strict";
  var $ = function (id) { return document.getElementById(id); };
  var store = {
    get: function (k, d) { try { var v = localStorage.getItem(k); return v === null ? d : JSON.parse(v); } catch (e) { return d; } },
    set: function (k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch (e) {} }
  };
  var headers = { "Content-Type": "application/x-www-form-urlencoded", "X-IAR-Remote": "1" };

  // ---- transport icons ---------------------------------------------
  // Drawn rather than typed. A font glyph renders at whatever weight and
  // aspect the device's system font feels like - on a phone the heart
  // came out squashed - and the filled and outline hearts have to share
  // exact geometry so the state swap does not shift anything.
  var SVG = '<svg viewBox="0 0 24 24" aria-hidden="true" focusable="false" ';
  var HEART_PATH = "M12 21.35l-1.45-1.32C5.4 15.36 2 12.28 2 8.5 2 5.42 4.42 3 7.5 3c1.74 0 " +
    "3.41.81 4.5 2.09C13.09 3.81 14.76 3 16.5 3 19.58 3 22 5.42 22 8.5c0 3.78-3.4 6.86-8.55 11.54L12 21.35z";
  var icon = {
    play: SVG + 'fill="currentColor"><path d="M8 5v14l11-7z"/></svg>',
    stop: SVG + 'fill="currentColor"><rect x="6" y="6" width="12" height="12" rx="2.5"/></svg>',
    pause: SVG + 'fill="currentColor"><rect x="7" y="5" width="3.6" height="14" rx="1.4"/>' +
      '<rect x="13.4" y="5" width="3.6" height="14" rx="1.4"/></svg>',
    next: SVG + 'fill="currentColor"><path d="M6 18l8.5-6L6 6v12z"/><rect x="16" y="6" width="2.4" height="12" rx="1.2"/></svg>',
    prev: SVG + 'fill="currentColor"><rect x="5.6" y="6" width="2.4" height="12" rx="1.2"/><path d="M18 6l-8.5 6 8.5 6V6z"/></svg>',
    heart: SVG + 'fill="none" stroke="currentColor" stroke-width="1.7" stroke-linejoin="round"><path d="' + HEART_PATH + '"/></svg>',
    heartFull: SVG + 'fill="currentColor"><path d="' + HEART_PATH + '"/></svg>'
  };

  // ---- press feedback ----------------------------------------------
  // Every control answers a touch within a frame. The listeners are
  // delegated from the document, so rows and chips built later are
  // covered too, and pointercancel is handled: without it a press that
  // turns into a scroll leaves the control stuck down.
  var pressedEl = null;
  var pressedAt = 0;
  // A tap on a phone is over in well under a tenth of a second. Without
  // a floor the pressed state is gone before the screen has drawn it,
  // which reads as "the button did nothing".
  var minPressMs = 160;
  function releasePress() {
    if (!pressedEl) return;
    var el = pressedEl;
    pressedEl = null;
    var held = Date.now() - pressedAt;
    if (held >= minPressMs) {
      el.classList.remove("is-pressed");
      return;
    }
    setTimeout(function () {
      if (pressedEl !== el) el.classList.remove("is-pressed");
    }, minPressMs - held);
  }
  document.addEventListener("pointerdown", function (e) {
    var t = e.target && e.target.closest && e.target.closest(".tap");
    releasePress();
    if (!t || t.getAttribute("aria-disabled") === "true") return;
    pressedEl = t;
    pressedAt = Date.now();
    t.classList.add("is-pressed");
  }, { passive: true, capture: true });
  ["pointerup", "pointercancel", "pointerleave", "blur"].forEach(function (name) {
    document.addEventListener(name, releasePress, { passive: true, capture: true });
  });
  // An empty touchstart listener is what makes :active fire on iOS.
  document.addEventListener("touchstart", function () {}, { passive: true });

  // buzz is a supplement to the visible press, never the only feedback:
  // it is absent on iOS and on a phone with vibration switched off.
  var haptics = store.get("iar.haptics", true) !== false;
  function buzz() {
    if (!haptics || !("vibrate" in navigator)) return;
    try { navigator.vibrate(10); } catch (e) {}
  }

  // say announces a transition to a screen reader. The visible status
  // line updates every couple of seconds with an elapsed count, which
  // is not something anyone wants read aloud.
  var lastSaid = "";
  function say(text) {
    var el = $("say");
    if (!el || (text || "") === lastSaid) return;
    lastSaid = text || "";
    el.textContent = lastSaid;
  }

  // ---- permissions ------------------------------------------------
  var me = null;
  function applyPerms() {
    var perms = { steer: !!me.steer, new_prompt: !!me.new_prompt, save: !!me.save, admin: !!me.admin };
    Array.prototype.forEach.call(document.querySelectorAll("[data-perm]"), function (el) {
      el.hidden = !perms[el.getAttribute("data-perm")];
    });
    // The sound block itself is a readout everyone may see; only the
    // controls inside it need the permission, and the heading says
    // which of the two the reader is looking at.
    var canSteer = perms.steer || perms.new_prompt;
    $("steerbox").hidden = !canSteer;
    $("soundlabel").textContent = canSteer ? "Steer the sound" : "The sound";
    // A card whose every control is out of reach is a heading with
    // nothing under it.
    $("vocalscard").hidden = !(perms.steer || perms.admin);
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
    // An action started on the main screen answers in the transport bar
    // as well as in its own card, which may be inside the sheet.
    if (el.length !== undefined && el.nodeType === undefined) {
      for (var i = 0; i < el.length; i++) setStatus(el[i], text, cls);
      return;
    }
    if (el === stateEl) { toast(text, cls); return; }
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

  // act posts one mutating action with honest feedback: the press has
  // already shown, the button is marked busy for the round trip, an
  // immediate optimistic status appears where the tap was, failures
  // show in a distinct colour, and double-taps are no-ops.
  function act(btn, statusEl, path, body, pending) {
    if (btn && btn.getAttribute("aria-disabled") === "true") { return Promise.resolve(null); }
    if (btn) {
      // aria-disabled rather than disabled: the button keeps its
      // contrast and stays focusable, and the guard above is what
      // actually stops a double tap. The ring inside .is-working only
      // becomes visible after 400ms, so a quick round trip never
      // flashes a spinner.
      btn.setAttribute("aria-disabled", "true");
      btn.classList.add("is-working");
      releasePress();
    }
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
        say(d.ack || "done");
        return d;
      })
      .catch(function (e) {
        setStatus(statusEl, "failed: " + e.message, "err");
        say("failed: " + e.message);
        return null;
      })
      .finally(function () {
        if (btn) { btn.removeAttribute("aria-disabled"); btn.classList.remove("is-working"); }
      });
  }

  // ---- mode switch -------------------------------------------------
  // The visible tab and the playing audio are independent: switching
  // tabs is just looking, and whatever was playing keeps playing. The
  // bottom transport follows the PLAYING source, not the tab - it only
  // hands over when the listener actually starts the other side.
  var mode = store.get("iar.mode", "live");
  var audioSource = "live"; // which player the transport controls
  function syncSourceTransport() {
    var live = audioSource === "live";
    $("livetransport").hidden = !live;
    $("savedtransport").hidden = live;
  }
  function setAudioSource(src) {
    if (audioSource === src) return;
    audioSource = src;
    if (src === "saved") {
      stopListening("stopped (playing a saved song)");
    } else {
      savedAudio.pause();
    }
    syncSourceTransport();
    syncPrevAction();
  }
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
    $("scroll").scrollTop = 0;
    if (!live) loadChunks();
    syncSourceTransport();
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

  // An acknowledgement owns the status line for a few seconds; the
  // stream's own running commentary waits its turn. A failure never
  // waits.
  var ackUntil = 0;
  function toast(text, cls) {
    ackUntil = Date.now() + 4000;
    stateEl.textContent = text || "";
    stateEl.className = cls || "";
    signalFor(cls === "err" ? "bad" : "");
    say(text);
  }
  function streamState(text, cls) {
    if (cls !== "bad" && Date.now() < ackUntil) return;
    ackUntil = 0;
    stateEl.textContent = text;
    stateEl.className = cls || "";
    signalFor(cls);
  }
  // signalFor keeps the app bar's one coloured dot honest: green while
  // audio is flowing, amber while it is working on it, red when it has
  // stopped and needs a hand.
  function signalFor(cls) {
    var dot = $("signal");
    if (!dot) return;
    var on = "";
    if (cls === "good" || cls === "ok") on = "live";
    else if (cls === "bad" || cls === "err") on = "bad";
    else if (wantStream || pf.active) on = "warn";
    dot.className = "signal" + (on ? " " + on : "");
  }
  // setPlayButton renders intent, not readiness: while a reconnect is
  // running the button already says Stop, because that is what tapping
  // it does. The status line carries the honest connection state.
  function setPlayButton(on) {
    playBtn.classList.toggle("playing", !!on);
    $("playglyph").innerHTML = on ? icon.stop : icon.play;
    $("playlabel").textContent = on ? "Stop" : "Play";
    playBtn.setAttribute("aria-label", on ? "Stop listening" : "Play the live stream");
  }
  function setSavedPlayButton(on) {
    $("splayglyph").innerHTML = on ? icon.pause : icon.play;
    $("splaylabel").textContent = on ? "Pause" : "Play";
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
    streamState("audio output disconnected - tap play", "bad");
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

  // ---- blocked autoplay --------------------------------------------
  // A page cannot make sound until the browser has seen a gesture on
  // it, so a reload while listening (or the car reopening the page) is
  // refused. Rather than leaving a dead error, the next tap anywhere
  // starts playback. The events armed here are the ones that actually
  // count as activation on a touch screen: a finger's pointerdown does
  // not, pointerup and touchend do. Only automatic starts arm this; an
  // explicit tap on play is already the gesture.
  var gestureArmed = false;
  var autoStarting = false;
  var gestureEvents = ["pointerup", "touchend", "keydown"];
  function disarmGestureStart() {
    if (!gestureArmed) return;
    gestureArmed = false;
    gestureEvents.forEach(function (name) {
      document.removeEventListener(name, onGesture, true);
    });
  }
  function onGesture(e) {
    if (wantStream || pf.active || mode !== "live") { disarmGestureStart(); return; }
    var t = e && e.target;
    // A tap aimed at a control belongs to that control.
    if (t && t.closest && t.closest("button, a, input, select, textarea, label, summary")) return;
    primeAudio();
    startListening();
  }
  function armGestureStart() {
    if (!autoStarting || gestureArmed) return;
    gestureArmed = true;
    gestureEvents.forEach(function (name) {
      document.addEventListener(name, onGesture, true);
    });
    streamState("tap anywhere to start the audio", "bad");
  }
  // primeAudio clears the per-element playback lock during the gesture
  // itself. The buffered player opens its store before it can play, and
  // a play() that only happens after that wait has already lost the
  // gesture on some browsers; a load() now keeps the element allowed.
  function primeAudio() {
    if (transport !== "buffered" || !idbSupported) return;
    for (var i = 0; i < pf.els.length; i++) {
      try { pfEl(i).load(); } catch (err) {}
    }
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
    setPlayButton(false);
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
      autoStarting = false;
      disarmGestureStart();
      applyMediaMetadata();
      mediaPlaybackState("playing");
    }).catch(function (e) {
      if (e && e.name === "NotAllowedError") {
        stopStream("tap play to start audio", "bad");
        armGestureStart();
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
    // The button tracks the intent from this instant: during a slow
    // connect or a reconnect it must not invite a tap that would in
    // fact abort what it says it is starting.
    setPlayButton(true);
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
    seen: {},        // ids already heard in this context, so Next moves on
    wrapped: false,  // the last pick came back round to something heard
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
    setPlayButton(true);
    pfState("preparing buffered playback…", "");
    (pf.db ? Promise.resolve(pf.db) : idbOpen().then(function (db) { pf.db = db; return db; }))
      .then(function () { return idbReq(idbStore("readonly").getAll()); })
      .then(function (recs) {
        (recs || []).forEach(function (rec) {
          if (!pf.have[rec.id]) {
            // epoch left undefined: the first queue listing decides
            // whether the record is still current and restamps it.
            pf.have[rec.id] = { url: URL.createObjectURL(rec.blob), prompt: rec.prompt, title: rec.title, subtitle: rec.subtitle, dur: rec.dur, lyrics: rec.lyrics || "" };
          }
        });
        pf.wantPlay = true;
        pfShowMinutes();
        updateSaveButtons(null);
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
    updateSaveButtons(null);
    setPlayButton(false);
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
      // Songs are often downloaded before the server has settled on
      // their name; when a fresh listing carries a better one, adopt
      // it - in the record, in the store, and on the lock screen if
      // that song is the one playing.
      pf.rows.forEach(function (row) {
        var rec = pf.have[row.id];
        if (!rec || !row.title || (rec.title === row.title && rec.subtitle === row.subtitle)) return;
        rec.title = row.title;
        rec.subtitle = row.subtitle;
        idbReq(idbStore("readonly").get(row.id)).then(function (stored) {
          if (!stored) return;
          stored.title = row.title;
          stored.subtitle = row.subtitle;
          return idbReq(idbStore("readwrite").put(stored));
        })["catch"](function () {});
        if (row.id === pf.playingId) {
          lastNow = rec.title || rec.prompt || "buffered track";
          msArtist = "Track " + (pf.played || 0) + (rec.subtitle ? " \u00b7 " + rec.subtitle : "");
          applyMediaMetadata();
          pfStatus();
        }
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
    pf.seen = {};
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
      pf.have[row.id] = { url: URL.createObjectURL(blob), prompt: row.prompt, title: row.title, subtitle: row.subtitle, epoch: pf.epoch, dur: row.duration_s, lyrics: row.lyrics || "" };
      idbReq(idbStore("readwrite").put({ id: row.id, prompt: row.prompt, title: row.title, subtitle: row.subtitle, epoch: pf.epoch, dur: row.duration_s, lyrics: row.lyrics || "", blob: blob, saved: Date.now() }))["catch"](function () {});
      pfTrimStore();
      pfShowMinutes();
      // Only a freshly generated track is worth cutting the current
      // song short for. Banked filler is older than what is playing and
      // may be in a language the listener has just switched off.
      if (pf.switchOnDownload && row.kind !== "library") {
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
  // order, falling back to any stored track (offline loop). In strict
  // mode it returns null rather than wrapping onto the track already
  // playing: a natural end may restart the only stored track, but a
  // press of Next that silently replays it reads as a broken button.
  function pfNextId(afterId, strict) {
    var ids = [];
    pf.rows.forEach(function (row) { if (pf.have[row.id]) ids.push(row.id); });
    if (!ids.length) ids = Object.keys(pf.have);
    if (!ids.length) return null;
    var at = ids.indexOf(afterId);
    // Prefer something not heard yet in this context: a store of three
    // tracks otherwise cycles the same three forever.
    pf.wrapped = false;
    for (var i = 1; i <= ids.length; i++) {
      var cand = ids[(at + i) % ids.length];
      if (!pf.seen[cand]) return cand;
    }
    pf.wrapped = true;
    var next = ids[(at + 1) % ids.length];
    if (strict && next === afterId) return null;
    return next;
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
    pf.seen[id] = true;
    if (el.src !== rec.url) {
      el.src = rec.url;
    } else if (el.ended || el.currentTime > 0) {
      // Replaying a staged or finished element needs a rewind.
      try { el.currentTime = 0; } catch (e) {}
    }
    el.onended = function () { pfAdvance(); };
    el.play().then(function () {
      autoStarting = false;
      disarmGestureStart();
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
      if (e && e.name === "NotAllowedError") {
        // The browser will not let a page make sound until it has seen
        // a gesture. Downloaded tracks stay banked; only playback stops.
        stopBuffered("tap play to start audio", "bad");
        armGestureStart();
        return;
      }
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
    if (!pfNextId(pf.playingId, true)) {
      setStatus([stateEl, $("steerstatus")],
        "nothing new to skip to yet - still downloading the next track", "warn");
      pfEnsureDownloads();
      return;
    }
    setStatus([stateEl, $("steerstatus")], "skipped on this device only", "ok");
    pfAdvance();
  }

  function pfStatus() {
    var extra = pf.offline ? " · offline, playing banked tracks" : "";
    if (!pf.offline && pf.wrapped) extra = " · replaying stored tracks, nothing new yet";
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
    setAudioSource("live");
    store.set("iar.wasplaying", true);
    if (transport === "buffered" && idbSupported) startBuffered(); else startStream();
  }
  function stopListening(msg) {
    if (pf.active) stopBuffered(msg === undefined ? "stopped" : msg, "");
    if (wantStream) stopStream(msg === undefined ? "stopped" : msg, "");
  }

  playBtn.addEventListener("click", function () {
    autoStarting = false;
    disarmGestureStart();
    if (resumePending) { tryResume(); return; }
    if (wantStream || pf.active) {
      stopListening("stopped");
      return;
    }
    startListening();
  });

  $("next").addEventListener("click", function () {
    buzz();
    if (pf.active) {
      pfSkip();
      return;
    }
    act($("next"), [stateEl, $("steerstatus")], "/next", "", "skipping…");
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
    doSave($("save"), false, [stateEl, $("savestatus")]).then(function (d) {
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
        if (audioSource === "live") { if (!tryResume()) startListening(); } else savedAudio.play();
        break;
      case "pause":
        if (audioSource === "live") stopListening("stopped"); else savedAudio.pause();
        break;
      case "stop":
        if (audioSource === "live") stopListening("stopped"); else savedAudio.pause();
        break;
      case "nexttrack":
        if (audioSource === "live") {
          // Same debounce/double-fire guards as the on-page controls.
          if (pf.active) { pfSkip(); return; }
          if (me && me.steer) act($("next"), [stateEl, $("steerstatus")], "/next", "", "skipping…");
        } else {
          repeatOne = null; updateLoopState(); step(1);
        }
        break;
      case "previoustrack":
        if (audioSource === "live") { if (carSave) carSaveAction(); } else { repeatOne = null; updateLoopState(); step(-1); }
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
    var active = audioSource === "saved" || carSave;
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
  // The steering panel is set-and-forget, so it starts closed; the
  // lyrics below it are what changes every track, so they start open.
  // Either choice sticks once made, so nobody re-does it every load.
  [["steercard", false], ["lyricscard", true]].forEach(function (pair) {
    var box = $(pair[0]);
    if (!box) return;
    var key = "iar." + pair[0] + "open";
    box.open = store.get(key, pair[1]) !== false;
    box.addEventListener("toggle", function () { store.set(key, box.open); });
  });

  // ---- lyrics ------------------------------------------------------
  // Section markers ([Verse], [Chorus]) are the writer's structure, not
  // words anyone sings, so they are set back rather than dropped.
  var lyricsSig = null;
  var lyricsText = "";
  // Buffered playback runs on this device's own copy, which is rarely
  // the track the machine is on: the words have to follow what is
  // coming out of THIS phone, or they arrive a track late.
  function playingLyrics(s) {
    if (pf.active) {
      if (!pf.playingId) return "";
      var rec = pf.have[pf.playingId];
      return (rec && rec.lyrics) || "";
    }
    if (!wantStream && mode === "live") return "";
    return (s.track && s.track.lyrics) || "";
  }
  function renderLyrics(s) {
    var text = playingLyrics(s);
    lyricsText = text;
    if (text === lyricsSig) return;
    lyricsSig = text;
    var box = $("lyrics");
    box.innerHTML = "";
    if (!text) {
      var none = document.createElement("span");
      none.className = "empty";
      none.textContent = !(pf.active || wantStream) ? "press play to follow the words"
        : s.vocals ? "no words for this track" : "this track has no vocals";
      box.appendChild(none);
      return;
    }
    text.split("\n").forEach(function (line, i) {
      if (i) box.appendChild(document.createTextNode("\n"));
      var trimmed = line.trim();
      if (trimmed.charAt(0) === "[" && trimmed.charAt(trimmed.length - 1) === "]") {
        var tag = document.createElement("span");
        tag.className = "tag";
        tag.textContent = line;
        box.appendChild(tag);
      } else {
        box.appendChild(document.createTextNode(line));
      }
    });
  }

  // ---- the settings sheet ------------------------------------------
  // Everything set once per device lives here, one tap from anywhere,
  // and nothing set once per device lives anywhere else.
  var sheet = $("sheet");
  function openSheet() {
    if (sheet.open) return;
    if (sheet.showModal) sheet.showModal(); else sheet.setAttribute("open", "");
  }
  function closeSheet() {
    if (sheet.close) sheet.close(); else sheet.removeAttribute("open");
  }
  $("more").addEventListener("click", openSheet);
  $("sheetclose").addEventListener("click", closeSheet);
  // A tap on the backdrop is the gesture everyone reaches for first.
  sheet.addEventListener("click", function (e) {
    if (e.target === sheet) closeSheet();
  });
  $("haptics").checked = haptics;
  $("haptics").addEventListener("change", function () {
    haptics = $("haptics").checked;
    store.set("iar.haptics", haptics);
    if (haptics) buzz();
  });
  if (!("vibrate" in navigator)) $("hapticsrow").hidden = true;

  // ---- sung languages ----------------------------------------------
  // The configured list is the machine's; which of them this session
  // sings in is one tap on the live screen. With nothing configured the
  // whole surface stays out of the way and the engine keeps choosing.
  var langSig = "";
  function renderLanguages(s) {
    var list = s.languages || [];
    var sig = JSON.stringify(list);
    if (sig === langSig) return;
    langSig = sig;
    $("langgroup").hidden = list.length === 0;
    var box = $("langs");
    box.innerHTML = "";
    list.forEach(function (l) {
      var chip = document.createElement("button");
      chip.type = "button";
      chip.className = "chip tap" + (l.on ? " on" : " off");
      chip.textContent = l.name;
      chip.setAttribute("aria-pressed", l.on ? "true" : "false");
      if (!l.engine) {
        chip.title = "The music engine has no voice for " + l.name +
          "; the words are written in it and sung untagged.";
      }
      if (!(me && me.steer)) {
        chip.disabled = true;
        chip.classList.remove("tap");
      } else {
        chip.addEventListener("click", function () {
          // Flip it now; the next poll confirms or corrects it.
          var next = !l.on;
          l.on = next;
          langSig = "";
          chip.classList.toggle("on", next);
          chip.classList.toggle("off", !next);
          chip.setAttribute("aria-pressed", next ? "true" : "false");
          buzz();
          act(chip, stateEl, "/language",
            "name=" + encodeURIComponent(l.name) + "&on=" + (next ? "1" : "0"),
            (next ? "singing in " : "dropping ") + l.name + "…");
        });
      }
      box.appendChild(chip);
    });
    // A tooltip is invisible on a touch screen, and this one is a
    // consequence rather than a rationale, so it goes on the page.
    var untagged = [];
    list.forEach(function (l) { if (!l.engine) untagged.push(l.name); });
    var note = $("langnote");
    note.hidden = untagged.length === 0;
    note.textContent = untagged.length
      ? "The music engine has no voice for " + untagged.join(", ") +
        ": the words are written in it and sung untagged."
      : "";
    // The editor shows the same list, as the line it was typed on.
    var names = [];
    list.forEach(function (l) { names.push(l.name); });
    var field = $("langnames");
    if (document.activeElement !== field) field.value = names.join(", ");
  }
  $("langsave").addEventListener("click", function () {
    langSig = "";
    act($("langsave"), [$("langstatus"), stateEl], "/languages",
      "names=" + encodeURIComponent($("langnames").value), "saving languages…");
  });

  // The page is served over plain HTTP on a home network, where the
  // clipboard API is unavailable, so a hidden textarea is the fallback
  // rather than an afterthought.
  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text);
    }
    return new Promise(function (resolve, reject) {
      var ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      var ok = false;
      try { ok = document.execCommand("copy"); } catch (e) { ok = false; }
      document.body.removeChild(ta);
      ok ? resolve() : reject(new Error("copy refused"));
    });
  }
  $("lyrcopy").addEventListener("click", function () {
    if (!lyricsText) {
      setStatus($("lyrcopystatus"), "nothing to copy yet", "err");
      return;
    }
    buzz();
    copyText(lyricsText).then(function () {
      setStatus($("lyrcopystatus"), "copied", "ok");
    })["catch"](function () {
      setStatus($("lyrcopystatus"), "could not copy - select the words and copy by hand", "err");
    });
  });

  // ---- lyric writer ------------------------------------------------
  // A small select in the steering card switches which generator pens
  // the lyrics on vocal tracks. Options come from /state; the value is
  // never clobbered while the select has focus.
  var lyrListSig = "";
  function renderLyricsGen(s) {
    var sel = $("lyricsgen");
    var list = s.lyrics_generators || [];
    var sig = JSON.stringify(list);
    if (sig !== lyrListSig) {
      lyrListSig = sig;
      sel.innerHTML = "";
      list.forEach(function (g) {
        var opt = document.createElement("option");
        opt.value = g.name;
        opt.textContent = "lyrics by " + g.name;
        if (g.blurb) opt.title = g.blurb;
        sel.appendChild(opt);
      });
    }
    if (document.activeElement !== sel && s.lyrics_generator) {
      sel.value = s.lyrics_generator;
    }
  }
  $("lyricsgen").addEventListener("change", function () {
    var name = $("lyricsgen").value;
    if (!name) return;
    act(null, $("steerstatus"), "/lyrics-gen", "name=" + encodeURIComponent(name), "switching lyric writer…");
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
  // isSaved must answer with a real boolean. classList.toggle treats an
  // undefined second argument as "no force given" and flips the class,
  // so a bare `a || b` lookup (undefined for an unsaved id) would make
  // the save buttons alternate on every poll.
  function isSaved(id) { return !!id && !!(savedIds[id] || localSaved[id]); }
  function setSavedClass(btn, id) {
    if (!btn) return;
    var on = isSaved(id);
    btn.classList.toggle("saved", !!on);
    btn.setAttribute("aria-pressed", on ? "true" : "false");
    if (btn === $("save")) {
      // Filled heart, same geometry, so nothing moves when it flips.
      $("saveglyph").innerHTML = on ? icon.heartFull : icon.heart;
      $("savelabel").textContent = on ? "Saved" : "Save";
      btn.setAttribute("aria-label", on ? "Already saved" : "Save this track");
    }
  }
  // updateSaveButtons re-derives the greyed state; called on every
  // /state poll and after local saves. s may be null to reuse the last
  // known server state.
  function updateSaveButtons(s) {
    if (s) {
      // A stopgap or a library filler leaves /state without a track,
      // but the machine's save target is still the last generated one -
      // so keeping it here is what stops the control from claiming the
      // track is saveable and then answering "already saved".
      if (s.track) lastTrack = s.track;
      if (s.prev) lastPrev = s.prev;
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
  function doSave(btn, wantPrev, statusEl) {
    statusEl = statusEl || $("savestatus");
    var ids = saveTargets();
    var id = wantPrev ? ids.prev : ids.cur;
    if (isSaved(id)) {
      setStatus(statusEl, "already saved", "ok");
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
    var body = "tag=" + encodeURIComponent(saveTag());
    if (which) body += "&which=" + encodeURIComponent(which);
    buzz();
    return act(btn, statusEl, "/save", body,
      wantPrev ? "saving the previous track…" : "saving this track…").then(function (d) {
      if (!d) return d;
      // The server says whether the track is in the snippets; a refused
      // save must not leave the control claiming it is.
      if (id && d.saved) {
        localSaved[id] = true;
        updateSaveButtons(null);
      }
      if (mode === "saved") loadChunks();
      return d;
    });
  }
  // The tag is a standing preference, not something to retype per save.
  function saveTag() { return $("tag").value.trim(); }
  $("tag").value = store.get("iar.tag", "");
  $("tag").addEventListener("change", function () { store.set("iar.tag", saveTag()); });
  $("save").addEventListener("click", function () { doSave($("save"), false, [stateEl, $("savestatus")]); });
  $("saveprev").addEventListener("click", function () { doSave($("saveprev"), true, $("savestatus")); });

  // ---- sessions ---------------------------------------------------
  $("sesssave").addEventListener("click", function () {
    var n = $("sessname").value.trim();
    if (!n) { setStatus($("sessstatus"), "type a name for this session first", "err"); return; }
    act($("sesssave"), $("sessstatus"), "/sessions/save", "name=" + encodeURIComponent(n), "saving session…").then(function (d) {
      if (d) { $("sessname").value = ""; loadSessions(); }
    });
  });

  function sessionRow(item, group) {
    var wrap = document.createElement("div");
    wrap.className = "rowwrap sess" + (item.current ? " playing" : "");
    var canLoad = me && me.new_prompt && !item.current;
    var main = document.createElement(canLoad ? "button" : "div");
    main.className = "rowbtn" + (canLoad ? " tap" : "") + (item.current ? " playing" : "");
    var title = document.createElement("span");
    title.className = "ctitle";
    title.textContent = item.name + (item.current ? "  ·  playing" : "");
    var meta = document.createElement("span");
    meta.className = "cmeta";
    meta.textContent = item.summary + (item.played ? " · " + item.played : "");
    main.appendChild(title);
    main.appendChild(meta);
    wrap.appendChild(main);
    if (canLoad) {
      // The whole row is the button: a separate Load control would be
      // a second target for the thing the row already names.
      main.type = "button";
      main.setAttribute("data-action", group === "presets" ? "Start preset" : "Load");
      main.addEventListener("click", function () {
        buzz();
        act(main, [stateEl, $("sessstatus")], "/sessions/load",
          "name=" + encodeURIComponent(item.name), "starting " + item.name + "…").then(function (d) {
          if (d) loadSessions();
        });
      });
    }
    if (me && me.admin && !item.current) {
      var del = document.createElement("button");
      del.type = "button";
      del.className = "rowicon tap danger";
      del.setAttribute("data-action", "Delete");
      del.setAttribute("aria-label", (group === "presets" ? "Hide preset " : "Delete session ") + item.name);
      del.innerHTML = SVG + 'fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">' +
        '<path d="M6 6l12 12M18 6L6 18"/></svg>';
      del.addEventListener("click", function () {
        var what = group === "presets" ? "Hide preset " : "Delete session ";
        if (!window.confirm(what + item.name + "?")) return;
        act(del, [stateEl, $("sessstatus")], "/sessions/delete",
          "name=" + encodeURIComponent(item.name), "deleting…").then(function (d) {
          if (d) loadSessions();
        });
      });
      wrap.appendChild(del);
    }
    return wrap;
  }

  // sessionGroup is one collapsible band of the station list. The count
  // rides in the summary so a closed band still says how much is in it.
  function sessionGroup(label, items, group, open) {
    var det = document.createElement("details");
    det.className = "grp";
    det.open = !!open;
    var sum = document.createElement("summary");
    sum.className = "tap";
    sum.appendChild(document.createTextNode(label));
    var count = document.createElement("span");
    count.className = "count";
    count.textContent = items.length;
    sum.appendChild(count);
    var chev = document.createElement("span");
    chev.className = "chev";
    chev.setAttribute("aria-hidden", "true");
    sum.appendChild(chev);
    det.appendChild(sum);
    if (!items.length) {
      var empty = document.createElement("span");
      empty.className = "empty";
      empty.textContent = group === "named" ? "none yet - name this session in Settings" : "none";
      det.appendChild(empty);
    }
    items.forEach(function (item) { det.appendChild(sessionRow(item, group)); });
    return det;
  }

  // presetGroups renders the presets as energy bands, the playing
  // preset's band open and the rest closed.
  function presetGroups(presets, currentPreset) {
    var frag = document.createDocumentFragment();
    if (!presets.length) {
      var empty = document.createElement("span");
      empty.className = "empty";
      empty.textContent = "no presets";
      frag.appendChild(empty);
      return frag;
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
    // With nothing playing from a preset, open the first band so the
    // list never opens as a wall of closed headings.
    if (!openGroup) openGroup = byGroup[0].name;
    byGroup.forEach(function (g) {
      frag.appendChild(sessionGroup(g.name, g.items, "presets", g.name === openGroup));
    });
    return frag;
  }

  function loadSessions() {
    fetch("/api/sessions").then(function (r) {
      if (r.status === 401) { loggedOut(); return null; }
      return r.json();
    }).then(function (d) {
      if (!d) return;
      var box = $("sessions");
      box.innerHTML = "";
      // Presets first: they are what gets started, and the owner should
      // not scroll past anything to reach them.
      box.appendChild(presetGroups(d.presets, d.current_preset));
      box.appendChild(sessionGroup("Your sessions", d.named, "named", d.named.length > 0));
      box.appendChild(sessionGroup("Auto-saved", d.auto, "auto", false));
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
      pollFails = 0;
      $("conn").textContent = "connected";
      var t = s.track;
      $("now").textContent = (t && (t.title || t.prompt)) || s.source || s.state || "...";
      $("nowprompt").textContent = (t && t.title && t.prompt) || "";
      var meta = t && t.number ? "Track " + t.number : "";
      if (t && t.subtitle) meta += (meta ? "  ·  " : "") + t.subtitle;
      if (s.session) meta += (meta ? "  ·  " : "") + s.session;
      if (s.elapsed) meta += (meta ? "  ·  " : "") + s.elapsed + " / " + s.duration;
      if (t && t.lang) meta += (meta ? "  ·  " : "") + "sung in " + t.lang;
      meta += (meta ? "  ·  " : "") + (s.ready || s.queued + " ready") + (s.generating ? " · generating" : "");

      $("meta").textContent = meta;
      // A pause at the machine is a fact about a room this listener is
      // not in and cannot act on, so it is not mentioned. What is worth
      // saying is why the music is not what they just asked for yet.
      var phaseText = "";
      if (s.phase) phaseText = s.phase + " (" + s.phase_info + ")";
      else if (s.switching) phaseText = "New setting saved - the first track in it is generating.";
      else if (s.looping) phaseText = "Replaying the last track while the next one generates.";
      $("phase").textContent = phaseText;
      renderSound(s);
      renderLyrics(s);
      renderLyricsGen(s);
      renderLanguages(s);
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
      if (audioSource === "live" && !pf.active) {
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
      // One missed poll on mobile data is normal. Saying "disconnected"
      // on the first one and taking it back on the next just strobes.
      pollFails++;
      if (pollFails >= 3) $("conn").textContent = "disconnected";
    });
  }
  var pollFails = 0;
  poll();
  setInterval(poll, 2000);

  // ---- saved chunks ------------------------------------------------
  var savedAudio = $("savedaudio");
  var chunks = [];          // everything the server has
  var tagsOn = store.get("iar.tags", {}); // tag -> selected; unknown tags default to on
  var langsOn = store.get("iar.slangs", {}); // language name -> selected; default on
  var unchecked = store.get("iar.unchecked", {}); // song key -> true when skipped
  var openKey = null;       // song whose accordion panel is open
  var playlist = [];        // checked songs in the selected tags and languages
  var current = null;       // song playing in saved mode
  var repeatOne = null;     // song looped on its own

  function tagOn(tag) { return tagsOn[tag] !== false; }
  function keyOf(c) { return c.tag + "/" + c.file; }
  function langOf(c) { return c.language_name || "unknown"; }
  function langOn(name) { return langsOn[name] !== false; }
  function isChecked(c) { return unchecked[keyOf(c)] !== true; }
  function setChecked(c, on) {
    if (on) delete unchecked[keyOf(c)]; else unchecked[keyOf(c)] = true;
    store.set("iar.unchecked", unchecked);
  }
  function visible(c) { return tagOn(c.tag) && langOn(langOf(c)); }

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
    playlist = chunks.filter(function (c) { return visible(c) && isChecked(c); });
  }

  function renderTags(tags) {
    var box = $("tags");
    box.innerHTML = "";
    if (!tags.length) {
      box.innerHTML = '<span class="empty">no saved songs yet</span>';
      return;
    }
    tags.forEach(function (tag) {
      var label = document.createElement("label");
      label.className = "chip tap" + (tagOn(tag) ? " on" : "");
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

  function renderLangs() {
    var box = $("slangs");
    box.innerHTML = "";
    var names = [];
    chunks.forEach(function (c) {
      var n = langOf(c);
      if (names.indexOf(n) < 0) names.push(n);
    });
    names.sort();
    if (!names.length) {
      box.innerHTML = '<span class="empty">no saved songs yet</span>';
      return;
    }
    names.forEach(function (name) {
      var label = document.createElement("label");
      label.className = "chip tap" + (langOn(name) ? " on" : "");
      var cb = document.createElement("input");
      cb.type = "checkbox";
      cb.checked = langOn(name);
      cb.addEventListener("change", function () {
        langsOn[name] = cb.checked;
        store.set("iar.slangs", langsOn);
        label.classList.toggle("on", cb.checked);
        rebuildPlaylist();
        renderChunks();
      });
      label.appendChild(cb);
      label.appendChild(document.createTextNode(name));
      box.appendChild(label);
    });
  }

  // chunkPost drives one curation action and reloads the listing;
  // failures land in the on-this-device line, where the eyes already are.
  function chunkPost(path, params) {
    var body = new URLSearchParams(params).toString();
    return fetch(path, { method: "POST", headers: headers, body: body })
      .then(function (r) {
        if (r.status === 401) { loggedOut(); throw new Error("logged out"); }
        return r.json().catch(function () { return {}; }).then(function (d) {
          if (!r.ok) throw new Error(d.error || ("error " + r.status));
          return d;
        });
      })
      .then(function (d) { loadChunks(); return d; })
      .catch(function (e) {
        $("savednow").className = "";
        $("savednow").textContent = "failed: " + e.message;
        return null;
      });
  }

  function tagListHint() {
    var tags = [];
    chunks.forEach(function (c) { if (tags.indexOf(c.tag) < 0) tags.push(c.tag); });
    tags.sort();
    return tags.length ? " Existing: " + tags.join(", ") : "";
  }

  function renameChunk(c) {
    var title = window.prompt("New name for this song:", c.title);
    if (title === null) return;
    title = title.trim();
    if (!title || title === c.title) return;
    chunkPost("/chunks/rename", { tag: c.tag, file: c.file, title: title });
  }

  function moveChunks(list) {
    if (!list.length) return;
    var what = list.length === 1 ? '"' + list[0].title + '"' : list.length + " songs";
    var to = window.prompt("Move " + what + " to tag (a new name makes a new group)." + tagListHint(), "");
    if (to === null) return;
    to = to.trim();
    if (!to) return;
    var params = new URLSearchParams({ to: to });
    list.forEach(function (c) { params.append("item", c.tag + "/" + c.file); });
    chunkPost("/chunks/move", params);
  }

  function deleteChunk(c) {
    if (!window.confirm('Delete "' + c.title + '" for good? The file and its lyrics are removed from the server.')) return;
    if (current && current.url === c.url) { savedAudio.pause(); current = null; }
    if (repeatOne && repeatOne.url === c.url) { repeatOne = null; updateLoopState(); }
    chunkPost("/chunks/delete", { tag: c.tag, file: c.file });
  }

  // panelFor builds the accordion panel under an open song row.
  function panelFor(c) {
    var panel = document.createElement("div");
    panel.className = "chunkpanel";
    var data = document.createElement("div");
    data.className = "paneldata";
    var bits = [langOf(c) === "unknown" ? "language unknown" : "sung in " + langOf(c),
      "tag: " + c.tag, fmtSecs(c.seconds), fmtDate(c.saved), Math.round((c.bytes || 0) / 1024 / 1024 * 10) / 10 + " MB"];
    data.textContent = bits.join(" · ");
    panel.appendChild(data);
    var fileLine = document.createElement("div");
    fileLine.className = "paneldata";
    fileLine.textContent = c.tag + "/" + c.file;
    panel.appendChild(fileLine);
    if (c.lyrics) {
      var sheet = document.createElement("pre");
      sheet.className = "lyricsheet";
      sheet.textContent = c.lyrics;
      panel.appendChild(sheet);
    }
    var actions = document.createElement("div");
    actions.className = "panelactions";
    var dl = document.createElement("a");
    dl.className = "chip tap";
    dl.href = c.url + "?dl=1";
    dl.setAttribute("download", "");
    dl.textContent = "Download";
    actions.appendChild(dl);
    var mk = function (text, fn, extra) {
      var b = document.createElement("button");
      b.type = "button";
      b.className = "chip tap" + (extra || "");
      b.textContent = text;
      b.addEventListener("click", fn);
      actions.appendChild(b);
    };
    mk("Rename", function () { renameChunk(c); });
    mk("Move…", function () { moveChunks([c]); });
    mk("Delete", function () { deleteChunk(c); }, " danger");
    panel.appendChild(actions);
    return panel;
  }

  function renderChunks() {
    var box = $("chunks");
    box.innerHTML = "";
    if (!chunks.length) {
      box.innerHTML = '<span class="empty">nothing saved yet - save a track from the live stream</span>';
      return;
    }
    var shown = 0;
    chunks.forEach(function (c) {
      if (!visible(c)) return;
      shown++;
      var checked = isChecked(c);
      var looping = repeatOne && repeatOne.url === c.url;
      var open = openKey === keyOf(c);
      var wrap = document.createElement("div");
      wrap.className = "rowwrap chunk" + (current && current.url === c.url ? " playing" : "") +
        (looping ? " looping" : "") + (checked ? "" : " unchecked");

      // The checkbox decides whether the loop plays this song.
      var check = document.createElement("label");
      check.className = "rowcheck tap";
      var cb = document.createElement("input");
      cb.type = "checkbox";
      cb.checked = checked;
      cb.setAttribute("aria-label", "Include " + c.title + " in the loop");
      cb.addEventListener("change", function () {
        setChecked(c, cb.checked);
        rebuildPlaylist();
        renderChunks();
      });
      check.appendChild(cb);
      wrap.appendChild(check);

      // The row itself opens the song's panel; playing moved to its
      // own button so details are reachable without changing the music.
      var main = document.createElement("button");
      main.type = "button";
      main.className = "rowbtn tap" + (current && current.url === c.url ? " playing" : "");
      main.setAttribute("data-action", open ? "Close" : "Details");
      main.setAttribute("aria-expanded", open ? "true" : "false");
      var title = document.createElement("span");
      title.className = "ctitle";
      title.textContent = c.title;
      var meta = document.createElement("span");
      meta.className = "cmeta";
      var langBit = langOf(c) === "unknown" ? "" : langOf(c) + " · ";
      meta.textContent = (c.subtitle ? c.subtitle + " · " : "") + langBit + c.tag + " · " + fmtSecs(c.seconds) + " · " + fmtDate(c.saved);
      main.appendChild(title);
      main.appendChild(meta);
      main.addEventListener("click", function () {
        openKey = open ? null : keyOf(c);
        renderChunks();
      });
      wrap.appendChild(main);

      var play = document.createElement("button");
      play.type = "button";
      play.className = "rowicon tap";
      play.setAttribute("data-action", "Play");
      play.setAttribute("aria-label", "Play " + c.title);
      play.innerHTML = icon.play;
      play.addEventListener("click", function () { playChunk(c); });
      wrap.appendChild(play);

      var loop = document.createElement("button");
      loop.type = "button";
      loop.className = "rowicon tap" + (looping ? " on" : "");
      loop.setAttribute("data-action", "Loop");
      loop.setAttribute("aria-pressed", looping ? "true" : "false");
      loop.setAttribute("aria-label", (looping ? "Stop looping " : "Loop ") + c.title);
      loop.innerHTML = SVG + 'fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">' +
        '<path d="M4 12a8 8 0 0 1 8-8 8 8 0 0 1 6.9 4M20 12a8 8 0 0 1-8 8 8 8 0 0 1-6.9-4"/>' +
        '<path d="M19 3v5h-5M5 21v-5h5"/></svg>';
      loop.addEventListener("click", function () { looping ? unloopOne() : loopOne(c); });
      wrap.appendChild(loop);

      box.appendChild(wrap);
      if (open) box.appendChild(panelFor(c));
    });
    if (!shown) {
      box.innerHTML = '<span class="empty">no songs match the checked tags and languages</span>';
    }
  }

  function setAllChecked(on) {
    chunks.forEach(function (c) { if (visible(c)) setChecked(c, on); });
    rebuildPlaylist();
    renderChunks();
  }
  $("scheckall").addEventListener("click", function () { setAllChecked(true); });
  $("schecknone").addEventListener("click", function () { setAllChecked(false); });
  $("smovechecked").addEventListener("click", function () {
    moveChunks(chunks.filter(function (c) { return visible(c) && isChecked(c); }));
  });

  function updateLoopState() {
    var el = $("loopstate");
    if (repeatOne) {
      el.textContent = "Looping one song: " + repeatOne.title;
      el.className = "one";
      $("backloop").hidden = false;
    } else {
      el.textContent = "Looping every checked song in the selected tags and languages";
      el.className = "";
      $("backloop").hidden = true;
    }
    savedAudio.loop = !!repeatOne;
  }

  function playChunk(c) {
    setAudioSource("saved");
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

  function unloopOne() {
    repeatOne = null;
    updateLoopState();
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
  savedAudio.addEventListener("play", function () { setSavedPlayButton(true); mediaPlaybackState("playing"); });
  savedAudio.addEventListener("pause", function () { setSavedPlayButton(false); mediaPlaybackState("paused"); });

  function loadChunks() {
    fetch("/api/chunks").then(function (r) {
      if (r.status === 401) { loggedOut(); return null; }
      return r.json();
    }).then(function (d) {
      if (!d) return;
      chunks = d.chunks || [];
      renderTags(d.tags || []);
      renderLangs();
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
    autoStarting = true;
    // Only skip the attempt when the browser says outright that this
    // page may not make sound. An untouched document is not proof of
    // that: an installed web app, a per-site allowance or a desktop
    // browser that already trusts the site will all play. Everywhere
    // else the attempt is refused and degrades to the tap.
    var policy = navigator.getAutoplayPolicy;
    if (policy && policy.call(navigator, "mediaelement") === "disallowed") {
      armGestureStart();
    } else {
      startListening();
    }
  }
})();
