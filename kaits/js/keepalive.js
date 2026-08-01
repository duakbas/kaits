// keepalive.js — stop the system reaping the app the moment it's out of sight.
//
// KaiOS inherits Firefox OS's process priority manager. A backgrounded app sits
// at the bottom of the pile and is the first thing killed when another app
// wants memory — which is exactly the reported symptom: it dies soon after you
// leave it, and sooner if you then go and do something.
//
// An app that is playing audio is not treated the same way: the platform counts
// it as perceptibly doing something for the user and moves it up the priority
// list. That's how a music player survives being backgrounded. So the app makes
// an inaudible tone while it is out of sight, and stops the moment it returns.
//
// It SYNTHESISES that tone rather than looping a file, and the difference is
// the whole reason this file was rewritten. A one-second file loops 3600 times
// an hour, and every loop point was audible as a click — reported, accurately,
// as "clicking like the Predator". The click is not the tone; the tone is 80 dB
// below anything hearable. It is the element restarting: the decoder tears down
// and comes back, and an amplifier that gates on silence pops each time it
// wakes. A continuously running oscillator has no loop point to click at, by
// construction. The file remains as a fallback for an engine with no Web Audio,
// and is long rather than short so that even then the clicks are rare.
//
// This is a real trade, not a free win:
//   - it costs battery, because the audio pipeline stays open;
//   - it is not a guarantee, only a demotion of how attractive a target we are;
//   - it may interact with other audio (see the channel choice below).
// Which is why it is a setting, and why life.js measures whether it helped
// instead of us assuming it did.

window.Keepalive = (function () {
  "use strict";

  var el = null;                 // fallback path: a looping <audio> element
  var ctx = null, osc = null, gain = null;   // primary path: synthesis
  var engine = "";               // "oscillator" | "file" | ""
  var primed = false;
  var running = false;
  var wanted = true;
  var lastError = "";
  var interrupted = false;
  var interruptions = 0;
  var channelAsked = "";
  var channelGot = "";

  // Answered once and remembered. state() is called from the diagnostics screen
  // AND from the lifecycle heartbeat every 15 seconds, so building a throwaway
  // <audio> element to answer it each time is a slow leak in the one code path
  // that must not cost anything.
  var supportedCache = null;
  function supported() {
    if (supportedCache === null) {
      supportedCache = !!(window.AudioContext || window.webkitAudioContext) ||
        typeof Audio !== "undefined" ||
        !!document.createElement("audio").canPlayType;
    }
    return supportedCache;
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

  // The oscillator path. No file, no decoder, no loop — one continuously
  // running tone whose gain is turned up when the app goes out of sight and
  // down when it comes back. Nothing starts or stops, so nothing can click.
  function buildOsc() {
    if (ctx) return true;
    var AC = window.AudioContext || window.webkitAudioContext;
    if (!AC) return false;
    try {
      ctx = new AC();
      // Same channel reasoning as the file path: see the note in config.js.
      channelAsked = (window.CONFIG && CONFIG.KEEPALIVE_CHANNEL) || "normal";
      try {
        ctx.mozAudioChannelType = channelAsked;
        channelGot = ctx.mozAudioChannelType || "";
      } catch (e) { channelGot = "(refused)"; }

      osc = ctx.createOscillator();
      gain = ctx.createGain();
      gain.gain.value = 0;             // silent until we are actually hidden
      osc.frequency.value = 20;        // below hearing; see audio/mkkeepalive.py
      osc.type = "sine";
      osc.connect(gain);
      gain.connect(ctx.destination);
      osc.start(0);
      engine = "oscillator";
      return true;
    } catch (e) {
      lastError = "osc: " + String(e && e.message || e);
      ctx = null; osc = null; gain = null;
      return false;
    }
  }

  // The level both paths aim for: the file is 1% of full scale and is played at
  // KEEPALIVE_VOLUME, so the oscillator multiplies the two to land in the same
  // place rather than having a second number to keep in step.
  function targetGain() {
    var vol = (window.CONFIG && typeof CONFIG.KEEPALIVE_VOLUME === "number")
      ? CONFIG.KEEPALIVE_VOLUME : 0.01;
    if (!(vol > 0)) vol = 0.01;
    if (vol > 1) vol = 1;
    return vol * 0.01;
  }

  // Audio can't start without a user gesture, and "the app just went to the
  // background" is not one. So the pipeline is opened during a keypress, which
  // leaves it ready to be turned up later without a gesture of its own.
  function prime() {
    if (primed || !wanted) return;
    if (buildOsc()) {
      try {
        if (ctx.state === "suspended" && ctx.resume) ctx.resume();
        primed = ctx.state !== "suspended";
      } catch (e) {
        lastError = String(e && e.message || e);
      }
      return;
    }
    // No Web Audio: fall back to the file.
    engine = "file";
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
    if (engine === "oscillator" || buildOsc()) {
      try {
        if (ctx.state === "suspended" && ctx.resume) ctx.resume();
        // Ramp rather than jump: an instantaneous gain change is a step in the
        // waveform, which is a click of exactly the kind being fixed.
        rampTo(targetGain());
        running = true;
      } catch (e) {
        lastError = String(e && e.message || e);
      }
      return;
    }
    var a = build();
    try {
      a.currentTime = 0;
      a.play();
      running = true;
    } catch (e) {
      lastError = String(e && e.message || e);
    }
  }

  // A 50 ms ramp is inaudible in itself and removes the discontinuity.
  function rampTo(v) {
    if (!gain) return;
    var now = ctx.currentTime;
    try {
      gain.gain.cancelScheduledValues(now);
      gain.gain.setValueAtTime(gain.gain.value, now);
      gain.gain.linearRampToValueAtTime(v, now + 0.05);
    } catch (e) {
      gain.gain.value = v;      // an engine without the scheduling API
    }
  }

  function stop() {
    if (!running) return;
    running = false;
    if (engine === "oscillator") {
      try {
        rampTo(0);
        // Suspend after the ramp has finished, or the suspend itself truncates
        // it into the step change the ramp exists to avoid.
        setTimeout(function () {
          if (!running && ctx && ctx.state === "running" && ctx.suspend) {
            try { ctx.suspend(); } catch (e) {}
          }
        }, 120);
      } catch (e) {}
      return;
    }
    if (el) { try { el.pause(); } catch (e) {} }
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
      engine: engine || "not started",
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
