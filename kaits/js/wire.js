// wire.js — the phone half of the protocol defined in the daemon's protocol.go.
// Keep the type strings IN SYNC with that file. This is the only thing that
// talks to the network; the UI subscribes to events via Wire.on(type, fn).

(function () {
  var C = window.CONFIG;

  // ---- message type constants (must match daemon protocol.go) ----
  var T = {
    // daemon -> app
    READY: "ready", PAIRED: "paired", QR: "qr",
    MESSAGE: "message", RECEIPT: "receipt", CHATLIST: "chatlist",
    HISTORY: "history", PRESENCE: "presence",
    CALLOFFER: "calloffer", CALLSTATE: "callstate", CALLSIGNAL: "callsignal",
    ERROR: "error",
    PROFILE: "profile", CHATUPDATE: "chatupdate",
    REACTION: "reaction", STATUS: "status", TYPING: "typing",
    SEARCHRESULT: "searchresult", EDITED: "edited", LIVELOCSTATE: "livelocstate",
    STICKERS: "stickers", GIFRESULTS: "gifresults",
    // app -> daemon
    SEND: "send", GETCHATS: "getchats", GETHISTORY: "gethistory",
    MARKREAD: "markread",
    DELETE: "delete", FORWARD: "forward",
    CHATACTION: "chataction", GETPROFILE: "getprofile", SAVECONTACT: "savecontact",
    SENDREACTION: "sendreaction", SEARCH: "search", WATCH: "watch",
    GETSTICKERS: "getstickers", GIFSEARCH: "gifsearch",
    PUSHSUB: "pushsub", EDIT: "edit", LIVELOC: "liveloc",
    CALLANSWER: "callanswer", CALLREJECT: "callreject",
    CALLDIAL: "calldial", CALLHANGUP: "callhangup"
  };

  var ws = null;
  var backoff = C.RECONNECT_MIN;
  var listeners = {};   // type -> [fn]
  var pendingFrames = [];  // frames received before a handler existed
  var statusFns = [];   // each called with "connecting"|"open"|"closed"
  var queuedFn = null;  // called with the number of frames waiting to go out
  var reqId = 0;

  function emit(type, data) {
    var fns = listeners[type] || [];
    if (fns.length === 0) { pendingFrames.push([type, data]); return; }
    for (var i = 0; i < fns.length; i++) {
      try { fns[i](data); } catch (e) { console.error("listener " + type, e); }
    }
  }

  function setStatus(s) {
    console.log("wire: " + s);
    // A list, not a slot: app.js and push.js both need to know, and a setter
    // would silently leave whichever registered first with no callbacks.
    for (var i = 0; i < statusFns.length; i++) {
      try { statusFns[i](s); } catch (e) { console.error("status listener", e); }
    }
  }

  function url() {
    var sep = C.DAEMON_WS.indexOf("?") >= 0 ? "&" : "?";
    return C.DAEMON_WS + sep + "token=" + encodeURIComponent(C.TOKEN);
  }

  // ---- why isn't it connecting? ----
  //
  // A socket that never opens looks identical to one the daemon refused, to a
  // hostname that doesn't resolve, to a token off by one character. All four
  // show the same thing on the phone: a chat list that says "waiting for
  // messages" forever. There is no console on the device, so unless the app
  // records what happened, nobody can tell those apart — which is exactly the
  // position a second handset put us in.
  //
  // So: keep a small ledger of every attempt, and expose it to the settings
  // screen. None of this changes behaviour; it only makes failure legible.
  var diag = {
    attempts: 0,
    everOpened: false,
    lastOpenAt: 0,
    lastCloseAt: 0,
    lastCloseCode: 0,
    lastCloseClean: null,
    lastError: ""
  };

  // httpURL turns the WebSocket address into the plain HTTP one at the same
  // place, so the daemon can be asked a question a failed socket can't answer.
  // Order matters: wss must be tested first or ws:// would match it.
  function httpURL() {
    var u = String(C.DAEMON_WS || "");
    if (!u) return "";
    if (/^wss:/i.test(u)) return u.replace(/^wss:/i, "https:");
    if (/^ws:/i.test(u)) return u.replace(/^ws:/i, "http:");
    return "";
  }

  // classify turns an HTTP status from that address into the one fact worth
  // knowing. The daemon checks the token BEFORE attempting the upgrade, so a
  // plain GET separates "wrong token" from "wrong address" cleanly:
  //
  //   401  token rejected — we reached the daemon, so the address is right
  //   400  gorilla refusing a non-websocket GET — address AND token are right,
  //        and the problem is the socket itself
  //   404  something answered, but it isn't wad — wrong path, or another site
  //        on that hostname
  //   5xx  a reverse proxy is up and the daemon behind it is not
  function classify(status) {
    if (status === 401) return { kind: "badtoken", status: status };
    if (status === 400 || status === 426) return { kind: "ok", status: status };
    if (status === 404) return { kind: "notwad", status: status };
    if (status >= 500) return { kind: "daemondown", status: status };
    if (status === 0) return { kind: "unreachable", status: 0 };
    return { kind: "odd", status: status };
  }

  // probe asks that question. mozSystem lifts the same-origin rule, which a
  // packaged app needs to reach any host at all from XHR; the daemon sends no
  // CORS headers, so without it every answer would come back as a network
  // error and the probe would accuse a perfectly good server. Whether we got
  // it is reported alongside the result rather than assumed.
  function probe(cb) {
    var base = httpURL();
    if (!base) { cb({ kind: "noaddress", status: 0, mozSystem: false }); return; }
    var sep = base.indexOf("?") >= 0 ? "&" : "?";
    var full = base + sep + "token=" + encodeURIComponent(C.TOKEN);

    var xhr;
    try { xhr = new XMLHttpRequest({ mozSystem: true }); }
    catch (e) { try { xhr = new XMLHttpRequest(); } catch (e2) { cb({ kind: "noxhr", status: 0, mozSystem: false }); return; } }
    var system = xhr.mozSystem === true;

    var done = false;
    function finish(r) {
      if (done) return;
      done = true;
      r.mozSystem = system;
      try { cb(r); } catch (e) { console.error("probe callback", e); }
    }
    xhr.onload = function () { finish(classify(xhr.status)); };
    xhr.onerror = function () { finish({ kind: "unreachable", status: 0 }); };
    xhr.ontimeout = function () { finish({ kind: "timeout", status: 0 }); };
    try {
      xhr.open("GET", full, true);
      xhr.timeout = 8000;
      xhr.send();
    } catch (e) {
      finish({ kind: "unreachable", status: 0 });
    }
    // xhr.timeout is honoured by some builds and not others, and a probe that
    // never calls back leaves the settings screen reading "asking…" for ever —
    // which is a worse answer than a wrong one. Our own deadline, slightly
    // longer, so the XHR's own timeout wins when it works.
    if (!done && typeof setTimeout === "function") {
      setTimeout(function () {
        if (done) return;
        try { xhr.abort(); } catch (e) {}
        finish({ kind: "timeout", status: 0 });
      }, 10000);
    }
  }

  // Exactly one dial may be in flight. Without this, a kick and an already
  // pending backoff timer both call connect() and the phone ends up with two
  // sockets — the daemon adopts the newest and closes the older, which logs a
  // disconnect and looks precisely like the connection dropping on its own.
  var retryTimer = null;

  function connect() {
    if (ws && (ws.readyState === WebSocket.OPEN ||
               ws.readyState === WebSocket.CONNECTING)) return;
    if (retryTimer) { clearTimeout(retryTimer); retryTimer = null; }
    setStatus("connecting");
    diag.attempts++;
    try {
      ws = new WebSocket(url());
    } catch (e) {
      console.error("wire: construct failed", e);
      // A throw here means the ADDRESS is malformed — a bad scheme, a stray
      // space. Worth keeping, because it never reaches onclose and so leaves
      // no other trace.
      diag.lastError = "bad address: " + (e && e.message ? e.message : e);
      scheduleReconnect();
      return;
    }

    ws.onopen = function () {
       backoff = C.RECONNECT_MIN;
       diag.everOpened = true;
       diag.lastOpenAt = Date.now();
       diag.lastError = "";
       setStatus("open");
       // Anything typed while offline goes out before we ask for state, so the
       // chat list we get back already reflects it.
       flushOutbox();
       setTimeout(function () { if (ws && ws.readyState === WebSocket.OPEN) send(T.GETCHATS, null); }, 400);
     };

    ws.onmessage = function (ev) {
      var env;
      try { env = JSON.parse(ev.data); }
      catch (e) { console.error("wire: bad frame", ev.data); return; }
      emit(env.t, env.data);
    };

    ws.onclose = function (ev) {
      // The close code is the single most informative number the platform
      // gives us. 1006 — no close frame — means the connection died below the
      // WebSocket layer: DNS, TLS, a refused port, or an HTTP error instead of
      // an upgrade. Anything else means a real close frame arrived and the far
      // end was genuinely a WebSocket server.
      diag.lastCloseAt = Date.now();
      diag.lastCloseCode = (ev && typeof ev.code === "number") ? ev.code : 0;
      diag.lastCloseClean = (ev && typeof ev.wasClean === "boolean") ? ev.wasClean : null;
      setStatus("closed");
      scheduleReconnect();
    };

    ws.onerror = function (e) {
      // onclose will follow; just log.
      console.error("wire: socket error", e);
      diag.lastError = "socket error";
    };
  }

  function scheduleReconnect() {
    if (retryTimer) return;            // one pending retry is enough
    retryTimer = setTimeout(function () {
      retryTimer = null;
      connect();
    }, backoff);
    backoff = Math.min(backoff * 2, C.RECONNECT_MAX);
  }

  // Reconnect the moment the phone can plausibly reach the daemon again.
  //
  // The socket does not usually fail because the daemon went away — it fails
  // because the PHONE went away. KaiOS drops Wi-Fi when the screen goes off,
  // and the daemon then sees "no route to host": the app is still running, its
  // socket is simply pointed at a network the phone is no longer on. Waiting
  // out a backoff is the wrong response to that, because the backoff timer is
  // itself throttled while backgrounded, so the app can sit disconnected long
  // after the network has come back.
  //
  // Both of these mean "something just changed in our favour": the radio
  // reassociated, or the user woke the phone. Either way, try immediately and
  // start the backoff over.
  function kick(why) {
    if (ws && (ws.readyState === WebSocket.OPEN ||
               ws.readyState === WebSocket.CONNECTING)) return;
    console.log("wire: kick (" + why + ")");
    backoff = C.RECONNECT_MIN;
    connect();                         // clears any pending retry itself
  }

  if (window.addEventListener) {
    window.addEventListener("online", function () { kick("network back"); }, false);
    document.addEventListener("visibilitychange", function () {
      if (!document.hidden) kick("app visible");
    }, false);
    document.addEventListener("mozvisibilitychange", function () {
      if (!document.mozHidden) kick("app visible");
    }, false);
  }

  // ---- outbox ----
  // Frames that matter are queued while the socket is down and flushed on
  // reconnect. Without this they were logged and thrown away: you'd type a
  // message, the composer would clear, and it would simply never exist. On a
  // phone with real signal that isn't an edge case.
  //
  // Only frames worth replaying are queued. Re-sending a stale "getchats" or a
  // typing indicator from two minutes ago is noise at best, so those are
  // dropped as before.
  var QUEUEABLE = {
    send: true, delete: true, forward: true, chataction: true,
    savecontact: true, sendreaction: true, markread: true, edit: true,
    // The push endpoint is produced by the service worker, which registers as
    // soon as the app loads — reliably BEFORE the socket is up. Dropping it
    // meant the daemon never learned where to wake the phone, and nothing said
    // so beyond one line in a console nobody can read on the device.
    pushsub: true
    // NOT liveloc: a position is only true when it is fresh. Queueing one
    // through a reconnect would replay where you were, as though it were where
    // you are. Dropping it costs one 30-second tick.
  };
  var outbox = [];
  var OUTBOX_MAX = 50;
  // Matches the daemon's own read limit. Anything past this cannot be
  // delivered, so it is refused rather than stored.
  var MAX_FRAME_BYTES = 24 * 1024 * 1024;
  var oversizeFn = null;
  var OUTBOX_KEY = "wa_outbox";

  // Best-effort persistence so a refresh mid-outage doesn't lose the queue.
  // localStorage may be absent or full on KaiOS, and that must never break
  // sending, so every access is guarded.
  function saveOutbox() {
    try { localStorage.setItem(OUTBOX_KEY, JSON.stringify(outbox)); } catch (e) {}
  }
  function loadOutbox() {
    try {
      var raw = localStorage.getItem(OUTBOX_KEY);
      if (raw) outbox = JSON.parse(raw) || [];
    } catch (e) { outbox = []; }
    // A build before the size check could have stored a frame that jams the
    // connection. Drop those on load rather than replaying them forever.
    var kept = [];
    for (var i = 0; i < outbox.length; i++) {
      try {
        if (JSON.stringify(outbox[i]).length <= MAX_FRAME_BYTES) kept.push(outbox[i]);
        else console.warn("wire: dropping an oversized frame left by an older build");
      } catch (e) { /* unserialisable: drop it */ }
    }
    if (kept.length !== outbox.length) {
      outbox = kept;
      saveOutbox();
    }
  }
  loadOutbox();

  function flushOutbox() {
    if (!outbox.length || !ws || ws.readyState !== WebSocket.OPEN) return;
    var pending = outbox.slice();
    outbox = [];
    saveOutbox();
    console.log("wire: flushing " + pending.length + " queued frame(s)");
    for (var i = 0; i < pending.length; i++) {
      try { ws.send(JSON.stringify(pending[i])); }
      catch (e) { outbox.push(pending[i]); }
    }
    if (outbox.length) saveOutbox();
    if (queuedFn) queuedFn(outbox.length);
  }

  // send(type, dataObj) -> returns the request id (for correlating replies)
  function send(type, data) {
    var id = String(++reqId);
    var frame = { t: type, id: id };
    if (data !== null && data !== undefined) frame.data = data;
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(frame));
      return id;
    }
    if (QUEUEABLE[type]) {
      // A frame the far end will refuse must NEVER enter the outbox.
      //
      // The daemon closes the connection on an oversized frame rather than
      // rejecting it, so a queued one is replayed on reconnect, kills the
      // socket again, and is requeued — a single too-large video jams the
      // connection permanently, and the outbox persists to localStorage so it
      // survives a restart. Refusing it here costs one message; queueing it
      // costs every message after it.
      var wire = JSON.stringify(frame);
      if (wire.length > MAX_FRAME_BYTES) {
        console.warn("wire: too large to queue, dropped:", type, wire.length);
        if (oversizeFn) oversizeFn(type, wire.length);
        return id;
      }
      // Drop the oldest rather than grow without bound — a queue so long it
      // can't be delivered is worse than losing the stalest entry.
      outbox.push(frame);
      while (outbox.length > OUTBOX_MAX) outbox.shift();
      saveOutbox();
      console.warn("wire: queued while offline:", type);
      if (queuedFn) queuedFn(outbox.length);
    } else {
      console.warn("wire: send while not open, dropped:", type);
    }
    return id;
  }

  window.Wire = {
    T: T,
    connect: connect,
    // Exposed so the app can nudge a reconnect from anywhere it learns the
    // situation changed — a keypress after a long sleep, for instance.
    kick: kick,
    // The outbox is persistent, so a stuck message survives restarts. This is
    // the way out when one cannot be delivered at all.
    clearOutbox: function () {
      var n = outbox.length;
      outbox = [];
      saveOutbox();
      if (queuedFn) queuedFn(0);
      return n;
    },
    queuedCount: function () { return outbox.length; },
    onOversize: function (fn) { oversizeFn = fn; },
    // reconnect drops the current socket and dials again, for when the daemon's
    // ADDRESS changed rather than the connection failing — after setup, the old
    // socket points somewhere that may not exist any more, and waiting for its
    // backoff to expire would leave the app looking broken.
    reconnect: function () {
      if (ws) {
        // Silence the close handler first, or it schedules a retry against the
        // socket we're deliberately discarding.
        ws.onclose = null;
        ws.onerror = null;
        try { ws.close(); } catch (e) {}
        ws = null;
      }
      if (retryTimer) { clearTimeout(retryTimer); retryTimer = null; }
      backoff = C.RECONNECT_MIN;
      connect();
    },
    send: send,
    on: function (type, fn) {
      (listeners[type] = listeners[type] || []).push(fn);
      var still = [];
      for (var i = 0; i < pendingFrames.length; i++) {
        if (pendingFrames[i][0] === type) { try { fn(pendingFrames[i][1]); } catch(e){} }
        else still.push(pendingFrames[i]);
      }
      pendingFrames = still;
    },
    onStatus: function (fn) { statusFns.push(fn); },
    onQueued: function (fn) { queuedFn = fn; },
    isOpen: function () { return ws && ws.readyState === WebSocket.OPEN; },

    // For the settings screen, which is the only console this phone has.
    diag: function () {
      return {
        address: String(C.DAEMON_WS || ""),
        tokenLength: String(C.TOKEN || "").length,
        tokenTail: String(C.TOKEN || "").slice(-4),
        tokenHasSpace: /\s/.test(String(C.TOKEN || "")),
        // install.sh generates `openssl rand -hex 16`, which is lower case.
        // A T9 keypad capitalises readily — "Abc" mode capitalises the first
        // letter of a word, and some builds start in upper case entirely — and
        // the daemon compares the string exactly. An all-hex token carrying
        // capitals is therefore worth pointing at: it is the right length, it
        // looks correct at a glance, and it will be refused every time.
        tokenMiscased: /^[0-9a-fA-F]+$/.test(String(C.TOKEN || "")) &&
                       /[A-F]/.test(String(C.TOKEN || "")),
        attempts: diag.attempts,
        everOpened: diag.everOpened,
        lastOpenAt: diag.lastOpenAt,
        lastCloseAt: diag.lastCloseAt,
        lastCloseCode: diag.lastCloseCode,
        lastCloseClean: diag.lastCloseClean,
        lastError: diag.lastError
      };
    },
    httpURL: httpURL,
    classify: classify,
    probe: probe
  };
})();