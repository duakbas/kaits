// push.js — registers the service worker and hands the daemon a push endpoint.
//
// The daemon can't reach a sleeping phone directly; it POSTs to this endpoint
// and the OS wakes sw.js. Everything here is feature-detected and best-effort:
// a desktop browser without KaiOS's push service, or a user who declines the
// permission, must still get a fully working app.

(function () {
  var W = window.Wire;
  var C = window.CONFIG || {};

  function log(msg) { console.log("push: " + msg); }

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
    return reg.pushManager.getSubscription().then(function (existing) {
      if (existing) return existing;
      return reg.pushManager.subscribe({ userVisibleOnly: true });
    });
  }

  function register() {
    if (!supported()) {
      log("not supported here — the app still works, it just won't wake itself");
      return;
    }
    navigator.serviceWorker.register("/sw.js")
      .then(function (reg) {
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
            W.send(W.T.PUSHSUB, { endpoint: endpoint });
          });
        });
      })
      .catch(function (e) { log("registration failed: " + e); });
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

  // Register only once the socket is up, so the endpoint has somewhere to go.
  // Re-registering on later reconnects is harmless — the daemon upserts, and
  // an unchanged endpoint is a no-op.
  var done = false;
  W.onStatus(function (s) {
    if (s !== "open" || done) return;
    done = true;
    // Slightly after connect, so the app's own startup traffic goes first.
    setTimeout(register, 1200);
  });
})();
