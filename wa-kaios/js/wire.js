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
    // app -> daemon
    SEND: "send", GETCHATS: "getchats", GETHISTORY: "gethistory",
    MARKREAD: "markread",
    DELETE: "delete", FORWARD: "forward",
    CALLANSWER: "callanswer", CALLREJECT: "callreject",
    CALLDIAL: "calldial", CALLHANGUP: "callhangup"
  };

  var ws = null;
  var backoff = C.RECONNECT_MIN;
  var listeners = {};   // type -> [fn]
  var pendingFrames = [];  // frames received before a handler existed
  var statusFn = null;  // called with "connecting"|"open"|"closed"
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
    if (statusFn) statusFn(s);
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

  // send(type, dataObj) -> returns the request id (for correlating replies)
  function send(type, data) {
    var id = String(++reqId);
    var frame = { t: type, id: id };
    if (data !== null && data !== undefined) frame.data = data;
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(frame));
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
    onStatus: function (fn) { statusFn = fn; },
    isOpen: function () { return ws && ws.readyState === WebSocket.OPEN; }
  };
})();