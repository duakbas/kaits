// life.js — a flight recorder for the app's own lifecycle.
//
// When KaiOS kills a backgrounded app it does not ask, does not warn, and does
// not run any of your code. The process is simply gone. So "the app closes soon
// after I exit it" cannot be investigated from inside a running app: by the time
// you could ask a question, the thing that would have answered it is dead.
//
// What survives a kill is localStorage. So the app writes a heartbeat there
// while it is backgrounded, and on the next launch reads back the previous
// session's last heartbeat. No goodbye plus a heartbeat that stops is a kill,
// and the gap between backgrounding and that last beat is how long it survived.
// A clean exit writes a goodbye, so the two are distinguishable — which matters,
// because "you closed it" and "the system killed it" have completely different
// fixes.
//
// This is the same shape as the push measurement in pushtest/: get numbers
// first, then decide, rather than tuning against a guess.

window.Life = (function () {
  "use strict";

  var CUR = "kaits.life.cur";     // the session running right now
  var HIST = "kaits.life.hist";   // outcomes of previous sessions
  var HIST_MAX = 12;

  // While backgrounded the beat has to be frequent enough that the last one is
  // a decent estimate of the time of death. 15s costs a small synchronous write
  // per beat and pins the answer to within 15 seconds, which is enough to tell
  // "instantly" from "a few minutes" — the distinction that matters here.
  var BEAT_HIDDEN_MS = 15000;
  var BEAT_VISIBLE_MS = 60000;

  var cur = null;
  var prev = null;
  var timer = null;
  var watcher = null;   // supplies the facts worth knowing at time of death

  function now() { return Date.now(); }

  function read(key) {
    try {
      var raw = localStorage.getItem(key);
      return raw ? JSON.parse(raw) : null;
    } catch (e) { return null; }
  }

  function write(key, val) {
    try { localStorage.setItem(key, JSON.stringify(val)); } catch (e) {}
  }

  // Turn a raw record into what actually happened. The three outcomes are the
  // whole point of the file, so they get named rather than inferred at the call
  // site every time.
  function classify(rec) {
    if (!rec || !rec.boot) return null;
    var out = {
      version: rec.ver || "?",
      boot: rec.boot,
      ranFor: Math.round(((rec.bye || rec.last || rec.boot) - rec.boot) / 1000),
      backgrounded: !!rec.hidden,
      bgFor: 0,
      outcome: "unknown"
    };
    if (rec.hidden) {
      out.bgFor = Math.round(((rec.bye || rec.last || rec.hidden) - rec.hidden) / 1000);
    }
    if (rec.bye) out.outcome = "closed";
    else if (rec.hidden) out.outcome = "killed in background";
    else out.outcome = "died in foreground";
    out.state = rec.st || null;
    return out;
  }

  function describe(o) {
    if (!o) return "no previous session recorded";
    if (o.outcome === "closed") {
      return "last session: closed cleanly after " + fmt(o.ranFor);
    }
    if (o.outcome === "killed in background") {
      return "last session: backgrounded, then killed after " + fmt(o.bgFor) +
             " in the background (ran " + fmt(o.ranFor) + " total)" +
             stateLine(o.state);
    }
    return "last session: died while on screen after " + fmt(o.ranFor);
  }

  // What the app was doing when it stopped. Deliberately terse — this goes on
  // a 240px screen — but every field here answers a question that would
  // otherwise need another day of waiting to ask again.
  function stateLine(st) {
    if (!st) return "";
    var bits = [];
    if (st.ka) bits.push("keepalive " + st.ka);
    if (st.sock) bits.push("socket " + st.sock);
    if (typeof st.msgs === "number") bits.push(st.msgs + " msgs while hidden");
    return bits.length ? "\n  at the time: " + bits.join(", ") : "";
  }

  function fmt(secs) {
    if (secs < 90) return secs + "s";
    if (secs < 5400) return Math.round(secs / 60) + "m";
    return (Math.round(secs / 360) / 10) + "h";
  }

  function beat() {
    if (!cur) return;
    cur.last = now();
    cur.beats = (cur.beats || 0) + 1;
    // Stamp the app's state into every heartbeat. "It died after 47 minutes" is
    // a fact; "it died after 47 minutes with the keepalive playing and the
    // socket open" is a diagnosis. A killed process writes nothing on the way
    // out, so whatever we want to know afterwards has to already be on disk.
    if (watcher) {
      try {
        var st = watcher();
        if (st && typeof st === "object") cur.st = st;
      } catch (e) { /* never let diagnostics break the thing they measure */ }
    }
    write(CUR, cur);
  }

  function reschedule(hidden) {
    if (timer) clearInterval(timer);
    timer = setInterval(beat, hidden ? BEAT_HIDDEN_MS : BEAT_VISIBLE_MS);
  }

  function isHidden() {
    if (typeof document.hidden === "boolean") return document.hidden;
    if (document.mozHidden !== undefined) return document.mozHidden;
    return false;
  }

  function onVisibility() {
    if (!cur) return;
    var hidden = isHidden();
    if (hidden) {
      // Only the FIRST hide is recorded. A session that goes background,
      // foreground, background is still one session, and what we want to know
      // is how long it lasted out of sight — measured from the most recent
      // time it left the screen.
      cur.hidden = now();
    } else {
      cur.hidden = 0;
      cur.returns = (cur.returns || 0) + 1;
    }
    beat();
    reschedule(hidden);
  }

  // A clean exit is the one case where we get to say goodbye. Gecko 48 has
  // pagehide; beforeunload is there as a second chance. Both DO fire, one after
  // the other, so this has to be idempotent — without the guard a single clean
  // exit was recorded twice, which quietly halves the apparent kill rate. That
  // is the one number this file exists to report.
  var saidBye = false;
  function goodbye() {
    if (!cur || saidBye) return;
    saidBye = true;
    cur.bye = now();
    write(CUR, cur);
    close(cur);
  }

  // Fold a finished session into the history ring, so a pattern is visible
  // rather than just the most recent data point.
  function close(rec) {
    var o = classify(rec);
    if (!o) return;
    var hist = read(HIST) || [];
    hist.push({ t: o.boot, ran: o.ranFor, bg: o.bgFor, out: o.outcome,
                ver: o.version, st: o.state });
    while (hist.length > HIST_MAX) hist.shift();
    write(HIST, hist);
  }

  function start(version) {
    // Whatever was in CUR belongs to the session before this one. If it has no
    // goodbye, that session did not get to finish — which is the measurement.
    var last = read(CUR);
    prev = classify(last);
    if (last && !last.bye) close(last);

    cur = { boot: now(), last: now(), hidden: 0, bye: 0, ver: version || "?" };
    saidBye = false;
    write(CUR, cur);

    document.addEventListener("visibilitychange", onVisibility, false);
    document.addEventListener("mozvisibilitychange", onVisibility, false);
    window.addEventListener("pagehide", goodbye, false);
    window.addEventListener("beforeunload", goodbye, false);
    reschedule(isHidden());
    if (isHidden()) cur.hidden = now();
  }

  // Every recorded session, newest first, as lines for the diagnostics screen.
  function report() {
    var hist = read(HIST) || [];
    var lines = [];
    for (var i = hist.length - 1; i >= 0; i--) {
      var h = hist[i];
      lines.push(
        (h.out === "closed" ? "closed  " :
         h.out === "killed in background" ? "KILLED  " : "died    ") +
        "ran " + fmt(h.ran) +
        (h.bg ? ", bg " + fmt(h.bg) : "") +
        "  v" + (h.ver || "?") +
        (h.st && h.st.ka ? "  ka:" + h.st.ka : "") +
        (h.st && h.st.sock ? "  " + h.st.sock : ""));
    }
    return lines;
  }

  // How many of the recent sessions ended in a background kill, which is the
  // number that says whether this is a pattern or a one-off.
  function killRate() {
    var hist = read(HIST) || [];
    if (!hist.length) return null;
    var killed = 0;
    for (var i = 0; i < hist.length; i++) {
      if (hist[i].out === "killed in background") killed++;
    }
    return { killed: killed, total: hist.length };
  }

  function clear() {
    try { localStorage.removeItem(HIST); } catch (e) {}
  }

  return {
    // watch(fn) registers a supplier of the facts to record on each heartbeat.
    // Kept as a callback rather than a setter so life.js needs to know nothing
    // about sockets, audio, or anything else it is recording.
    watch: function (fn) { watcher = fn; },
    start: start,
    previous: function () { return prev; },
    describe: function () { return describe(prev); },
    report: report,
    killRate: killRate,
    clear: clear,
    beat: beat,
    // exposed for testing
    _classify: classify,
    _fmt: fmt
  };
})();
