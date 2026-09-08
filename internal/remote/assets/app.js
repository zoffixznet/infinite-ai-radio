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

  // ---- standby ------------------------------------------------------
  // The radio can be held: nothing played, nothing generated. It is
  // easy to forget, because a phone holding an hour of songs carries
  // on regardless - so the page says so in a bar that cannot be
  // scrolled away, and says it loudest to someone who just pressed
  // play into a radio that is not running.
  var radioStandby = false;
  function paintStandby(on) {
    var box = $("standby");
    if (box) {
      if (box.checked !== !!on) box.checked = !!on;
    }
    syncStandbyBanner();
  }
  function syncStandbyBanner() {
    var bar = $("standbybar");
    if (!bar) return;
    // Worth saying whenever the radio is held; worth saying urgently
    // to a device playing anyway - on borrowed songs, or on a stream
    // that is carrying nothing.
    var msg = "The radio is on standby. It is not making music; wake it to start again.";
    if (pf.active) {
      msg = "The radio is on standby. You are hearing songs already on this device, and it will fall silent when they run out.";
    } else if (wantStream) {
      msg = "The radio is on standby. The stream is carrying silence until you wake it.";
    }
    if (bar.hidden !== !radioStandby) bar.hidden = !radioStandby;
    setText($("standbytext"), msg);
  }
  $("standbywake").addEventListener("click", function () {
    buzz();
    act($("standbywake"), [stateEl, $("steerstatus")], "/standby", "", "waking the radio…");
  });
  $("standby").addEventListener("change", function () {
    // The checkbox reports what the radio IS; the server decides. A
    // failed call leaves it painted wrong until the next poll fixes
    // it, which is a second away.
    act(null, [stateEl, $("steerstatus")], "/standby", "",
      $("standby").checked ? "putting the radio on standby…" : "waking the radio…");
    if ($("standby").checked) {
      // Holding the radio stops this device too: otherwise the
      // listener walks away believing music is still being made.
      stopListening("stopped - the radio is on standby");
    }
  });

  $("bufflush").addEventListener("click", function () {
    // Hours of generated music, gone: worth one question first.
    if (!window.confirm("Empty the radio's buffer? Every song made ahead is thrown away, " +
      "and the radio starts generating again from one quick song.")) return;
    buzz();
    act($("bufflush"), [$("bufflushstatus"), stateEl], "/buffer/flush", "", "emptying the buffer…");
  });

  // ---- audio cue for trouble ---------------------------------------
  // A radio that quietly repeats itself looks exactly like a radio
  // that is working. Six soft beeps in a hole in the music say
  // otherwise, without being an alarm: the music ducks, the beeps
  // sound, the music comes back.
  var audioCues = store.get("iar.audiocues", true) !== false;
  // Enough beeps to be unmistakable across a room, still soft.
  var cueBeeps = 6;
  var cueUntil = 0;      // a cue is sounding; do not stack another
  var lastCueAt = 0;     // when the last one sounded
  var cueCtx = null;
  function cueTone(ctx, at, freq) {
    var osc = ctx.createOscillator();
    var gain = ctx.createGain();
    osc.type = "sine";
    osc.frequency.value = freq;
    // Shaped edges: a square-edged beep clicks.
    gain.gain.setValueAtTime(0.0001, at);
    gain.gain.exponentialRampToValueAtTime(0.25, at + 0.02);
    gain.gain.exponentialRampToValueAtTime(0.0001, at + 0.16);
    osc.connect(gain);
    gain.connect(ctx.destination);
    osc.start(at);
    osc.stop(at + 0.2);
  }
  // playTroubleCue sounds the cue and returns roughly how long the
  // music should stay out of the way, in milliseconds. Returns 0 when
  // cues are off, unsupported, or one just sounded.
  function playTroubleCue() {
    if (!audioCues) return 0;
    var now = Date.now();
    if (now < cueUntil) return 0;
    var Ctx = window.AudioContext || window.webkitAudioContext;
    if (!Ctx) return 0;
    try {
      if (!cueCtx) cueCtx = new Ctx();
      if (cueCtx.state === "suspended") cueCtx.resume();
      var lead = 0.6, gap = 0.24, t = cueCtx.currentTime + lead;
      for (var i = 0; i < cueBeeps; i++) cueTone(cueCtx, t + i * gap, 880);
    } catch (e) {
      return 0;
    }
    lastCueAt = now;
    // Lead-in silence, the beeps, and a breath before the music.
    var total = Math.round((0.6 + cueBeeps * 0.24 + 0.5) * 1000);
    cueUntil = now + total;
    return total;
  }
  // duckThrough silences whatever is playing for the cue, then brings
  // it back, so the beeps land in a hole rather than under the music.
  function duckThrough(el, ms) {
    if (!el || !ms) return;
    var was = el.volume;
    try { el.volume = 0; } catch (e) { return; }
    setTimeout(function () { try { el.volume = was; } catch (e) {} }, ms);
  }

  // clockSeconds parses the server's "m:ss" clock; -1 when it is not
  // one.
  function clockSeconds(text) {
    var m = /^(\d+):(\d\d)$/.exec(String(text || ""));
    if (!m) return -1;
    return parseInt(m[1], 10) * 60 + parseInt(m[2], 10);
  }
  // loopClock is the last elapsed reading while the stream was
  // repeating itself, so a restart can be told from a steady advance.
  var loopClock = -1;

  // say announces a transition to a screen reader. The visible status
  // line updates every couple of seconds with an elapsed count, which
  // is not something anyone wants read aloud.
  var lastSaid = "";
  // setText writes only when the text actually changed: a same-text
  // rewrite still replaces the node, which collapses any selection the
  // reader has made and makes clipboard watchers that follow the
  // selection chatter. Everything painted on a timer goes through here.
  function setText(el, text) {
    if (el && el.textContent !== text) el.textContent = text;
  }
  // The same rule for the other things a timed repaint writes. An
  // attribute or a class re-set to the value it already holds still
  // counts as a change to the accessibility tree, and an input's value
  // re-assigned to itself still moves the caret - both of which reach
  // the phone as "this page changed", every two seconds, forever.
  function setAttr(el, name, value) {
    if (el && el.getAttribute(name) !== value) el.setAttribute(name, value);
  }
  function setClass(el, name, on) {
    if (el && el.classList.contains(name) !== !!on) el.classList.toggle(name, !!on);
  }
  function setHTML(el, html) {
    if (el && el.innerHTML !== html) el.innerHTML = html;
  }
  function setValue(el, value) {
    if (el && el.value !== String(value)) el.value = value;
  }

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
    $("livetransport").hidden = !live;
    $("savedtransport").hidden = live;
    $("scroll").scrollTop = 0;
    if (live) {
      savedAudio.pause();
      // Keeping saved songs ready behind the stream needs their
      // listing, which only the saved screen used to ask for.
      if (preloadOther) loadChunks();
    } else {
      stopListening("stopped (switched to saved chunks)");
      loadChunks();
    }
    syncPrevAction();
    // Which of the two banks fills in the background follows which
    // mode is on screen.
    syncWarm();
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
    setText(stateEl, text);
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
    flushSaveQueue();
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
        streamState("playing the live stream · " + Math.floor(t) + "s", "good");
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
    wantPlay: false, // start playback as soon as anything is stored
    loop: false,     // repeat the playing track on this device
    storeFull: false,// the device refused to store a downloaded track
    // warm is the background bank: downloading upcoming songs without
    // playing any of them, so a listener sitting in the saved songs can
    // switch back to the live stream and hear something at once.
    warm: false,
    // tossed holds the songs this listener pressed Next on. They are
    // deleted from the bank, never downloaded again, and never picked -
    // pressing Next means "not this one", and a device that answers by
    // starting the same song over reads as a broken button.
    tossed: {}
  };

  // The toss list outlives a reload, because the radio goes on offering
  // the song until the stream reaches it. It is bounded and it expires:
  // a song id is only unique within a run of the radio, so an entry
  // kept for days could one day silence a different song that happens
  // to inherit the id.
  var maxTossed = 300;
  var tossKeepMs = 24 * 60 * 60 * 1000;
  (function loadTossed() {
    var saved = store.get("iar.tossed", {});
    var now = Date.now();
    Object.keys(saved || {}).forEach(function (id) {
      var rec = saved[id];
      if (!rec || typeof rec !== "object") return;
      if (!rec.at || now - rec.at > tossKeepMs) return;
      pf.tossed[id] = rec;
    });
  })();
  function saveTossed() { store.set("iar.tossed", pf.tossed); }
  // pfTossed answers for one listing row or banked record. The title
  // rides along with the id: ids restart with the radio, names do not,
  // so a name that no longer matches means this is a different song
  // wearing an old id and it deserves its chance.
  function pfTossed(id, title) {
    var rec = id && pf.tossed[id];
    if (!rec) return false;
    if (rec.title && title && rec.title !== title) return false;
    return true;
  }
  // pfToss throws one song away for good: off the device, out of the
  // listing, and onto the list of what never comes back.
  function pfToss(id, title) {
    if (!id) return;
    pf.tossed[id] = { title: title || "", at: Date.now() };
    var ids = Object.keys(pf.tossed);
    while (ids.length > maxTossed) { delete pf.tossed[ids.shift()]; }
    saveTossed();
    if (pf.have[id]) {
      try { URL.revokeObjectURL(pf.have[id].url); } catch (e) {}
      delete pf.have[id];
    }
    try { idbReq(idbStore("readwrite")["delete"](id))["catch"](function () {}); } catch (e) {}
    pf.rows = pf.rows.filter(function (row) { return row.id !== id; });
    delete pf.seen[id];
  }

  // Buffering level (per device): how far ahead to download and how
  // many finished tracks to keep banked.
  var bufLevel = store.get("iar.buflevel", "auto");
  if (bufLevel !== "eco" && bufLevel !== "max" && bufLevel !== "steady" && bufLevel !== "ultra") bufLevel = "auto";
  // Whether this device keeps a little of the mode it is NOT showing
  // ready: a few saved songs behind the live stream, a few live songs
  // behind the saved ones. Off by default, because it costs data
  // nobody asked for, and deliberately shallow - it exists so that
  // switching modes in a dead zone is not a wait, not so the device
  // downloads everything twice.
  var preloadOther = store.get("iar.preloadother", false) === true;

  // Two banks live in one database: "tracks" is the live stream's
  // queue, "chunks" is the saved songs. The upgrade names both rather
  // than assuming which one is missing - a device that never ran the
  // one-store version arrives here with nothing at all.
  function idbOpen() {
    return new Promise(function (resolve, reject) {
      var req = indexedDB.open("iar-radio", 2);
      req.onupgradeneeded = function () {
        var db = req.result;
        if (!db.objectStoreNames.contains("tracks")) db.createObjectStore("tracks", { keyPath: "id" });
        if (!db.objectStoreNames.contains("chunks")) db.createObjectStore("chunks", { keyPath: "key" });
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
  function idbNamed(name, mode) { return pf.db.transaction(name, mode).objectStore(name); }
  function idbStore(mode) { return idbNamed("tracks", mode); }
  // dbReady opens the database once and hands the same one to whoever
  // asks. Either bank may be the first to want it, and two opens racing
  // each other through the version upgrade is how a browser ends up
  // blocking one of them forever.
  var dbOpening = null;
  function dbReady() {
    if (!idbSupported) return Promise.reject(new Error("this device has no storage for songs"));
    if (pf.db) return Promise.resolve(pf.db);
    if (!dbOpening) {
      dbOpening = idbOpen().then(function (db) {
        pf.db = db;
        return db;
      })["catch"](function (e) {
        dbOpening = null;
        throw e;
      });
    }
    return dbOpening;
  }

  function pfDepth() {
    if (bufLevel === "eco") return 1;
    if (bufLevel === "steady") return 3;
    if (bufLevel === "max") return 16;
    if (bufLevel === "ultra") return 48;
    var c = navigator.connection;
    if (c && c.type === "wifi" && !c.saveData) return 5;
    return 2; // cellular, save-data, or no connection API
  }

  // pfStoreCap bounds the banked tracks on this device.
  function pfStoreCap() {
    if (bufLevel === "eco") return 4;
    if (bufLevel === "steady") return 8; // ~20 min at default track length
    if (bufLevel === "max") return 18; // ~45 min at default track length
    // Hours rather than minutes, for a flight or a long dead zone.
    // Whole songs on the device, so the ceiling is the phone's room
    // rather than ours; a phone that runs out says so.
    if (bufLevel === "ultra") return 72;
    return 8;
  }

  // warmDepth is how many songs the background bank pulls for the mode
  // nobody is listening to. A handful, not a full bank: it exists so
  // switching between the two modes has something to play at once, not
  // so the device downloads everything twice.
  var warmDepth = 3;
  // pfLive covers both reasons the live bank has work to do: someone is
  // listening to it, or it is being kept warm behind the saved songs.
  function pfLive() { return pf.active || pf.warm; }

  // pfMinutes reports how much audio is banked on this device.
  function pfMinutes() {
    var secs = 0;
    Object.keys(pf.have).forEach(function (id) { secs += pf.have[id].dur || 0; });
    return Math.round(secs / 60);
  }

  function pfShowMinutes() {
    var n = Object.keys(pf.have).length;
    setText($("bufmins"), n ? "~" + pfMinutes() + " min banked on this device" : "");
    var row = $("devrow");
    if (!row) return;
    row.hidden = !pf.active;
    if (!pf.active) return;
    setText($("devcount"), n + " song" + (n === 1 ? "" : "s") + " on this device (~" + pfMinutes() + " min)");
    var note = "";
    if (pf.wrapped) note = "replaying earlier songs, nothing new yet";
    else if (pf.storeFull) note = "no room left on this device - these play now but are not saved";
    setText($("devnote"), note);
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

  // pfAdoptRecords brings the device's stored songs back into memory.
  // Songs the listener pressed Next on stay gone: the file survived the
  // reload, the decision has to survive it too.
  function pfAdoptRecords(recs) {
    (recs || []).forEach(function (rec) {
      if (pfTossed(rec.id, rec.title)) {
        idbReq(idbStore("readwrite")["delete"](rec.id))["catch"](function () {});
        return;
      }
      if (!pf.have[rec.id]) {
        // epoch left undefined: the first queue listing decides
        // whether the record is still current and restamps it.
        pf.have[rec.id] = { url: URL.createObjectURL(rec.blob), prompt: rec.prompt, title: rec.title, subtitle: rec.subtitle, dur: rec.dur, lyrics: rec.lyrics || "" };
      }
    });
  }

  function startBuffered() {
    if (pf.active) return;
    pf.active = true;
    pf.warm = false;
    setPlayButton(true);
    pfState("preparing buffered playback…", "");
    dbReady()
      .then(function () { return idbReq(idbStore("readonly").getAll()); })
      .then(function (recs) {
        pfAdoptRecords(recs);
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
    // Nobody is listening to the live bank now, which is exactly when
    // the background one may want to take over.
    syncWarm();
  }

  // syncWarm starts and stops the background live bank. It follows the
  // setting and the mode: while the saved songs are on screen, the live
  // queue is the one worth having ready.
  function syncWarm() {
    var want = preloadOther && idbSupported && !pf.active && mode === "saved";
    if (want === pf.warm) return;
    pf.warm = want;
    if (!want) {
      if (pf.ctrl) { pf.ctrl.abort(); pf.ctrl = null; }
      if (pf.fetchTimer) { clearTimeout(pf.fetchTimer); pf.fetchTimer = null; }
      if (pf.queueTimer) { clearInterval(pf.queueTimer); pf.queueTimer = null; }
      return;
    }
    dbReady().then(function () {
      if (!pf.warm || pf.active) return;
      return idbReq(idbStore("readonly").getAll()).then(function (recs) {
        pfAdoptRecords(recs);
        pfShowMinutes();
        pfRefreshQueue();
        if (!pf.queueTimer) {
          // Slower than a listening device polls: nothing here is
          // waiting on the answer.
          pf.queueTimer = setInterval(function () { pfRefreshQueue(); }, 30000);
        }
      });
    })["catch"](function () { pf.warm = false; });
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
      if (!q || !pfLive()) return;
      pf.offline = false;
      var first = pf.epoch < 0;
      if (!first && q.epoch !== pf.epoch) {
        pfEpochChanged(q.epoch);
      }
      pf.epoch = q.epoch;
      // A song the listener pressed Next on never comes back, however
      // long the radio keeps offering it.
      pf.rows = (q.tracks || []).filter(function (row) { return !pfTossed(row.id, row.title); });
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
      // A song is named where its words are written, so the name a
      // listing carries never changes on its own. When it does change,
      // somebody renamed that song deliberately - adopt it, in the
      // record, in the store, and on the lock screen if that song is
      // the one playing.
      pf.rows.forEach(function (row) {
        var rec = pf.have[row.id];
        if (!rec || !row.title) return;
        if (rec.title === row.title && rec.subtitle === row.subtitle) return;
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
          paintNow(null);
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
      if (!pfLive()) return;
      // Offline: keep playing what is stored; the interval retries.
      pf.offline = true;
      if (!pf.active) return; // the background bank has nothing to play
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
    // The listener asked for a new setting; a held loop would transplant
    // onto the first new-context track and repeat it forever. The server
    // breaks its loop on any context change - the device does the same.
    if (pf.loop) pfSetLoop(false, true);
  }

  // trackFetchTimeout bounds one track download. Generous: a whole
  // song over a bad connection is slow, but never endless.
  var trackFetchTimeout = 90 * 1000;

  // pfEnsureDownloads keeps the store filled to the level's depth,
  // sequentially, one AbortController per download. It never recurses
  // into playback: pf.wantPlay marks that playback should start as
  // soon as anything is stored, and each completed download honours it.
  function pfEnsureDownloads() {
    if (!pfLive() || pf.ctrl) return;
    var starving = pf.wantPlay && !pf.playingId && pfNextId(null) === null;
    // The next row in play order that is not stored yet.
    var next = null;
    var passed = !pfListedPlaying();
    for (var i = 0; i < pf.rows.length; i++) {
      var row = pf.rows[i];
      if (row.id === pf.playingId) { passed = true; continue; }
      if (passed && !pf.have[row.id] && !pfTossed(row.id, row.title)) { next = row; break; }
    }
    // A bank kept warm behind the saved songs takes a few songs and
    // stops; the depth the listener chose is for the mode they are
    // actually listening to.
    var depth = pf.active ? pfDepth() : warmDepth;
    if (!next || (pfAhead() >= depth && !starving)) {
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
    // A download that never finishes would wedge the device for good:
    // one request is in flight at a time, and the next only starts
    // when this one settles. Give it a deadline so a stalled fetch
    // becomes a retry instead of a bank that stops filling.
    var ctrl = pf.ctrl;
    var deadline = setTimeout(function () {
      try { ctrl.abort(); } catch (e) {}
    }, trackFetchTimeout);
    // no-store: this device stores the song itself, in a place a flush
    // can actually empty. Left to the browser's cache there is a
    // second copy nothing here controls, and a flushed bank refills
    // from it instantly - even with the radio switched off.
    fetch(row.url, { signal: pf.ctrl.signal, cache: "no-store" }).then(function (r) {
      clearTimeout(deadline);
      if (r.status === 401 || r.status === 403) { throw { auth: true }; }
      if (!r.ok) { throw new Error("track " + r.status); }
      return r.blob();
    }).then(function (blob) {
      pf.ctrl = null;
      pf.have[row.id] = { url: URL.createObjectURL(blob), prompt: row.prompt, title: row.title, subtitle: row.subtitle, epoch: pf.epoch, dur: row.duration_s, lyrics: row.lyrics || "" };
      idbReq(idbStore("readwrite").put({ id: row.id, prompt: row.prompt, title: row.title, subtitle: row.subtitle, epoch: pf.epoch, dur: row.duration_s, lyrics: row.lyrics || "", blob: blob, saved: Date.now() }))
        .then(function () { pf.storeFull = false; })
        ["catch"](function () {
          // Out of room on the device: the song plays from memory this
          // session but will not survive a reload, which matters most
          // to the listener banking hours for a flight.
          pf.storeFull = true;
          pfShowMinutes();
        });
      pfTrimStore();
      pfShowMinutes();
      // Only a freshly generated track is worth cutting the current
      // song short for. Banked filler is older than what is playing and
      // may be in a language the listener has just switched off.
      if (pf.active && pf.switchOnDownload && row.kind !== "library") {
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
      clearTimeout(deadline);
      pf.ctrl = null;
      if (e && e.auth) {
        stopBuffered("session expired - reload this page and log in again", "bad");
        return;
      }
      if (!pfLive()) return;
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
    pf.rows.forEach(function (row) {
      if (pf.have[row.id] && !pfTossed(row.id, row.title)) ids.push(row.id);
    });
    if (!ids.length) {
      ids = Object.keys(pf.have).filter(function (id) { return !pfTossed(id, pf.have[id].title); });
    }
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

  // ---- the seek row -----------------------------------------------
  // A buffered song is a whole local file: the slider seeks it. The
  // direct live stream has no rewind, so its row is grayed and only
  // shows the machine's clock.
  function fmtClock(secs) {
    if (!isFinite(secs) || secs < 0) secs = 0;
    var m = Math.floor(secs / 60), sec = Math.floor(secs % 60);
    return m + ":" + (sec < 10 ? "0" : "") + sec;
  }
  var seekDragging = false;
  function updateSeek(el, elapsedText, durationText) {
    var row = $("seekrow");
    if (!row) return;
    if (el) {
      setClass(row, "disabled", false);
      var dur = el.duration;
      if (!isFinite(dur) || dur <= 0) return;
      if (!seekDragging) setValue($("seek"), Math.round(el.currentTime / dur * 1000));
      setText($("seeknow"), fmtClock(el.currentTime));
      setText($("seekdur"), fmtClock(dur));
      return;
    }
    setClass(row, "disabled", true);
    setValue($("seek"), 0);
    setText($("seeknow"), elapsedText || "0:00");
    setText($("seekdur"), durationText || "–:––");
  }
  $("seek").addEventListener("input", function () { seekDragging = true; });
  $("seek").addEventListener("change", function () {
    seekDragging = false;
    if (!pf.active || !pf.playingId) return;
    var el = pf.els[pf.cur];
    if (!el || !isFinite(el.duration) || el.duration <= 0) return;
    try { el.currentTime = $("seek").value / 1000 * el.duration; } catch (e) {}
  });

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
    el.loop = pf.loop;
    el.onended = function () { pfAdvance(); };
    el.ontimeupdate = function () { if (pf.playingId === id) updateSeek(el); };
    el.play().then(function () {
      autoStarting = false;
      disarmGestureStart();
      pfStatus();
      pf.played = (pf.played || 0) + 1;
      lastNow = rec.title || rec.prompt || "buffered track";
      msArtist = "Track " + pf.played + (rec.subtitle ? " · " + rec.subtitle : "");
      paintNow(null);
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
    // Nothing new has arrived and this device is about to repeat
    // itself: say so before it does, in the gap between the tracks.
    if (pf.wrapped) {
      var ms = playTroubleCue();
      if (ms) {
        pf.cur = 1 - pf.cur;
        setTimeout(function () { pfPlay(nextId); }, ms);
        return;
      }
    }
    // With a single stored track, pfPlay rewinds and replays it on the
    // other element; with two or more, the staged element takes over.
    pf.cur = 1 - pf.cur;
    pfPlay(nextId);
  }

  // pfSkip is the manual, debounced skip. It changes playback only on
  // this device; the stream and other listeners keep their position.
  var lastManualSkip = 0;
  // pfJumpLive discards every banked song and rejoins the head of the
  // server's queue: the manual escape from a device buffer full of
  // sound the listener has moved past.
  // pfJumpLive empties this device completely: every downloaded song,
  // whatever is playing, and whatever was staged to play next. What
  // comes back comes from the server, so with the server unreachable
  // the honest result is silence rather than the same bank again.
  function pfJumpLive() {
    if (!pf.active) return;
    buzz();
    // Stop the audio first. Both elements: the staged one holds the
    // next song and would otherwise play on happily through a flush.
    pf.els.forEach(function (el) {
      if (!el) return;
      el.onended = null;
      el.ontimeupdate = null;
      quiet(el);
      try {
        el.removeAttribute("src");
        el.load();
      } catch (e) {}
    });
    // Then the bank. Each blob is released whatever its neighbours do:
    // one failure used to throw straight out of the loop and leave the
    // flush half done, which is how a flushed device kept its songs.
    Object.keys(pf.have).forEach(function (id) {
      try { URL.revokeObjectURL(pf.have[id].url); } catch (e) {}
      delete pf.have[id];
    });
    pf.have = {};
    // One transaction empties the store, rather than one per song.
    try {
      idbReq(idbStore("readwrite").clear())["catch"](function () {});
    } catch (e) {}
    pf.seen = {};
    pf.wrapped = false;
    pf.storeFull = false;
    if (pf.loop) pfSetLoop(false, true);
    pf.prevId = null;
    pf.playingId = null;
    pf.wantPlay = true;
    pf.switchOnDownload = false;
    setStatus([stateEl, $("steerstatus")], "catching up with the live stream…", "ok");
    pfShowMinutes();
    pfRefreshQueue();
    pfEnsureDownloads();
  }
  $("jumplive").addEventListener("click", pfJumpLive);

  function pfSkip() {
    var now = Date.now();
    if (now - lastManualSkip < 700 || !pf.active) return;
    lastManualSkip = now;
    if (pf.loop) pfSetLoop(false, true);
    var tossId = pf.playingId;
    var rec = tossId ? pf.have[tossId] : null;
    // Picked before the song is thrown away, so what follows is the
    // next song in the listing rather than whatever happens to be first
    // once the skipped one is gone.
    var nextId = pfNextId(tossId, true);
    // Whatever happens after this, the skipped song stops here.
    var out = pf.els[pf.cur];
    if (out) {
      out.onended = null;
      out.ontimeupdate = null;
      quiet(out);
    }
    pfToss(tossId, rec && rec.title);
    if (nextId && nextId !== tossId && pf.have[nextId]) {
      setStatus([stateEl, $("steerstatus")], "skipped on this device only", "ok");
      pf.cur = 1 - pf.cur;
      pfPlay(nextId);
      return;
    }
    // Nothing else is on the device. The old answer was to start the
    // skipped song again from the top, which is exactly what the
    // listener just said they did not want to hear; the honest one is
    // the trouble beeps and a wait for the radio.
    pf.playingId = null;
    pf.wantPlay = true;
    pf.wrapped = false;
    lastNow = "";
    msArtist = "";
    updateSeek(null);
    updateSaveButtons(null);
    paintNow(null);
    applyMediaMetadata();
    playTroubleCue();
    setStatus([stateEl, $("steerstatus")],
      "skipped - nothing else on this device; waiting for the radio", "warn");
    pfShowMinutes();
    pfEnsureDownloads();
  }

  // paintNow names the song THIS listener is hearing. In buffered
  // playback that is the device's own banked track, not the machine's:
  // the phone plays its bank at its own pace, so painting the laptop's
  // track here renamed the song under a listener who was looping one.
  // The lyrics and the seek row already follow the device; the
  // now-block was the part left behind.
  // nowBase is the playing song's name without the save marker in
  // front of it, so the marker can be redrawn the instant a save is
  // asked for rather than at the next poll.
  var nowBase = "…";
  function paintNowText(text) {
    nowBase = text;
    setText($("now"), saveMark() + nowBase);
  }
  function paintSaveMark() { setText($("now"), saveMark() + nowBase); }

  function paintNow(s) {
    var rec = pf.active && pf.playingId ? pf.have[pf.playingId] : null;
    if (rec) {
      nowTitle = rec.title || "";
      paintNowText(rec.title || rec.prompt || "...");
      setText($("nowprompt"), (rec.title && rec.prompt) || "");
      var m = "Track " + (pf.played || 0);
      if (rec.subtitle) m += "  ·  " + rec.subtitle;
      setText($("meta"), m);
      return;
    }
    // Buffered playback between songs: this block names what THIS
    // device is hearing, and it is hearing nothing. Painting the
    // machine's song here would name a song the listener cannot hear.
    if (pf.active) {
      nowTitle = "";
      paintNowText("…");
      setText($("nowprompt"), "");
      setText($("meta"), "");
      return;
    }
    // Without a device track there is nothing to say until the next
    // poll brings the machine's.
    if (!s) return;
    var t = s.track;
    nowTitle = (t && t.title) || "";
    paintNowText((t && (t.title || t.prompt)) || s.source || s.state || "...");
    setText($("nowprompt"), (t && t.title && t.prompt) || "");
    var meta = t && t.number ? "Track " + t.number : "";
    if (t && t.subtitle) meta += (meta ? "  ·  " : "") + t.subtitle;
    if (t && t.lang) meta += (meta ? "  ·  " : "") + "sung in " + t.lang;
    setText($("meta"), meta);
  }

  // ---- renaming what is playing ------------------------------------
  // Names are written by a helper model that has only the words to go
  // on, and the moment a listener knows it got one wrong is while the
  // song is playing. The pencil renames it from here: the alternative
  // is the saved list, which means leaving the live page and stopping
  // the radio to fix a name. nowTitle is the song's own name as last
  // painted - never the placeholder line - so the box opens on
  // something worth editing.
  var nowTitle = "";
  function pfSetTitle(id, title) {
    var rec = pf.have[id];
    if (!rec) return;
    rec.title = title;
    idbReq(idbStore("readonly").get(id)).then(function (stored) {
      if (!stored) return;
      stored.title = title;
      return idbReq(idbStore("readwrite").put(stored));
    })["catch"](function () {});
  }
  $("rename").addEventListener("click", function () {
    // A device playing its own banked copy renames that song, not
    // whatever the machine happens to be playing.
    var id = pf.active && pf.playingId ? pf.playingId : "";
    var rec = id ? pf.have[id] : null;
    var was = rec ? (rec.title || "") : nowTitle;
    var name = window.prompt("New name for this song:", was);
    if (name === null) return;
    name = name.trim();
    if (!name || name === was) return;
    buzz();
    act($("rename"), [stateEl, $("steerstatus")], "/retitle",
      "id=" + encodeURIComponent(id) + "&title=" + encodeURIComponent(name),
      "renaming…").then(function (d) {
      if (!d || !rec) return;
      // The server has no say over this device's own copy; rename it
      // here so the screen and the lock screen change at once instead
      // of at the next listing.
      pfSetTitle(id, name);
      lastNow = name;
      paintNow(null);
      applyMediaMetadata();
      renderLyrics(null);
    });
  });

  function pfStatus() {
    // Nothing playing is its own state, and saying "playing" through it
    // is how a silent radio looks like a working one - after a flush
    // with the machine unreachable, or after a skip with nothing left
    // to skip to, most of all.
    if (!pf.playingId) {
      var empty = !Object.keys(pf.have).length;
      pfState(pf.offline
        ? (empty ? "nothing on this device and the radio is unreachable" : "offline - waiting for a song to arrive")
        : "waiting for the radio to send a song…", pf.offline ? "bad" : "");
      pfShowMinutes();
      return;
    }
    var extra = pf.offline ? " · offline, playing banked tracks" : "";
    if (!pf.offline && pf.wrapped) extra = " · replaying stored tracks, nothing new yet";
    if (pf.loop) extra += " · looping this track";
    pfState("playing (buffered) · " + pfAhead() + " ahead" + extra, pf.offline ? "bad" : "good");
    pfShowMinutes();
  }

  // ---- transport choice and the play button ------------------------
  function syncTransportUI() {
    $("buffered").checked = transport === "buffered";
    $("buflevel").value = bufLevel;
    $("preloadother").checked = preloadOther;
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
    // One level, both banks: the saved songs are kept to the same
    // depth as the stream, so "Maximum" means the same thing in both
    // modes.
    svTrim();
    svEnsure();
  });
  $("preloadother").addEventListener("change", function () {
    preloadOther = $("preloadother").checked;
    store.set("iar.preloadother", preloadOther);
    syncWarm();
    if (!preloadOther) return;
    if (chunks.length) svEnsure(); else loadChunks();
  });

  function startListening() {
    store.set("iar.wasplaying", true);
    if (transport === "buffered" && idbSupported) startBuffered(); else startStream();
    // Pressing play into a held radio is exactly when the warning has
    // to change from "it is not making music" to "and this will stop".
    syncStandbyBanner();
  }
  function stopListening(msg) {
    if (pf.active) stopBuffered(msg === undefined ? "stopped" : msg, "");
    if (wantStream) stopStream(msg === undefined ? "stopped" : msg, "");
    syncStandbyBanner();
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

  // The loop button repeats what this listener is hearing. Buffered
  // playback loops the local track on this device alone; on the direct
  // stream it asks the radio itself, which loops the room for everyone.
  function paintLoop(on) {
    var btn = $("loop");
    if (!btn) return;
    setClass(btn, "on", on);
    setAttr(btn, "aria-pressed", on ? "true" : "false");
  }
  function pfSetLoop(on, quietly) {
    pf.loop = !!on;
    var el = pf.els && pf.els[pf.cur];
    if (el) el.loop = pf.loop;
    paintLoop(pf.loop);
    if (quietly) return;
    setStatus([stateEl, $("steerstatus")],
      pf.loop ? "looping this track on this device" : "loop off - moving on when this track ends", "ok");
  }
  $("loop").addEventListener("click", function () {
    buzz();
    if (pf.active) { pfSetLoop(!pf.loop); return; }
    act($("loop"), [stateEl, $("steerstatus")], "/loop", "", "toggling the loop…");
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
  // subtitle (or the chunk's tag).
  var lastNow = "";
  var msArtist = "";
  var lastMetaSig = "";
  // saveMark is the standing answer to "did that save work?". It sits
  // in front of the song's name from the moment a save is asked for
  // until the radio has it, and then for as long as that song plays.
  // It used to be a two-second flash, which meant learning whether the
  // song was saved required watching the screen at the right instant -
  // in a car, on a bad signal, that is no answer at all.
  function saveMark() {
    if (mode !== "live") return "";
    var id = saveTargets().cur;
    if (!id) return "";
    if (isSaved(id)) return "Saved: ";
    if (savingIds[id]) return "Saving: ";
    return "";
  }
  // offlineMark says the radio itself is out of reach. A car screen
  // shows the song and nothing else, so a device playing happily out of
  // its own bank looks exactly like one the radio is still feeding -
  // right up to the moment the bank runs out. This is the difference,
  // in front of the name where it cannot be missed.
  function offlineMark() { return radioUnreachable() ? "[X] " : ""; }
  function applyMediaMetadata() {
    if (!("mediaSession" in navigator)) return;
    var title = offlineMark() + saveMark() + (lastNow || "Infinite AI Radio");
    var artist = msArtist || "AI-generated stream";
    // Handing the lock screen the words it already shows still counts
    // as a change to it, and this runs on every poll.
    var sig = title + "\u0000" + artist;
    if (sig === lastMetaSig) return;
    lastMetaSig = sig;
    try {
      navigator.mediaSession.metadata = new MediaMetadata({
        title: title,
        artist: artist,
        album: "Infinite AI Radio",
        artwork: [
          { src: "/icons/icon-192.png", sizes: "192x192", type: "image/png" },
          { src: "/icons/icon-512.png", sizes: "512x512", type: "image/png" }
        ]
      });
    } catch (e) {}
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
    // No confirmation to flash: the marker in front of the song's name
    // says "Saving:" from this instant and "Saved:" once the radio has
    // it, and it stays there for the rest of the song.
    doSave($("save"), false, [stateEl, $("savestatus")]);
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
          if (me && me.steer) act($("next"), [stateEl, $("steerstatus")], "/next", "", "skipping…");
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
  $("audiocues").checked = audioCues;
  $("audiocues").addEventListener("change", function () {
    audioCues = $("audiocues").checked;
    store.set("iar.audiocues", audioCues);
    // Sound one immediately so the listener knows what to listen for.
    if (audioCues) playTroubleCue();
  });
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
  var lyricsTitle = "";
  // Buffered playback runs on this device's own copy, which is rarely
  // the track the machine is on: the words have to follow what is
  // coming out of THIS phone, or they arrive a track late.
  // playingSong is the song whose words the panel is showing: this
  // device's own record while it plays its bank, the machine's track
  // otherwise. Returning the song rather than just its words lets the
  // panel head the lyrics with the name they belong to.
  function playingSong(s) {
    if (pf.active) {
      if (!pf.playingId) return null;
      return pf.have[pf.playingId] || null;
    }
    // Not playing this device's own bank: the panel follows the radio
    // itself. The room's speakers are singing these words right now,
    // so they show whether or not this device also streams the audio.
    return (s && s.track) || null;
  }
  function renderLyrics(s) {
    var song = playingSong(s);
    var text = (song && song.lyrics) || "";
    lyricsText = text;
    lyricsTitle = (song && song.title) || "";
    // The name is its own element, so it repaints with the song even
    // when the words happen to be unchanged.
    setText($("lyrtitle"), text ? lyricsTitle : "");
    if (text === lyricsSig) return;
    lyricsSig = text;
    var box = $("lyrics");
    box.innerHTML = "";
    if (!text) {
      var none = document.createElement("span");
      none.className = "empty";
      none.textContent = s.vocals ? "no words for this track" : "this track has no vocals";
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
      chip.className = "chip tap" + (l.on ? " on" : " off") +
        (l.configured ? "" : " foreign");
      chip.textContent = l.name;
      chip.setAttribute("aria-pressed", l.on ? "true" : "false");
      if (!l.configured) {
        // This session was saved singing in it, and still does, but the
        // machine's own list no longer offers it. Marked rather than
        // hidden: switching it off is the only way back to the list,
        // and it takes a session of its own, like any other change.
        chip.title = l.name + " is not in this machine's list of languages; " +
          "this session was saved singing in it.";
      } else if (!l.engine) {
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
    // The editor shows the machine's own list, as the line it was typed
    // on - never a language that only this session sings, or saving
    // would quietly adopt it.
    var names = [];
    list.forEach(function (l) { if (l.configured) names.push(l.name); });
    var field = $("langnames");
    if (document.activeElement !== field) setValue(field, names.join(", "));
  }
  $("langsave").addEventListener("click", function () {
    langSig = "";
    act($("langsave"), [$("langstatus"), stateEl], "/languages",
      "names=" + encodeURIComponent($("langnames").value), "saving languages…");
  });

  // The page is served over plain HTTP on a home network, where the
  // clipboard API is unavailable, so a hidden textarea is the fallback
  // rather than an afterthought. Both paths run only from a press of
  // the button below: nothing here touches the clipboard on its own.
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
    // A sheet of words with no name on it is a puzzle once it is out
    // of the page and in a notes app.
    copyText(lyricsTitle ? lyricsTitle + "\n\n" + lyricsText : lyricsText).then(function () {
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
      setValue(sel, s.lyrics_generator);
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
  var savingIds = {};     // saves asked for and not answered yet

  // ---- saves that outlive the signal -------------------------------
  // Saving is the one thing a listener does at exactly the moment the
  // signal is at its worst: a mountain road, a tunnel, a dead patch on
  // the motorway. A save that fell into one of those used to be simply
  // lost. Queued instead, it goes through by itself when the signal
  // comes back.
  //
  // Every entry names one concrete track id, never "the one playing":
  // by the time the queue drains, the one playing is a different song
  // and saving that would be saving the wrong thing. The radio keeps a
  // song reachable while it is the playing or the previous one, so the
  // honest reach of this is a few minutes - long enough for the dead
  // patches that lose saves, and no promise beyond that.
  var saveQueue = [];
  // How long a queued save is worth retrying. Past this the radio has
  // moved on and there is nothing left to save, so the entry is
  // dropped and said so rather than retried forever.
  var saveQueueMaxAge = 15 * 60 * 1000;
  var saveRetryMs = 10000;
  var saveFlushing = false;
  var saveRetryTimer = null;
  (function loadSaveQueue() {
    var stored = store.get("iar.savequeue", []);
    if (!stored || !stored.length) return;
    stored.forEach(function (item) {
      if (!item || !item.id) return;
      saveQueue.push(item);
      savingIds[item.id] = true;
    });
    // A page reloaded still off the network has to get back to these
    // on its own; the radio being reachable is the other trigger.
    scheduleSaveFlush();
  })();
  function persistSaveQueue() { store.set("iar.savequeue", saveQueue); }
  // A queued save lands minutes after the tap that asked for it, quite
  // possibly with the settings sheet long closed, so the answer goes
  // to the transport line as well as to the card it came from.
  function saveNews() { return [stateEl, $("savestatus")]; }
  function scheduleSaveFlush() {
    if (saveRetryTimer || !saveQueue.length) return;
    saveRetryTimer = setTimeout(function () {
      saveRetryTimer = null;
      flushSaveQueue();
    }, saveRetryMs);
  }
  function dropQueuedSave(id) {
    var before = saveQueue.length;
    saveQueue = saveQueue.filter(function (item) { return item.id !== id; });
    if (saveQueue.length !== before) persistSaveQueue();
  }
  // postSave is the one place a save reaches the radio. It marks the
  // failures that are worth queueing - the request never arrived, or
  // the radio is up but not answering - apart from the ones that mean
  // the song is simply gone, which no amount of retrying fixes.
  function postSave(id, tag) {
    var body = "tag=" + encodeURIComponent(tag || "");
    if (id) body += "&which=" + encodeURIComponent(id);
    return fetch("/save", { method: "POST", headers: headers, body: body })
      .then(function (r) {
        if (r.status === 401) { loggedOut(); throw new Error("logged out"); }
        return r.json().catch(function () { return {}; }).then(function (d) {
          if (r.status >= 500) {
            var busy = new Error(d.error || ("error " + r.status));
            busy.offline = true;
            throw busy;
          }
          if (!r.ok) throw new Error(d.error || d.ack || ("error " + r.status));
          return d;
        });
      }, function () {
        var gone = new Error("no connection to the radio");
        gone.offline = true;
        throw gone;
      });
  }
  function queueSave(id, tag, title) {
    if (!id) return;
    for (var i = 0; i < saveQueue.length; i++) {
      if (saveQueue[i].id === id) return;
    }
    saveQueue.push({ id: id, tag: tag || "", title: title || "", at: Date.now() });
    persistSaveQueue();
    savingIds[id] = true;
    updateSaveButtons(null);
    scheduleSaveFlush();
  }
  function saveSettled(id, ok) {
    delete savingIds[id];
    if (ok) localSaved[id] = true;
    updateSaveButtons(null);
  }
  // flushSaveQueue drains the queue oldest first, one at a time, and
  // stops at the first entry the network refuses so the rest keep
  // their order and their turn.
  function flushSaveQueue() {
    if (saveFlushing || !saveQueue.length) return;
    var item = saveQueue[0];
    var named = item.title || "an earlier song";
    if (Date.now() - (item.at || 0) > saveQueueMaxAge) {
      saveQueue.shift();
      persistSaveQueue();
      delete savingIds[item.id];
      updateSaveButtons(null);
      setStatus(saveNews(), "gave up saving " + named + " - the radio has moved past it", "err");
      flushSaveQueue();
      return;
    }
    saveFlushing = true;
    postSave(item.id, item.tag).then(function (d) {
      saveFlushing = false;
      saveQueue.shift();
      persistSaveQueue();
      var ok = !!(d && d.saved);
      saveSettled(item.id, ok);
      setStatus(saveNews(), ok ? "saved " + named : "could not save " + named, ok ? "ok" : "err");
      say(ok ? "saved " + named : "could not save " + named);
      if (mode === "saved") loadChunks();
      flushSaveQueue();
    })["catch"](function (e) {
      saveFlushing = false;
      if (e && e.offline) { scheduleSaveFlush(); return; }
      saveQueue.shift();
      persistSaveQueue();
      saveSettled(item.id, false);
      setStatus(saveNews(), "could not save " + named + ": " + e.message, "err");
      flushSaveQueue();
    });
  }
  window.addEventListener("online", flushSaveQueue);

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
    // A save waiting on the signal is neither saved nor not saved, and
    // the control has to say so or the listener presses it again.
    var waiting = !on && !!(id && savingIds[id]);
    setClass(btn, "saved", on);
    setClass(btn, "saving", waiting);
    setAttr(btn, "aria-pressed", on ? "true" : "false");
    if (btn === $("save")) {
      // Filled heart, same geometry, so nothing moves when it flips.
      setHTML($("saveglyph"), on ? icon.heartFull : icon.heart);
      setText($("savelabel"), waiting ? "Saving" : (on ? "Saved" : "Save"));
      setAttr(btn, "aria-label", waiting ? "Saving this track" : (on ? "Already saved" : "Save this track"));
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
      // The radio can have a song safe before its answer gets back to
      // this device - a save whose reply was lost with the signal is
      // still a save, and retrying it would be asking twice.
      Object.keys(savingIds).forEach(function (id) {
        if (!savedIds[id]) return;
        delete savingIds[id];
        dropQueuedSave(id);
      });
    }
    var ids = saveTargets();
    setSavedClass($("save"), ids.cur);
    setSavedClass($("saveprev"), ids.prev);
    // The marker in front of the song's name is derived from exactly
    // this state, so it is repainted from exactly here.
    paintSaveMark();
    applyMediaMetadata();
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
    // Always a concrete track id, even where the old code could get
    // away with "the one playing": a save that waits out a dead patch
    // has to name the song the listener meant, not whichever song is
    // playing when the signal returns.
    if (!id) {
      setStatus(statusEl, wantPrev
        ? "no previous track to save yet"
        : (pf.active ? "nothing is playing on this device yet" : "nothing is playing yet"), "err");
      return Promise.resolve(null);
    }
    if (savingIds[id]) {
      setStatus(statusEl, "already saving that track", "ok");
      return Promise.resolve({ ack: "already saving" });
    }
    var tag = saveTag();
    var named = wantPrev ? ((lastPrev && lastPrev.title) || "") : (lastNow || "");
    buzz();
    savingIds[id] = true;
    updateSaveButtons(null);
    if (btn) {
      btn.setAttribute("aria-disabled", "true");
      btn.classList.add("is-working");
      releasePress();
    }
    setStatus(statusEl, wantPrev ? "saving the previous track…" : "saving this track…", "");
    return postSave(id, tag).then(function (d) {
      saveSettled(id, !!(d && d.saved));
      setStatus(statusEl, d.ack || "done", "ok");
      say(d.ack || "done");
      if (mode === "saved") loadChunks();
      return d;
    })["catch"](function (e) {
      if (e && e.offline) {
        queueSave(id, tag, named);
        setStatus(statusEl, "no signal - queued; this song saves itself when the radio is reachable", "warn");
        say("queued to save when the connection is back");
        return { queued: true };
      }
      saveSettled(id, false);
      setStatus(statusEl, "failed: " + e.message, "err");
      say("failed: " + e.message);
      return null;
    }).then(function (d) {
      if (btn) { btn.removeAttribute("aria-disabled"); btn.classList.remove("is-working"); }
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
    // The generated-name band fills up one session per change to the
    // sound, so it gets the one gesture that empties it; deleting them
    // a row at a time is not a realistic option.
    if (group === "auto" && me && me.admin && items.length) {
      var wipe = document.createElement("button");
      wipe.type = "button";
      wipe.className = "chip tap danger";
      wipe.id = "autowipe";
      wipe.setAttribute("data-action", "Delete automatic");
      wipe.textContent = "Delete all";
      wipe.addEventListener("click", function (e) {
        // Inside a summary, a click would otherwise fold the band.
        e.preventDefault();
        e.stopPropagation();
        if (!window.confirm("Delete all " + items.length + " automatic sessions? The one playing is kept.")) return;
        act(wipe, [stateEl, $("sessstatus")], "/sessions/delete-auto", "", "deleting…").then(function (d) {
          if (d) loadSessions();
        });
      });
      sum.appendChild(wipe);
    }
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

  // lastSession is the session name the station list was drawn for; a
  // change to it means the list is out of date.
  var lastSession = "";
  var sessionsLoaded = false;
  function loadSessions() {
    sessionsLoaded = true;
    fetch("/api/sessions").then(function (r) {
      if (r.status === 401) { loggedOut(); return null; }
      return r.json();
    }).then(function (d) {
      if (!d) return;
      // The list is now drawn for this session, so the next poll has
      // nothing to catch up on.
      lastSession = d.current || "";
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
      setText($("conn"), "connected");
      // Proof the radio is reachable: whatever a dead patch swallowed
      // can go now.
      if (saveQueue.length) flushSaveQueue();
      paintNow(s);
      // Every change to the sound branches the session, so the name in
      // the status line is also how the station list learns that what
      // is playing has moved - including when someone changed it from
      // the machine itself.
      if ((s.session || "") !== lastSession) {
        lastSession = s.session || "";
        if (sessionsLoaded) loadSessions();
      }
      var srv = s.session || "";
      srv += (srv ? "  ·  " : "") + (s.ready || s.queued + " ready") + (s.generating ? " · generating" : "");
      setText($("srvline"), srv);
      if (!pf.active) updateSeek(null, s.elapsed, s.duration);
      // A pause at the machine is a fact about a room this listener is
      // not in and cannot act on, so it is not mentioned. What is worth
      // saying is why the music is not what they just asked for yet.
      var phaseText = "";
      if (s.phase) phaseText = s.phase + " (" + s.phase_info + ")";
      else if (s.switching) phaseText = "New setting saved - the first track in it is generating.";
      else if (s.loop_on) phaseText = pf.active
        ? "The radio itself is looping its playing track; this device plays its own bank."
        : "Looping this track until the loop is turned off.";
      else if (s.looping) phaseText = "Replaying the last track while the next one generates.";
      // Cue the stream only at the seam, never over a playing song:
      // the looped track starting again is the moment worth marking,
      // and the elapsed clock running backwards is that moment.
      var at = clockSeconds(s.elapsed);
      if (s.looping && !pf.active && wantStream && at >= 0) {
        if (loopClock >= 0 && at < loopClock) {
          duckThrough(audio, playTroubleCue());
        }
        loopClock = at;
      } else {
        loopClock = -1;
      }
      setText($("phase"), phaseText);
      radioStandby = !!s.standby;
      paintStandby(radioStandby);
      paintLoop(pf.active ? pf.loop : !!s.loop_on);
      renderSound(s);
      renderLyrics(s);
      renderLyricsGen(s);
      renderLanguages(s);
      updateSaveButtons(s);
      if (pf.active && pf.epoch >= 0 && s.epoch !== pf.epoch) {
        pfRefreshQueue();
      }
      if (pf.active) pfStatus();
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
      // One missed poll on mobile data is normal. Saying "disconnected"
      // on the first one and taking it back on the next just strobes.
      pollFails++;
      if (pollFails >= 3) setText($("conn"), "disconnected");
      // A failed poll is the only place the car's marker can appear:
      // the successful path repaints the metadata on its way through
      // updateSaveButtons, and this one has to do it for itself.
      applyMediaMetadata();
    });
  }
  var pollFails = 0;
  // radioUnreachable is the same fact the connection line reports, and
  // it takes three failed polls - about six seconds - rather than one,
  // because mobile data drops the odd request on a good day and a mark
  // that blinks is a mark a driver learns to ignore.
  function radioUnreachable() { return pollFails >= 3; }
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
  var rowPlay = [];         // per-row play buttons, so they can be repainted

  // ---- the saved songs' own bank -----------------------------------
  // The saved songs used to be streamed off the radio on the tap that
  // asked for them: press play, wait, hear it stutter, then wait again
  // at every song change - which on a car's connection is most of the
  // listening. They get what the live stream gets now: whole songs
  // downloaded ahead of time into this device's own store, to the same
  // per-device buffering level, so "Maximum" means maximum in both
  // modes and a handful of saved songs is simply all of them.
  var sv = {
    have: {},        // key -> {url, bytes}
    ctrl: null,
    timer: null,
    full: false,     // the device refused to store a downloaded song
    adopted: false   // the store has been read back at least once
  };

  function svCap() { return pfStoreCap(); }
  // svWant is how many songs to have ready, counted from the one
  // playing. Behind the live stream it is a handful - enough that
  // switching over plays at once, not a second full bank.
  function svWant() { return mode === "saved" ? svCap() : Math.min(warmDepth, svCap()); }

  // svOrder is the order worth downloading in: the song playing, then
  // the loop in the order it will play it, then everything else, which
  // is still one tap away on its own row.
  function svOrder() {
    var out = [];
    var seen = {};
    function push(c) {
      var k = keyOf(c);
      if (seen[k]) return;
      seen[k] = true;
      out.push(c);
    }
    var at = 0;
    if (current) {
      chunks.forEach(function (c) { if (c.url === current.url) push(c); });
      playlist.forEach(function (c, i) { if (c.url === current.url) at = i + 1; });
    }
    for (var i = 0; i < playlist.length; i++) push(playlist[(at + i) % playlist.length]);
    chunks.forEach(push);
    return out;
  }

  function svForget(key) {
    if (sv.have[key]) {
      try { URL.revokeObjectURL(sv.have[key].url); } catch (e) {}
      delete sv.have[key];
    }
    try { idbReq(idbNamed("chunks", "readwrite")["delete"](key))["catch"](function () {}); } catch (e) {}
  }

  // svTrim caps the bank at the level's ceiling, oldest first, and
  // never drops the song currently playing out of under it.
  function svTrim() {
    if (!idbSupported || !pf.db) return;
    var cap = svCap();
    var playing = current ? keyOf(current) : "";
    idbReq(idbNamed("chunks", "readonly").getAll()).then(function (recs) {
      if (!recs || recs.length <= cap) return;
      recs.sort(function (a, b) { return a.saved - b.saved; });
      recs.slice(0, recs.length - cap).forEach(function (rec) {
        if (rec.key === playing) return;
        svForget(rec.key);
      });
      svShow();
    })["catch"](function () {});
  }

  // svPrune drops what the radio no longer lists: a deleted song, or
  // one whose rename moved its file out from under the name the bank
  // knows it by.
  function svPrune() {
    var known = {};
    chunks.forEach(function (c) { known[keyOf(c)] = true; });
    Object.keys(sv.have).forEach(function (k) { if (!known[k]) svForget(k); });
  }

  // svAdopt reads the store back after a reload, so a device that
  // banked its songs yesterday still has them today.
  function svAdopt() {
    if (!idbSupported) return Promise.resolve();
    return dbReady().then(function () {
      return idbReq(idbNamed("chunks", "readonly").getAll());
    }).then(function (recs) {
      var known = {};
      chunks.forEach(function (c) { known[keyOf(c)] = true; });
      (recs || []).forEach(function (rec) {
        if (!known[rec.key]) { svForget(rec.key); return; }
        if (!sv.have[rec.key]) {
          sv.have[rec.key] = { url: URL.createObjectURL(rec.blob), bytes: rec.blob.size };
        }
      });
      sv.adopted = true;
      svShow();
    })["catch"](function () {});
  }

  function svShow() {
    var n = Object.keys(sv.have).length;
    var note = n ? n + " song" + (n === 1 ? "" : "s") + " ready on this device" : "";
    if (sv.full) note += (note ? "  ·  " : "") + "no room left on this device for more";
    setText($("savedbank"), note);
  }

  // svEnsure downloads the next song the level asks for, one at a time.
  function svEnsure() {
    if (!idbSupported || sv.ctrl || !chunks.length) return;
    if (mode !== "saved" && !preloadOther) return;
    var want = svWant();
    var order = svOrder();
    var next = null;
    var banked = 0;
    for (var i = 0; i < order.length && banked < want; i++) {
      if (sv.have[keyOf(order[i])]) { banked++; continue; }
      next = order[i];
      break;
    }
    if (!next) { svShow(); return; }
    var song = next;
    var key = keyOf(song);
    var ctrl = new AbortController();
    sv.ctrl = ctrl;
    var deadline = setTimeout(function () { try { ctrl.abort(); } catch (e) {} }, trackFetchTimeout);
    dbReady().then(function () {
      // no-store for the same reason the live bank uses it: this device
      // keeps the song itself, in a place it can actually empty.
      return fetch(song.url, { signal: ctrl.signal, cache: "no-store" });
    }).then(function (r) {
      clearTimeout(deadline);
      if (r.status === 401 || r.status === 403) { throw { auth: true }; }
      if (!r.ok) throw new Error("song " + r.status);
      return r.blob();
    }).then(function (blob) {
      sv.ctrl = null;
      sv.have[key] = { url: URL.createObjectURL(blob), bytes: blob.size };
      return idbReq(idbNamed("chunks", "readwrite").put({ key: key, blob: blob, saved: Date.now() }))
        .then(function () { sv.full = false; })
        ["catch"](function () {
          // Out of room: the song is ready this session but will not
          // survive a reload, which is worth saying rather than
          // discovering on the next drive.
          sv.full = true;
        });
    }).then(function () {
      svTrim();
      svShow();
      svEnsure();
    })["catch"](function (e) {
      clearTimeout(deadline);
      sv.ctrl = null;
      if (e && e.auth) { loggedOut(); return; }
      // Off the network, or the file has gone: the timer retries
      // rather than a tight loop hammering a dead connection.
      if (sv.timer) return;
      sv.timer = setTimeout(function () { sv.timer = null; svEnsure(); }, 5000);
    });
  }

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
    svForget(keyOf(c));
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
    rowPlay = [];
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
      play.addEventListener("click", function () { toggleChunk(c); });
      wrap.appendChild(play);
      rowPlay.push({ chunk: c, btn: play, wrap: wrap });

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
      rowPlay = [];
      return;
    }
    paintRowPlay();
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
    current = c;
    // The device's own copy when it has one - that is the whole point
    // of banking them - and the radio's otherwise.
    var banked = sv.have[keyOf(c)];
    savedAudio.src = banked ? banked.url : c.url;
    savedAudio.play().catch(function (e) { $("savednow").textContent = "could not play: " + e.message; });
    $("savednow").className = "";
    $("savednow").textContent = c.title + "  (" + c.tag + ")";
    lastNow = c.title;
    msArtist = c.subtitle ? c.subtitle + " · " + c.tag : c.tag;
    applyMediaMetadata();
    renderChunks();
    // Playing moves the head of the queue, so what is worth having
    // ready moves with it.
    svEnsure();
  }

  // toggleChunk is what a row's own button does. It doubles as the
  // pause, because a song that takes a moment to start is
  // indistinguishable from a button that did nothing - and the pause
  // it performs is the player's own, not a second kind of pause.
  function toggleChunk(c) {
    if (current && current.url === c.url && savedAudio.src) {
      if (savedAudio.paused) {
        savedAudio.play()["catch"](function (e) {
          $("savednow").textContent = "could not play: " + e.message;
        });
      } else {
        savedAudio.pause();
      }
      return;
    }
    playChunk(c);
  }
  function chunkPlaying(c) {
    return !!(current && current.url === c.url && savedAudio.src && !savedAudio.paused);
  }
  // paintRowPlay repaints the rows' own buttons without rebuilding the
  // list: a play or pause must not collapse the panel someone has open.
  function paintRowPlay() {
    rowPlay.forEach(function (row) {
      var on = chunkPlaying(row.chunk);
      setHTML(row.btn, on ? icon.pause : icon.play);
      setAttr(row.btn, "data-action", on ? "Pause" : "Play");
      setAttr(row.btn, "aria-label", (on ? "Pause " : "Play ") + row.chunk.title);
      setClass(row.wrap, "playing", !!(current && current.url === row.chunk.url));
    });
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
  savedAudio.addEventListener("play", function () { setSavedPlayButton(true); mediaPlaybackState("playing"); paintRowPlay(); });
  savedAudio.addEventListener("pause", function () { setSavedPlayButton(false); mediaPlaybackState("paused"); paintRowPlay(); });

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
      // The bank follows the listing: songs the radio no longer has go,
      // and whatever the buffering level asks for is fetched.
      if (sv.adopted) { svPrune(); svEnsure(); } else { svAdopt().then(svEnsure); }
    }).catch(function () {});
  }
  // The listing is worth refreshing in live mode too when this device
  // keeps saved songs ready behind it.
  setInterval(function () { if (mode === "saved" || preloadOther) loadChunks(); }, 30000);

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
