// push.js — registers the service worker and hands the daemon a push endpoint.
//
// The daemon can't reach a sleeping phone directly; it POSTs to this endpoint
// and the OS wakes sw.js. Everything here is feature-detected and best-effort:
// a desktop browser without KaiOS's push service, or a user who declines the
// permission, must still get a fully working app.

(function () {
  var W = window.Wire;
  var C = window.CONFIG || {};

  // state is a one-line summary of how far push setup got, readable on the
  // phone via App.pushState(). Worth having because every failure here is
  // silent by design — the app works fine without push — and "no notifications"
  // otherwise gives you nothing to go on.
  var state = "not started";

  function log(msg) { console.log("push: " + msg); state = msg; }

  // withTimeout guards against a promise that never settles. A rejection is
  // easy to see; a hang looks exactly like success and leaves no trace. Reports
  // of KaiOS's push service misbehaving make that the failure to plan for —
  // apps hanging at their splash screen is what a stuck subscribe looks like
  // from the outside.
  function withTimeout(p, ms, label) {
    return new Promise(function (resolve, reject) {
      var settled = false;
      var timer = setTimeout(function () {
        if (settled) return;
        settled = true;
        reject(new Error(label + " did not respond within " + (ms / 1000) + "s"));
      }, ms);
      p.then(function (v) {
        if (settled) return;
        settled = true; clearTimeout(timer); resolve(v);
      }, function (e) {
        if (settled) return;
        settled = true; clearTimeout(timer); reject(e);
      });
    });
  }

  function supported() {
    return ("serviceWorker" in navigator) && ("PushManager" in window);
  }

  // Ask for notification permission. KaiOS grants it from the manifest for a
  // privileged app, so this usually resolves instantly; on desktop it prompts.
  function ensurePermission() {
    if (!("Notification" in window)) return Promise.resolve(false);
    if (Notification.permission === "granted") return Promise.resolve(true);
    if (Notification.permission === "denied") return Promise.resolve(false);
    return Notification.requestPermission().then(function (p) {
      return p === "granted";
    });
  }

  function subscribe(reg) {
    // Deliberately no applicationServerKey. KaiOS and Firefox allow subscribing
    // without one, and without VAPID only payload-free pushes are permitted —
    // which is exactly what we want. The push is a doorbell; the app pulls the
    // actual messages from the daemon once it's awake, so nothing sensitive
    // travels through the push service and there are no keys to manage.
    return withTimeout(reg.pushManager.getSubscription(), 10000, "getSubscription")
      .then(function (existing) {
        if (existing) return existing;
        return withTimeout(
          reg.pushManager.subscribe({ userVisibleOnly: true }), 30000, "subscribe");
      });
  }

  // The registration, once it exists. Notifications are raised through it, so
  // it's exposed rather than kept inside the promise chain.
  var registration = null;

  function register() {
    if (!supported()) {
      log("not supported here — the app still works, it just won't wake itself");
      return;
    }
    log("registering service worker…");
    withTimeout(navigator.serviceWorker.register("sw.js"), 20000, "worker registration")
      .then(function (reg) {
        registration = reg;
        log("service worker registered");
        // The worker can't read window.CONFIG, so give it what it needs to
        // reach the daemon for notification text.
        sendConfig(reg);
        return ensurePermission().then(function (ok) {
          if (!ok) { log("notification permission not granted"); return; }
          return subscribe(reg).then(function (sub) {
            if (!sub) { log("no subscription returned"); return; }
            var endpoint = (sub.endpoint || (sub.toJSON && sub.toJSON().endpoint));
            if (!endpoint) { log("subscription has no endpoint"); return; }
            log("subscribed; handing the endpoint to the daemon");
            // The socket may not be up yet — wire queues this and flushes it on
            // connect, so it doesn't need to be waited for here.
            W.send(W.T.PUSHSUB, { endpoint: endpoint });
          });
        });
      })
      .catch(function (e) {
        log("FAILED: " + (e && e.message ? e.message : e));
        // Let a later reconnect try again. The first attempt failing doesn't
        // mean the next one will — especially if the push service itself is
        // having a bad day.
        done = false;
      });
  }

  function sendConfig(reg) {
    var target = reg.active || reg.waiting || reg.installing;
    if (target && target.postMessage) {
      target.postMessage({
        type: "config",
        config: { DAEMON_WS: C.DAEMON_WS, TOKEN: C.TOKEN }
      });
    }
    // The worker that handles a future push may not be the one that exists now,
    // so resend once it takes over.
    if (navigator.serviceWorker) {
      navigator.serviceWorker.addEventListener("controllerchange", function () {
        if (navigator.serviceWorker.controller) {
          navigator.serviceWorker.controller.postMessage({
            type: "config",
            config: { DAEMON_WS: C.DAEMON_WS, TOKEN: C.TOKEN }
          });
        }
      });
    }
  }

  // Tapping a notification should land in that conversation, not the chat
  // list. Two routes: a message if the app was already running, or a URL
  // fragment if the worker had to launch it cold.
  if (navigator.serviceWorker) {
    navigator.serviceWorker.addEventListener("message", function (ev) {
      if (ev.data && ev.data.type === "openchat" && ev.data.jid) {
        if (window.App && window.App.openChat) window.App.openChat(ev.data.jid);
      }
    });
  }

  function pendingChatFromHash() {
    var m = /[#&]chat=([^&]+)/.exec(window.location.hash || "");
    return m ? decodeURIComponent(m[1]) : "";
  }

  // On a cold launch the chat list has to arrive before we can open anything,
  // so this waits for the first chatlist rather than firing immediately.
  var wanted = pendingChatFromHash();
  if (wanted) {
    W.on(W.T.CHATLIST, function () {
      if (!wanted) return;
      var jid = wanted;
      wanted = "";
      // Clear the fragment so a later refresh doesn't reopen it.
      try { window.location.hash = ""; } catch (e) {}
      if (window.App && window.App.openChat) window.App.openChat(jid);
    });
  }

  // Exposed for testing from the console: App.testNotify(). Shows the same
  // notifications a real push would, using live unread state — so grouping,
  // tag replacement and click-to-open can be checked without waiting for
  // someone to message you, and without a working push subscription.
  window.App = window.App || {};

  // App.pushState() — how far push setup got, and whether the pieces exist at
  // all. The first thing to check on the phone when notifications don't arrive.
  window.App.pushState = function () {
    return {
      state: state,
      hasServiceWorker: ("serviceWorker" in navigator),
      hasPushManager: ("PushManager" in window),
      permission: (window.Notification && Notification.permission) || "no Notification API"
    };
  };

  window.App.testNotify = function () {
    if (!navigator.serviceWorker) { console.log("push: no service worker here"); return; }
    navigator.serviceWorker.ready.then(function (reg) {
      var target = reg.active || (navigator.serviceWorker.controller);
      if (!target) { console.log("push: no active worker yet"); return; }
      target.postMessage({
        type: "config",
        config: { DAEMON_WS: C.DAEMON_WS, TOKEN: C.TOKEN }
      });
      target.postMessage({ type: "testnotify" });
      console.log("push: asked the worker to show notifications");
    });
  };

  // Register only once the socket is up, so the endpoint has somewhere to go.
  // Re-registering on later reconnects is harmless — the daemon upserts, and
  // an unchanged endpoint is a no-op.
  // Register immediately. The socket is needed to HAND OVER the endpoint, not
  // to register the worker or to show a notification — and gating registration
  // on the socket meant that with no daemon reachable there was no worker, so
  // navigator.serviceWorker.ready never resolved and every notification
  // silently did nothing.
  register();

  window.App = window.App || {};
  window.App.registration = function () { return registration; };
})();
