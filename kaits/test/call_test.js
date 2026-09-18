// Tests for call.js's signalling logic.
//
// The media itself cannot be tested here — there is no engine to carry it and
// the far end is a browser from 2016. What CAN be tested is everything around
// it, and that is where the silent half-failures live: candidates added before
// the description they belong to, a microphone attached after the answer was
// built, an answer sent for somebody else's call.
//
// Run with:  node kaits/test/call_test.js

"use strict";

const fs = require("fs");
const path = require("path");
const vm = require("vm");

let failures = 0;
function check(what, got, want) {
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    failures++;
    console.log("FAIL  " + what + "\n      got  " + JSON.stringify(got) +
                "\n      want " + JSON.stringify(want));
  }
}
function ok(what, cond) {
  if (!cond) { failures++; console.log("FAIL  " + what); }
}

// A peer connection that records what was done to it, in the order it was
// done. Order is the point of several of these tests.
function makeFakeEngine(opts) {
  opts = opts || {};
  const log = [];
  const sent = [];

  function FakePC() {
    this.iceConnectionState = "new";
    this.connectionState = "new";
    this.localDescription = null;
    this.remoteDescription = null;
    this.addedTracks = [];
    this.addedCandidates = [];
    this.closed = false;
    log.push("construct");
  }
  FakePC.prototype.addTrack = function (t, s) {
    this.addedTracks.push(t);
    log.push("addTrack");
  };
  FakePC.prototype.setRemoteDescription = function (d, res) {
    this.remoteDescription = d;
    log.push("setRemoteDescription:" + d.type);
    setTimeout(res, 0);
  };
  FakePC.prototype.createAnswer = function (res) {
    log.push("createAnswer");
    setTimeout(function () { res({ type: "answer", sdp: "ANSWER_SDP" }); }, 0);
  };
  FakePC.prototype.setLocalDescription = function (d, res) {
    this.localDescription = d;
    log.push("setLocalDescription:" + d.type);
    setTimeout(res, 0);
  };
  FakePC.prototype.addIceCandidate = function (c, res) {
    if (!this.remoteDescription) {
      // This is exactly what a real engine does, and why the queue exists.
      throw new Error("remote description is not set");
    }
    this.addedCandidates.push(c);
    log.push("addIceCandidate");
    setTimeout(res, 0);
  };
  FakePC.prototype.close = function () { this.closed = true; log.push("close"); };

  const tracks = [{ kind: "audio", stop: function () { log.push("trackStop"); } }];
  const stream = {
    getAudioTracks: function () { return tracks; },
    getTracks: function () { return tracks; }
  };

  const audioEl = { play: function () { log.push("play"); }, pause: function () {},
                    srcObject: null, removeAttribute: function () {} };

  const win = {
    RTCPeerConnection: FakePC,
    RTCSessionDescription: function (d) { this.type = d.type; this.sdp = d.sdp; },
    RTCIceCandidate: function (c) { this.candidate = c.candidate; this.sdpMid = c.sdpMid; },
    MediaStream: function () {},
    Promise: Promise,
    setTimeout: setTimeout,
    clearTimeout: clearTimeout,
    URL: { createObjectURL: function () { return "blob:x"; } },
    console: { log: function () {} },
    navigator: {
      mediaDevices: {
        getUserMedia: function (c) {
          log.push("getUserMedia");
          if (opts.micFails) return Promise.reject(new Error("NotAllowedError"));
          if (opts.richRefused && c.audio && typeof c.audio === "object") {
            log.push("richRefused");
            return Promise.reject(new Error("OverconstrainedError"));
          }
          return Promise.resolve(stream);
        }
      }
    },
    document: {
      getElementById: function (id) { return id === "call-audio" ? audioEl : null; }
    },
    Wire: {
      T: { CALLSIGNAL: "callsignal" },
      send: function (t, d) { sent.push(d); log.push("send:" + d.kind); }
    }
  };
  win.window = win;

  const ctx = vm.createContext(win);
  vm.runInContext(fs.readFileSync(path.join(__dirname, "..", "js", "call.js"), "utf8"),
    ctx, { filename: "call.js" });

  return { win: win, log: log, sent: sent, stream: stream, audioEl: audioEl };
}

function settle() {
  // Three turns is enough for the promise chain in handleOffer.
  return new Promise(function (r) { setTimeout(r, 20); });
}

// ---------------------------------------------------------------------------

(async function run() {

  // The microphone must be attached BEFORE the answer is created. An answer
  // built without it describes a receive-only session: the call sounds
  // perfectly fine on this phone and is silent to the other person, which is
  // the single most misleading way this can break.
  {
    const e = makeFakeEngine();
    e.win.Call.begin("c1");
    e.win.Call.signal({ callid: "c1", kind: "offer", sdp: "OFFER_SDP" });
    await settle();

    const mic = e.log.indexOf("addTrack");
    const answer = e.log.indexOf("createAnswer");
    ok("the microphone is attached at all", mic >= 0);
    ok("an answer is created", answer >= 0);
    ok("the microphone is attached BEFORE the answer is created (got: " +
       e.log.join(",") + ")", mic >= 0 && answer >= 0 && mic < answer);
  }

  // The answer goes back with the sdp, tagged with the right call.
  {
    const e = makeFakeEngine();
    e.win.Call.begin("c1");
    e.win.Call.signal({ callid: "c1", kind: "offer", sdp: "OFFER_SDP" });
    await settle();
    check("exactly one signal was sent back", e.sent.length, 1);
    check("and it is an answer", e.sent[0].kind, "answer");
    check("carrying the sdp", e.sent[0].sdp, "ANSWER_SDP");
    check("for the right call", e.sent[0].callid, "c1");
  }

  // Candidates that arrive before the offer must be queued, not dropped and
  // not passed to an engine that will throw. Losing them costs the fastest
  // route and sometimes the whole connection.
  {
    const e = makeFakeEngine();
    e.win.Call.begin("c1");
    e.win.Call.signal({ callid: "c1", kind: "ice", candidate: { candidate: "early1" } });
    e.win.Call.signal({ callid: "c1", kind: "ice", candidate: { candidate: "early2" } });
    await settle();
    e.win.Call.signal({ callid: "c1", kind: "offer", sdp: "OFFER_SDP" });
    await settle();
    e.win.Call.signal({ callid: "c1", kind: "ice", candidate: { candidate: "late1" } });
    await settle();

    // Find the connection the module built.
    const added = e.log.filter(function (l) { return l === "addIceCandidate"; }).length;
    check("all three candidates reached the engine", added, 3);
    const firstAdd = e.log.indexOf("addIceCandidate");
    const setRemote = e.log.indexOf("setRemoteDescription:offer");
    ok("and none was added before the remote description", setRemote < firstAdd);
  }

  // Signalling for a different call must be ignored. With one call at a time
  // this should not happen, but a late frame from a call that just ended
  // would otherwise tear into the live one.
  {
    const e = makeFakeEngine();
    e.win.Call.begin("c1");
    e.win.Call.signal({ callid: "OTHER", kind: "offer", sdp: "OFFER_SDP" });
    await settle();
    check("an offer for another call is ignored", e.sent.length, 0);
  }

  // The daemon offers; an answer coming back the other way means something is
  // confused, and acting on it would half-establish a session.
  {
    const e = makeFakeEngine();
    e.win.Call.begin("c1");
    e.win.Call.signal({ callid: "c1", kind: "answer", sdp: "NOPE" });
    await settle();
    check("an answer from the daemon is ignored", e.sent.length, 0);
  }

  // A refused constraint object falls back to plain audio rather than failing
  // the call. An echo is worse than nothing; no microphone is worse than both.
  {
    const e = makeFakeEngine({ richRefused: true });
    e.win.Call.begin("c1");
    e.win.Call.signal({ callid: "c1", kind: "offer", sdp: "OFFER_SDP" });
    await settle();
    ok("the rich constraints were tried first", e.log.indexOf("richRefused") >= 0);
    check("and the call still completed", e.sent.length, 1);
    check("with an answer", e.sent[0].kind, "answer");
  }

  // A denied microphone must report failure, not sit silently in a call.
  {
    const e = makeFakeEngine({ micFails: true });
    let failed = null;
    e.win.Call.onState(function (s, d) { if (s === "failed") failed = d; });
    e.win.Call.begin("c1");
    e.win.Call.signal({ callid: "c1", kind: "offer", sdp: "OFFER_SDP" });
    await settle();
    ok("a denied microphone is reported as a failure", failed !== null);
    check("and no answer was sent", e.sent.length, 0);
  }

  // Ending a call has to stop the microphone. A track left live holds the
  // recording indicator on and, on this hardware, the battery with it.
  {
    const e = makeFakeEngine();
    e.win.Call.begin("c1");
    e.win.Call.signal({ callid: "c1", kind: "offer", sdp: "OFFER_SDP" });
    await settle();
    e.win.Call.end();
    ok("the microphone track was stopped", e.log.indexOf("trackStop") >= 0);
    ok("and the connection was closed", e.log.indexOf("close") >= 0);
    // Twice is safe: a call can end from both ends at once.
    e.win.Call.end();
  }

  // supported() has to be honest, so the UI can decline rather than accept a
  // call it cannot carry.
  {
    const e = makeFakeEngine();
    ok("a capable engine reports supported", e.win.Call.supported());
    delete e.win.RTCPeerConnection;
    delete e.win.mozRTCPeerConnection;
    ok("an engine with no RTCPeerConnection reports unsupported",
       !e.win.Call.supported());
  }

  if (failures) {
    console.log("\n" + failures + " failure(s)");
    process.exit(1);
  }
  console.log("call: all checks passed");
})();
