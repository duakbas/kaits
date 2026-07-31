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
  if (!ev.data) return;
  if (ev.data.type === "config") {
    CONFIG = ev.data.config;
  } else if (ev.data.type === "testnotify") {
    // Dev hook: run the real push path without a real push. Chrome refuses to
    // subscribe without an applicationServerKey, so this is the only way to
    // exercise notification rendering there — and it's still the fastest way to
    // check grouping anywhere, since it needs no incoming message.
    showSummaryNotification();
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

// Ask the daemon what to say, then show ONE notification per chat.
//
// Tagging each notification with its chat JID is what produces the behaviour
// you'd expect from a phone: a second message in the same conversation replaces
// that conversation's bubble instead of stacking a new one, while a different
// conversation gets its own. renotify makes the replacement buzz, so an
// updating bubble still announces itself rather than changing silently.
//
// If the daemon can't be reached — woken with no data, or it's down — still
// show something generic, because the push already told us a message exists and
// silence would be worse than vague.
function showSummaryNotification() {
  var fallback = { title: "Kaits", body: "New message" };

  if (!CONFIG || !CONFIG.DAEMON_WS) {
    return notify(fallback, "wa-generic");
  }
  var base = CONFIG.DAEMON_WS.replace(/^ws/, "http").replace(/\/ws.*$/, "");
  var url = base + "/notify-summary?token=" + encodeURIComponent(CONFIG.TOKEN || "");

  return fetch(url, { cache: "no-store" })
    .then(function (r) { return r.ok ? r.json() : null; })
    .then(function (s) {
      if (!s || !s.chats) return notify(fallback, "wa-generic");
      // Nothing unread means it was read elsewhere between the push being sent
      // and the phone waking. Clear any stale bubbles instead of buzzing.
      if (!s.chats.length) return clearStale([]);

      var jobs = s.chats.map(function (ch) {
        return notify(
          { title: ch.title, body: ch.body },
          "wa-" + ch.jid,
          { jid: ch.jid }
        );
      });
      // Drop bubbles for conversations that are no longer unread — read on
      // another device, most likely.
      jobs.push(clearStale(s.chats.map(function (ch) { return "wa-" + ch.jid; })));
      return Promise.all(jobs);
    })
    .catch(function () { return notify(fallback, "wa-generic"); });
}

function notify(s, tag, data) {
  // Same minimal option set as the app's osNotify, for the same reason: on
  // Gecko 48 the fuller version vibrated without rendering anything. tag and
  // data earn their place (per-chat replacement, and knowing which chat to
  // open on tap); icon and renotify don't.
  return self.registration.showNotification(s.title || "Kaits", {
    body: s.body || "New message",
    tag: tag || "wa-generic",
    data: data || {}
  });
}

// Close notifications whose chat no longer has anything unread.
function clearStale(keepTags) {
  if (!self.registration.getNotifications) return Promise.resolve();
  return self.registration.getNotifications().then(function (list) {
    for (var i = 0; i < list.length; i++) {
      if (keepTags.indexOf(list[i].tag) === -1) list[i].close();
    }
  });
}

self.addEventListener("notificationclick", function (ev) {
  ev.notification.close();
  var jid = (ev.notification.data && ev.notification.data.jid) || "";
  ev.waitUntil(openApp(jid));
});

// Focus the app if it's already running, otherwise launch it. clients.openApp
// is a KaiOS 2.5 extension; openWindow is the standard path, and either may be
// missing, so all three are tried.
function openApp(jid) {
  return self.clients.matchAll({ type: "window", includeUncontrolled: true })
    .then(function (list) {
      for (var i = 0; i < list.length; i++) {
        // Already running: tell it which conversation was tapped, so the user
        // lands in that chat rather than the list.
        if (jid && list[i].postMessage) {
          list[i].postMessage({ type: "openchat", jid: jid });
        }
        if ("focus" in list[i]) return list[i].focus();
      }
      // Not running. The JID rides in the URL so the app can read it at boot.
      var path = "/index.html" + (jid ? "#chat=" + encodeURIComponent(jid) : "");
      if (self.clients.openApp) return self.clients.openApp(path);
      if (self.clients.openWindow) return self.clients.openWindow(path);
      return undefined;
    });
}
