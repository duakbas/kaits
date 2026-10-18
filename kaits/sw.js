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
// registration time. A global alone doesn't survive: the OS kills an idle
// worker within seconds, and the push that wakes it hours later starts with
// CONFIG === null — every notification then fell back to "New message". So the
// settings are also written to the Cache API and read back on wake-up.
var CONFIG_CACHE = "kaits-config";
var CONFIG_KEY = "/__kaits_config";

function saveConfig(cfg) {
  CONFIG = cfg;
  if (!self.caches) return Promise.resolve();
  return caches.open(CONFIG_CACHE).then(function (c) {
    return c.put(CONFIG_KEY, new Response(JSON.stringify(cfg),
      { headers: { "Content-Type": "application/json" } }));
  }).catch(function () {});
}

function loadConfig() {
  if (CONFIG && CONFIG.DAEMON_WS) return Promise.resolve(CONFIG);
  if (!self.caches) return Promise.resolve(null);
  return caches.open(CONFIG_CACHE)
    .then(function (c) { return c.match(CONFIG_KEY); })
    .then(function (r) { return r ? r.json() : null; })
    .then(function (cfg) { if (cfg) CONFIG = cfg; return cfg; })
    .catch(function () { return null; });
}

self.addEventListener("message", function (ev) {
  if (!ev.data) return;
  if (ev.data.type === "config") {
    var p = saveConfig(ev.data.config);
    if (ev.waitUntil) ev.waitUntil(p);
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
  return loadConfig().then(summaryWith);
}

function summaryWith(cfg) {
  var fallback = { title: "Kaits", body: "New message" };

  if (!cfg || !cfg.DAEMON_WS) {
    return notify(fallback, "wa-generic");
  }
  var base = cfg.DAEMON_WS.replace(/^ws/, "http").replace(/\/ws.*$/, "");
  var url = base + "/notify-summary?token=" + encodeURIComponent(cfg.TOKEN || "");

  return fetch(url, { cache: "no-store" })
    .then(function (r) { return r.ok ? r.json() : null; })
    .then(function (s) {
      if (!s || !s.chats) return notify(fallback, "wa-generic");
      var keep = s.chats.map(function (ch) { return "wa-" + ch.jid; });
      // A ringing call outranks everything: it's why the phone was woken.
      // Tapping it opens the app, which gets the call offer again on connect.
      if (s.call) {
        keep.push("wa-call");
        // KaiOS lets a service worker launch its app; for a call, put Kaits
        // in front right away rather than waiting for a tap. The notification
        // stays as the fallback if this isn't allowed here.
        try { if (self.clients && self.clients.openApp) self.clients.openApp({ msg: "call" }); } catch (e) {}
        return Promise.all([
          notify({ title: "Incoming call", body: s.call.name || "WhatsApp call" }, "wa-call", { call: s.call.id }),
          clearStale(keep)
        ]);
      }
      // Nothing unread means it was read elsewhere between the push being sent
      // and the phone waking. Clear any stale bubbles instead of buzzing.
      if (!s.chats.length) return saveNotified({}).then(function () { return clearStale([]); });

      // Only chats with something NEW since the last time we showed them get a
      // notification. Re-showing every unread chat on every push made each
      // message in one chat buzz once per unread chat — an avalanche once a
      // few groups had unread messages.
      return loadNotified().then(function (seen) {
        var next = {};
        var jobs = [];
        // First wake-up after an update: nothing remembered yet. Treat every
        // unread chat as already seen except the newest one, or the first push
        // would show the whole unread backlog at once.
        if (!seen || !Object.keys(seen).length) {
          seen = {};
          var newest = 0;
          s.chats.forEach(function (ch) { newest = Math.max(newest, ch.ts || 0); });
          s.chats.forEach(function (ch) { if ((ch.ts || 0) < newest) seen[ch.jid] = ch.ts || 0; });
        }
        s.chats.forEach(function (ch) {
          var last = seen[ch.jid] || 0;
          next[ch.jid] = Math.max(last, ch.ts || 0);
          if ((ch.ts || 0) > last) {
            jobs.push(notify({ title: ch.title, body: ch.body }, "wa-" + ch.jid, { jid: ch.jid }));
          }
        });
        // Drop bubbles for conversations that are no longer unread — read on
        // another device, most likely. Forgetting them means their next
        // message notifies again.
        jobs.push(clearStale(s.chats.map(function (ch) { return "wa-" + ch.jid; })));
        jobs.push(saveNotified(next));
        return Promise.all(jobs);
      });
    })
    .catch(function () { return notify(fallback, "wa-generic"); });
}

// What each chat's notification last showed (chat jid -> message ts), kept in
// the Cache API for the same reason as the config: the worker doesn't live
// between pushes.
var NOTIFIED_KEY = "/__kaits_notified";

function loadNotified() {
  if (!self.caches) return Promise.resolve({});
  return caches.open(CONFIG_CACHE)
    .then(function (c) { return c.match(NOTIFIED_KEY); })
    .then(function (r) { return r ? r.json() : {}; })
    .catch(function () { return {}; });
}

function saveNotified(map) {
  if (!self.caches) return Promise.resolve();
  return caches.open(CONFIG_CACHE).then(function (c) {
    return c.put(NOTIFIED_KEY, new Response(JSON.stringify(map),
      { headers: { "Content-Type": "application/json" } }));
  }).catch(function () {});
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
