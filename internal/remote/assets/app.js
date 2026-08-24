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
    stopStream("logged out - reload this page to log in again", "bad");
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
      if (wantStream) stopStream("stopped (switched to saved chunks)", "");
      loadChunks();
    }
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

  function teardownAudio() {
    if (watchdog) { clearInterval(watchdog); watchdog = null; }
    if (audio) {
      audio.onerror = null;
      audio.onended = null;
      audio.pause();
      audio.removeAttribute("src");
      audio.load();
      if (audio.parentNode) audio.parentNode.removeChild(audio);
      audio = null;
    }
  }

  function stopStream(msg, cls) {
    wantStream = false;
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

  // A connectivity change ends any backoff wait right away.
  function retryNow() {
    if (!wantStream || audio) return;
    if (retryTimer) { clearTimeout(retryTimer); retryTimer = null; }
    connectStream();
  }
  window.addEventListener("online", retryNow);
  document.addEventListener("visibilitychange", function () { if (!document.hidden) retryNow(); });
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

  playBtn.addEventListener("click", function () {
    if (wantStream) {
      stopStream("stopped", "");
      return;
    }
    startStream();
  });

  $("next").addEventListener("click", function () {
    act($("next"), $("steerstatus"), "/next", "", "skipping…");
  });

  // ---- media session (lock screen, car displays) -------------------
  var lastNow = "";
  var lastMeta = "";
  function applyMediaMetadata() {
    if (!("mediaSession" in navigator)) return;
    try {
      navigator.mediaSession.metadata = new MediaMetadata({
        title: lastNow || "Infinite AI Radio",
        artist: lastMeta || "AI-generated stream",
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
  if ("mediaSession" in navigator) {
    var handler = function (action, fn) {
      try { navigator.mediaSession.setActionHandler(action, fn); } catch (e) {}
    };
    handler("play", function () { if (mode === "live") startStream(); else savedAudio.play(); });
    handler("pause", function () { if (mode === "live") stopStream("stopped", ""); else savedAudio.pause(); });
    handler("stop", function () { if (mode === "live") stopStream("stopped", ""); else savedAudio.pause(); });
    handler("nexttrack", function () {
      if (mode === "live") {
        if (me && me.steer) act(null, $("steerstatus"), "/next", "", "skipping…");
      } else {
        repeatOne = null; updateLoopState(); step(1);
      }
    });
  }

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
  $("save").addEventListener("click", function () {
    act($("save"), $("savestatus"), "/save", "tag=" + encodeURIComponent($("tag").value.trim()), "saving this track…").then(function (d) {
      if (d && mode === "saved") loadChunks();
    });
  });

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

  function loadSessions() {
    fetch("/api/sessions").then(function (r) {
      if (r.status === 401) { loggedOut(); return null; }
      return r.json();
    }).then(function (d) {
      if (!d) return;
      var box = $("sessions");
      box.innerHTML = "";
      box.appendChild(sessionGroup("Your sessions", d.named, "named", false));
      box.appendChild(sessionGroup("Presets", d.presets, "presets", false));
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
      $("now").textContent = (s.track && s.track.prompt) || s.source || s.state || "...";
      var meta = s.session ? "session " + s.session : "";
      if (s.elapsed) meta += (meta ? "  ·  " : "") + s.elapsed + " / " + s.duration;
      meta += (meta ? "  ·  " : "") + s.queued + " ready" + (s.generating ? " · generating" : "");
      $("meta").textContent = meta;
      $("phase").textContent =
        s.phase ? s.phase + " (" + s.phase_info + ")" : (s.paused ? "paused at the machine" : "");
      renderSound(s);
      var now = (s.track && s.track.prompt) || s.source || s.state || "";
      var artist = s.session_desc || s.session || "";
      if (now !== lastNow || artist !== lastMeta) {
        lastNow = now;
        lastMeta = artist;
        if (wantStream) applyMediaMetadata();
      }
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
      meta.textContent = c.tag + " · " + fmtSecs(c.seconds) + " · " + fmtDate(c.saved);
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
    lastMeta = c.tag;
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

  setMode(mode);
})();
