// keepalive.js — stop the system reaping the app the moment it's out of sight.
//
// KaiOS inherits Firefox OS's process priority manager. A backgrounded app sits
// at the bottom of the pile and is the first thing killed when another app
// wants memory — which is exactly the reported symptom: it dies soon after you
// leave it, and sooner if you then go and do something.
//
// An app that is playing audio is not treated the same way: the platform counts
// it as perceptibly doing something for the user and moves it up the priority
// list. That's how a music player survives being backgrounded. So the app plays
// a near-silent one-second loop while it is out of sight, and stops the moment
// it comes back.
//
// This is a real trade, not a free win:
//   - it costs battery, because the audio pipeline stays open;
//   - it is not a guarantee, only a demotion of how attractive a target we are;
//   - it may interact with other audio (see the channel choice below).
// Which is why it is a setting, and why life.js measures whether it helped
// instead of us assuming it did.

window.Keepalive = (function () {
  "use strict";

  var el = null;
  var primed = false;
  var running = false;
  var wanted = true;
  var lastError = "";

  function supported() {
    return typeof Audio !== "undefined" || !!document.createElement("audio").canPlayType;
  }

  function build() {
    if (el) return el;
    el = document.createElement("audio");
    el.id = "keepalive-audio";
    el.src = "audio/keepalive.wav";
    el.loop = true;
    // Volume low as well as the file being near-silent: two independent reasons
    // for this to be inaudible, because one of them being wrong on some handset
    // should not mean a phone quietly humming in someone's pocket.
    el.volume = 0.01;
    el.preload = "auto";
    // Channel choice matters. "content" is the media channel, and starting
    // content playback interrupts whatever else is using it — this app would
    // pause the user's music every time it went to the background, which is a
    // far worse bug than the one being fixed. "normal" is the default,
    // non-exclusive channel and needs no permission at any app type.
    try { el.mozAudioChannelType = "normal"; } catch (e) { /* not KaiOS */ }
    el.setAttribute("aria-hidden", "true");
    document.body.appendChild(el);
    return el;
  }

  // Audio can't start without a user gesture, and "the app just went to the
  // background" is not one. So the element is unlocked during a keypress —
  // played and immediately paused — which leaves it primed to start later
  // without a gesture of its own.
  function prime() {
    if (primed || running || !wanted) return;
    var a = build();
    try {
      // play() returns undefined on Gecko 48 rather than a promise, so this
      // must not be chained. That exact assumption has already broken this app
      // once, in the notification beep.
      a.play();
      // Only count it as primed if playback ACTUALLY started. Blocked autoplay
      // does not throw — it just leaves the element paused — so latching
      // primed on the attempt would mean one silent failure at boot disables
      // the keepalive for the whole session.
      primed = !a.paused;
      setTimeout(function () { if (!running) { try { a.pause(); } catch (e) {} } }, 60);
    } catch (e) {
      lastError = String(e && e.message || e);
    }
  }

  function start() {
    if (!wanted || running) return;
    var a = build();
    try {
      a.currentTime = 0;
      a.play();
      running = true;
    } catch (e) {
      lastError = String(e && e.message || e);
    }
  }

  function stop() {
    if (!el || !running) return;
    try { el.pause(); } catch (e) {}
    running = false;
  }

  function setEnabled(on) {
    wanted = !!on;
    if (!wanted) stop();
  }

  function state() {
    return {
      wanted: wanted,
      primed: primed,
      running: running,
      supported: supported(),
      error: lastError
    };
  }

  return {
    prime: prime,
    start: start,
    stop: stop,
    setEnabled: setEnabled,
    state: state
  };
})();
