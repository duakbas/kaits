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
    // app -> daemon
    SEND: "send", GETCHATS: "getchats", GETHISTORY: "gethistory",
    MARKREAD: "markread",
    DELETE: "delete", FORWARD: "forward",
    CHATACTION: "chataction", GETPROFILE: "getprofile", SAVECONTACT: "savecontact",
    SENDREACTION: "sendreaction", SEARCH: "search", WATCH: "watch",
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
    try {
      ws = new WebSocket(url());
    } catch (e) {
      console.error("wire: construct failed", e);
      scheduleReconnect();
      return;
    }

    ws.onopen = function () {
       backoff = C.RECONNECT_MIN;
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

    ws.onclose = function () {
      setStatus("closed");
      scheduleReconnect();
    };

    ws.onerror = function (e) {
      // onclose will follow; just log.
      console.error("wire: socket error", e);
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
    queuedCount: function () { return outbox.length; },
    isOpen: function () { return ws && ws.readyState === WebSocket.OPEN; }
  };
})();