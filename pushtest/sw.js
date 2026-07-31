// sw.js — the whole point of the test.
//
// If this file's push handler runs while the phone is closed and asleep, then
// the architecture the real app is built on works, and the project is viable on
// this hardware. If it never runs, no amount of work on the app fixes that.
//
// Deliberately minimal: no fetch, no config, no daemon. Just proof of life.

self.addEventListener("install", function () { self.skipWaiting(); });
self.addEventListener("activate", function (ev) { ev.waitUntil(self.clients.claim()); });

self.addEventListener("push", function (ev) {
  var at = new Date().toLocaleTimeString();
  ev.waitUntil(
    self.registration.showNotification("Push arrived", {
      body: at + " — the worker woke up",
      tag: "pushtest",
      renotify: true
    }).then(tellPage)
  );
});

// Also poke the page, so a push landing while the app is open is visible in the
// log rather than only as a notification.
function tellPage() {
  return self.clients.matchAll({ type: "window", includeUncontrolled: true })
    .then(function (list) {
      for (var i = 0; i < list.length; i++) {
        if (list[i].postMessage) list[i].postMessage({ type: "push" });
      }
    });
}

self.addEventListener("notificationclick", function (ev) {
  ev.notification.close();
  ev.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(function (list) {
      if (list.length && "focus" in list[0]) return list[0].focus();
      // clients.openApp is the KaiOS extension; openWindow is the standard.
      if (self.clients.openApp) return self.clients.openApp("/pushtest/index.html");
      if (self.clients.openWindow) return self.clients.openWindow("/pushtest/index.html");
    })
  );
});
