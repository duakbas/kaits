// call.js — the phone's half of a call's media.
//
// The daemon always offers. That is deliberate: it is the side that learns a
// call exists first in both directions, and being the offerer is what lets it
// pin the codec list to G.711 µ-law alone, which in turn keeps the daemon's
// entire codec burden to a 256-entry table instead of libopus and cgo. So this
// file never creates an offer. It answers one.
//
// WRITING WEBRTC FOR GECKO 48
//
// This engine sits on the far side of two API transitions, and the app has to
// work on both sides of each because it is also developed in a current
// browser:
//
//   - Promises vs callbacks. The callback forms are what a 2016 engine
//     implements; current engines deprecate them but still dispatch on a
//     callable first argument, exactly as the spec's legacy overloads say.
//     So passing callbacks AND watching for a thenable means precisely one of
//     the two fires, whichever engine this is.
//   - onaddstream vs ontrack, addStream vs addTrack, srcObject vs
//     createObjectURL. Same story: try the modern one, fall back.
//
// None of this is speculative defensiveness — every fallback here is for a
// specific API that Gecko 48 either lacks or spells differently.

(function () {
  var pc = null;
  var localStream = null;
  var callID = null;
  var audioEl = null;

  // ICE can arrive before the description it belongs to, and adding it then is
  // an error. Queue until the remote description is in.
  var pending = [];
  var haveRemote = false;

  var listeners = { state: [] };

  function log() {
    if (window.console && console.log) {
      console.log.apply(console, ["call:"].concat([].slice.call(arguments)));
    }
  }

  function emit(state, detail) {
    for (var i = 0; i < listeners.state.length; i++) {
      try { listeners.state[i](state, detail); } catch (e) { log("listener", e); }
    }
  }

  // ---- engine differences ----

  function PeerConn() {
    return window.RTCPeerConnection || window.mozRTCPeerConnection || null;
  }
  function SessionDesc() {
    return window.RTCSessionDescription || window.mozRTCSessionDescription || null;
  }
  function IceCandidate() {
    return window.RTCIceCandidate || window.mozRTCIceCandidate || null;
  }

  // Each of these passes callbacks and also watches for a promise. On an engine
  // that implements the legacy overloads the callbacks fire and the return is
  // undefined; on a promise engine the extra arguments are ignored and the
  // thenable resolves. Never both.
  function pcSetRemote(desc) {
    return new Promise(function (res, rej) {
      var r = pc.setRemoteDescription(desc, res, rej);
      if (r && typeof r.then === "function") r.then(res, rej);
    });
  }
  function pcSetLocal(desc) {
    return new Promise(function (res, rej) {
      var r = pc.setLocalDescription(desc, res, rej);
      if (r && typeof r.then === "function") r.then(res, rej);
    });
  }
  function pcCreateAnswer() {
    return new Promise(function (res, rej) {
      var r = pc.createAnswer(res, rej);
      if (r && typeof r.then === "function") r.then(res, rej);
    });
  }
  function pcAddIce(cand) {
    return new Promise(function (res, rej) {
      var r = pc.addIceCandidate(cand, res, rej);
      if (r && typeof r.then === "function") r.then(res, rej);
    });
  }

  // getUserMedia, with the constraint object degraded on failure.
  //
  // Echo cancellation is worth asking for twice over on this hardware: the
  // speaker and the microphone are centimetres apart on a flip phone, and
  // without it the far end hears themselves. But a 2016 engine can reject a
  // constraint object it does not recognise outright, and a call with no
  // microphone is worse than a call with an echo — so a refusal falls back to
  // plain {audio:true} rather than failing.
  function getMic() {
    var rich = {
      audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
      video: false
    };
    return tryMic(rich).catch(function (e) {
      log("rich audio constraints refused, retrying plain:", e && e.name);
      return tryMic({ audio: true, video: false });
    });
  }

  function tryMic(constraints) {
    if (navigator.mediaDevices && navigator.mediaDevices.getUserMedia) {
      return navigator.mediaDevices.getUserMedia(constraints);
    }
    var legacy = navigator.mozGetUserMedia || navigator.getUserMedia ||
                 navigator.webkitGetUserMedia;
    if (!legacy) {
      return Promise.reject(new Error("no getUserMedia in this engine"));
    }
    return new Promise(function (res, rej) {
      legacy.call(navigator, constraints, res, rej);
    });
  }

  function addLocal(stream) {
    // addTrack landed in Firefox 46, so Gecko 48 has it; addStream is the
    // older spelling and is kept because it costs two lines and this is
    // exactly the class of thing a vendor build quietly differs on.
    if (pc.addTrack) {
      var tracks = stream.getAudioTracks ? stream.getAudioTracks() : [];
      for (var i = 0; i < tracks.length; i++) pc.addTrack(tracks[i], stream);
      return;
    }
    if (pc.addStream) { pc.addStream(stream); return; }
    throw new Error("no way to attach the microphone");
  }

  function attachRemote(stream) {
    if (!audioEl) {
      audioEl = document.getElementById("call-audio");
    }
    if (!audioEl) { log("no audio element to play into"); return; }
    // The call belongs on the media channel so the volume keys move it.
    // Guarded: the property does not exist off-device, and setting an audio
    // channel the app has no permission for throws.
    try { audioEl.mozAudioChannelType = "content"; } catch (e) {}

    try {
      if ("srcObject" in audioEl) { audioEl.srcObject = stream; }
      else if ("mozSrcObject" in audioEl) { audioEl.mozSrcObject = stream; }
      else { audioEl.src = URL.createObjectURL(stream); }
    } catch (e) {
      log("attach failed, falling back to object URL:", e);
      try { audioEl.src = URL.createObjectURL(stream); } catch (e2) { log(e2); }
    }
    // play() returns undefined on this engine, so nothing to catch.
    try { audioEl.play(); } catch (e) { log("play:", e); }
  }

  function sendSignal(kind, body) {
    var d = { callid: callID, kind: kind };
    if (body.sdp) d.sdp = body.sdp;
    if (body.candidate) d.candidate = body.candidate;
    window.Wire.send(window.Wire.T.CALLSIGNAL, d);
  }

  // ---- the session ----

  function build() {
    var PC = PeerConn();
    if (!PC) throw new Error("this engine has no RTCPeerConnection");
    // No ICE servers: the daemon has a public address, so its host candidate
    // is directly reachable and our own address is learned from the binding
    // request we send it. A STUN lookup here would only add a round trip.
    pc = new PC({ iceServers: [] });

    pc.onicecandidate = function (ev) {
      if (!ev || !ev.candidate) return; // end of candidates
      sendSignal("ice", { candidate: ev.candidate });
    };

    // ontrack is the modern spelling; onaddstream is what Gecko 48 fires.
    pc.ontrack = function (ev) {
      var s = ev.streams && ev.streams[0];
      if (s) attachRemote(s);
      else if (ev.track) attachRemote(new MediaStream([ev.track]));
    };
    pc.onaddstream = function (ev) {
      if (ev && ev.stream) attachRemote(ev.stream);
    };

    pc.oniceconnectionstatechange = function () {
      log("ice:", pc.iceConnectionState);
      emit("ice", pc.iceConnectionState);
      if (pc.iceConnectionState === "failed") emit("failed", "ice");
    };
    if ("onconnectionstatechange" in pc) {
      pc.onconnectionstatechange = function () {
        log("pc:", pc.connectionState);
        emit("pc", pc.connectionState);
      };
    }
  }

  function handleOffer(sdp) {
    var SD = SessionDesc();
    if (!SD) { emit("failed", "no RTCSessionDescription"); return; }

    try { build(); } catch (e) {
      log("build failed:", e);
      emit("failed", String(e && e.message || e));
      return;
    }

    // The microphone has to be attached BEFORE the answer is created, or the
    // answer describes a receive-only session and the far end never gets our
    // audio — a call that sounds fine to us and is silent to them.
    getMic().then(function (stream) {
      localStream = stream;
      addLocal(stream);
      return pcSetRemote(new SD({ type: "offer", sdp: sdp }));
    }).then(function () {
      haveRemote = true;
      flushPending();
      return pcCreateAnswer();
    }).then(function (answer) {
      return pcSetLocal(answer).then(function () {
        sendSignal("answer", { sdp: answer.sdp });
        emit("answered", null);
      });
    }).catch(function (e) {
      log("answering failed:", e);
      emit("failed", String((e && (e.name || e.message)) || e));
    });
  }

  function flushPending() {
    var IC = IceCandidate();
    for (var i = 0; i < pending.length; i++) {
      try {
        pcAddIce(IC ? new IC(pending[i]) : pending[i]).catch(function (e) {
          log("addIceCandidate:", e);
        });
      } catch (e) { log("candidate:", e); }
    }
    pending = [];
  }

  window.Call = {
    // Whether this engine can do a call at all, so the UI can say so instead
    // of ringing and then failing silently.
    supported: function () {
      return !!(PeerConn() && SessionDesc() &&
        ((navigator.mediaDevices && navigator.mediaDevices.getUserMedia) ||
          navigator.mozGetUserMedia || navigator.getUserMedia));
    },

    // Called when the user answers, before any signalling arrives.
    begin: function (id) {
      callID = id;
      pending = [];
      haveRemote = false;
    },

    // A callsignal frame from the daemon.
    signal: function (d) {
      if (!d) return;
      if (d.callid && callID && d.callid !== callID) {
        log("signalling for another call, ignored");
        return;
      }
      if (!callID && d.callid) callID = d.callid;

      if (d.kind === "offer") { handleOffer(d.sdp); return; }
      if (d.kind === "ice") {
        if (!d.candidate) return;
        if (!haveRemote || !pc) { pending.push(d.candidate); return; }
        var IC = IceCandidate();
        try {
          pcAddIce(IC ? new IC(d.candidate) : d.candidate).catch(function (e) {
            log("addIceCandidate:", e);
          });
        } catch (e) { log("candidate:", e); }
        return;
      }
      if (d.kind === "answer") {
        // The daemon offers; it should never answer us.
        log("unexpected answer from the daemon, ignored");
      }
    },

    end: function () {
      if (localStream) {
        var ts = localStream.getTracks ? localStream.getTracks() : [];
        for (var i = 0; i < ts.length; i++) {
          try { ts[i].stop(); } catch (e) {}
        }
        localStream = null;
      }
      if (pc) {
        try { pc.close(); } catch (e) {}
        pc = null;
      }
      if (audioEl) {
        try {
          audioEl.pause();
          if ("srcObject" in audioEl) audioEl.srcObject = null;
          else audioEl.removeAttribute("src");
        } catch (e) {}
      }
      callID = null;
      pending = [];
      haveRemote = false;
    },

    onState: function (fn) { listeners.state.push(fn); },

    // For the settings screen and for anyone reading a bug report.
    describe: function () {
      if (!pc) return "no call";
      return "ice=" + pc.iceConnectionState +
        (("connectionState" in pc) ? " pc=" + pc.connectionState : "");
    }
  };
})();
