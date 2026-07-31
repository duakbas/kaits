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
    SEARCHRESULT: "searchresult", EDITED: "edited",
    // app -> daemon
    SEND: "send", GETCHATS: "getchats", GETHISTORY: "gethistory",
    MARKREAD: "markread",
    DELETE: "delete", FORWARD: "forward",
    CHATACTION: "chataction", GETPROFILE: "getprofile", SAVECONTACT: "savecontact",
    SENDREACTION: "sendreaction", SEARCH: "search", WATCH: "watch",
    PUSHSUB: "pushsub", EDIT: "edit",
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

  function connect() {
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
    setTimeout(connect, backoff);
    backoff = Math.min(backoff * 2, C.RECONNECT_MAX);
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
    savecontact: true, sendreaction: true, markread: true, edit: true
  };
  var outbox = [];
  var OUTBOX_MAX = 50;
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