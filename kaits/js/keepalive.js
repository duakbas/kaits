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
  var interrupted = false;
  var interruptions = 0;
  var channelAsked = "";
  var channelGot = "";

  function supported() {
    return typeof Audio !== "undefined" || !!document.createElement("audio").canPlayType;
  }

  function build() {
    if (el) return el;
    el = document.createElement("audio");
    el.id = "keepalive-audio";
    el.src = "audio/keepalive.wav";
    el.loop = true;
    // Two independent reasons to be inaudible, so one of them being wrong on
    // some handset doesn't leave a phone humming in a pocket: a file at 1% of
    // full scale, and this on top of it. What it must NOT be is zero — a muted
    // or zero-volume element counts as inaudible, and inaudible is exactly
    // what the priority manager disregards. Clamped above zero for that
    // reason, because a well-meaning edit to config.js otherwise turns the
    // whole mechanism off while appearing to leave it on.
    var vol = (window.CONFIG && typeof CONFIG.KEEPALIVE_VOLUME === "number")
      ? CONFIG.KEEPALIVE_VOLUME : 0.01;
    if (!(vol > 0)) vol = 0.01;
    if (vol > 1) vol = 1;
    el.volume = vol;
    el.preload = "auto";
    // Channel choice is the whole question, and it is a trade in both
    // directions:
    //
    //   "normal"  — the default, non-exclusive channel. It should NOT stop
    //               anything else playing, which is why it is the default
    //               here. The risk is that the platform may not count it as
    //               "this app is doing something for the user", in which case
    //               the keepalive buys nothing.
    //   "content" — the media channel. Certain to be counted, and certain to
    //               interrupt whatever else holds it: your music would stop
    //               every time you left the app.
    //
    // Which of those is true on this handset is a measurement, not a fact I
    // have. So the channel is configurable, the value the engine actually
    // accepted is read back, and the Settings screen reports both alongside
    // the kill rate — change it, use the phone for a day, compare.
    channelAsked = (window.CONFIG && CONFIG.KEEPALIVE_CHANNEL) || "normal";
    try {
      el.mozAudioChannelType = channelAsked;
      channelGot = el.mozAudioChannelType || "";
    } catch (e) {
      channelGot = "(refused)";
    }

    // The platform interrupts a lower-priority channel when a higher one wants
    // the speaker — an incoming call, an alarm. Fighting that by restarting
    // would be exactly the antisocial behaviour this is trying to avoid, so
    // note it and wait to be told it is over.
    el.addEventListener("mozinterruptbegin", function () {
      interrupted = true;
      interruptions++;
    }, false);
    el.addEventListener("mozinterruptend", function () {
      interrupted = false;
      if (running && wanted) { try { el.play(); } catch (e) {} }
    }, false);

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
    if (!wanted || running || interrupted) return;
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
      channel: channelGot || channelAsked,
      channelAsked: channelAsked,
      interrupted: interrupted,
      interruptions: interruptions,
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
