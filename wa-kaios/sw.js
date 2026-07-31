// sw.js — the service worker. Its only job is to be awake when the app isn't.
//
// On KaiOS there is exactly one always-on connection on the whole device: the
// OS's push channel, shared by every app. An app cannot hold its own socket
// while closed. So the daemon stays connected to WhatsApp and POSTs to this
// device's push endpoint when something arrives; the OS wakes this worker.
//
// The push carries NO payload. KaiOS lets you subscribe without an
// applicationServerKey, and without VAPID only empty pushes are allowed — which
// suits us, because it means message content never passes through Mozilla's or
// KaiOS's push infrastructure. The worker fetches a summary from the daemon
// instead, over the same authenticated connection the app uses.
//
// Budget: PushEvent has a ~30s idle timeout (dom.serviceWorkers.idle_timeout),
// so everything here is one fetch and one notification. Nothing long-running.

/* global clients */

var CONFIG = null;

// The worker can't read window.CONFIG, so the page hands its settings over at
// registration time and they're kept for the next wake-up.
self.addEventListener("message", function (ev) {
  if (ev.data && ev.data.type === "config") {
    CONFIG = ev.data.config;
  }
});

self.addEventListener("install", function () {
  // Take over immediately rather than waiting for the old worker to be released
  // — on a phone the app may not be reopened for a long time.
  self.skipWaiting();
});

self.addEventListener("activate", function (ev) {
  ev.waitUntil(self.clients.claim());
});

self.addEventListener("push", function (ev) {
  ev.waitUntil(showSummaryNotification());
});

// Ask the daemon what to say. If it can't be reached — the phone woke but has
// no data, or the daemon is down — still show something, because the push
// already told us a message exists and silence would be worse than vague.
function showSummaryNotification() {
  var fallback = { title: "WhatsApp", body: "New message", count: 1 };

  if (!CONFIG || !CONFIG.DAEMON_WS) {
    return notify(fallback);
  }
  var base = CONFIG.DAEMON_WS.replace(/^ws/, "http").replace(/\/ws.*$/, "");
  var url = base + "/notify-summary?token=" + encodeURIComponent(CONFIG.TOKEN || "");

  return fetch(url, { cache: "no-store" })
    .then(function (r) { return r.ok ? r.json() : fallback; })
    .then(function (s) {
      // count 0 means everything has since been read, most likely on another
      // device. Don't buzz the phone for something already dealt with.
      if (s && s.count === 0) return;
      return notify(s && s.title ? s : fallback);
    })
    .catch(function () { return notify(fallback); });
}

function notify(s) {
  return self.registration.showNotification(s.title || "WhatsApp", {
    body: s.body || "New message",
    icon: "/icons/icon-112.png",
    // One tag so a burst collapses into a single notification rather than
    // stacking up a screenful on a feature phone.
    tag: "wa-messages",
    renotify: true
  });
}

self.addEventListener("notificationclick", function (ev) {
  ev.notification.close();
  ev.waitUntil(openApp());
});

// Focus the app if it's already running, otherwise launch it. clients.openApp
// is a KaiOS 2.5 extension; openWindow is the standard path, and either may be
// missing, so all three are tried.
function openApp() {
  return self.clients.matchAll({ type: "window", includeUncontrolled: true })
    .then(function (list) {
      for (var i = 0; i < list.length; i++) {
        if ("focus" in list[i]) return list[i].focus();
      }
      if (self.clients.openApp) return self.clients.openApp("/index.html");
      if (self.clients.openWindow) return self.clients.openWindow("/index.html");
      return undefined;
    });
}
