// Tests for the two pieces of address handling that have each broken a real
// phone: what a typed address normalizes to, and what the app concludes when
// the daemon answers.
//
// Run with:  node kaits/test/addr_test.js
//
// There is no framework here on purpose. These modules are plain IIFEs that
// hang things off window, so a fake window and a handful of assertions test
// them exactly as the phone loads them.

"use strict";

const fs = require("fs");
const path = require("path");
const vm = require("vm");

const JS = path.join(__dirname, "..", "js");

let failures = 0;
function check(what, got, want) {
  const ok = JSON.stringify(got) === JSON.stringify(want);
  if (!ok) {
    failures++;
    console.log("FAIL  " + what + "\n      got  " + JSON.stringify(got) +
                "\n      want " + JSON.stringify(want));
  }
}

// A window with just enough of a phone in it to load the modules under test.
function makeWindow(opts) {
  opts = opts || {};
  const store = opts.store || {};
  const listeners = [];
  const win = {
    CONFIG: { DAEMON_WS: "ws://localhost:8080/ws", TOKEN: "changeme",
              RECONNECT_MIN: 1000, RECONNECT_MAX: 15000 },
    addEventListener: function () { listeners.push(arguments); },
    localStorage: {
      getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
      setItem: function (k, v) { store[k] = String(v); },
      removeItem: function (k) { delete store[k]; }
    }
  };
  win.window = win;
  win.document = { addEventListener: function () {}, hidden: false };
  win.console = { log: function () {}, warn: function () {}, error: function () {} };
  win.setTimeout = function () { return 0; };
  win.clearTimeout = function () {};
  win.WebSocket = function () {};
  win.WebSocket.OPEN = 1;
  win.WebSocket.CONNECTING = 0;
  win.XMLHttpRequest = opts.XHR || function () {};
  return win;
}

function load(win, file) {
  const ctx = vm.createContext(win);
  vm.runInContext(fs.readFileSync(path.join(JS, file), "utf8"), ctx, { filename: file });
  return win;
}

// ---------------------------------------------------------------- normalize
//
// The rule that matters: a scheme you typed is a scheme you meant. Inventing
// :8080 for a bare LAN address is a kindness; inventing it for wss://host
// would point the phone at a port nothing is listening on, and silently.
(function testNormalize() {
  const win = load(makeWindow(), "settings.js");
  const n = win.Settings.preview;

  check("bare host gets ws, the default port and /ws",
    n("192.168.1.200"), "ws://192.168.1.200:8080/ws");
  check("bare host with a port keeps that port",
    n("192.168.1.200:9999"), "ws://192.168.1.200:9999/ws");
  check("wss with no port means 443, NOT 8080",
    n("wss://deniz.example.ch"), "wss://deniz.example.ch/ws");
  check("wss with a port keeps it",
    n("wss://deniz.example.ch:8080"), "wss://deniz.example.ch:8080/ws");
  check("a full url is left alone",
    n("wss://deniz.example.ch/ws"), "wss://deniz.example.ch/ws");
  check("a custom path survives",
    n("wss://deniz.example.ch/wa/ws"), "wss://deniz.example.ch/wa/ws");
  check("surrounding whitespace is not an address",
    n("  wss://deniz.example.ch/ws  "), "wss://deniz.example.ch/ws");
  check("nothing typed is nothing", n(""), "");
})();

// ------------------------------------------------------- saving and reading
//
// A stored wss address has to come back out of the settings screen unchanged.
// It did not, once: the screen stripped the scheme for display, the next save
// read it back as schemeless, and normalize turned it into plain ws — quietly
// downgrading the connection and putting the token on the wire in clear.
(function testRoundTrip() {
  const store = {};
  let win = load(makeWindow({ store: store }), "settings.js");
  win.Settings.save("wss://deniz.example.ch/ws", "tok");
  const shown = win.Settings.current().host;
  check("a wss address is shown in full", shown, "wss://deniz.example.ch/ws");

  win.Settings.save(shown, "tok");
  check("saving what was shown does not downgrade the scheme",
    win.Settings.current().url, "wss://deniz.example.ch/ws");

  // The LAN case the stripping exists for still works.
  win.Settings.save("192.168.1.200", "tok");
  check("a bare LAN address is shown without the scheme",
    win.Settings.current().host, "192.168.1.200:8080");
  win.Settings.save(win.Settings.current().host, "tok");
  check("and survives a re-save",
    win.Settings.current().url, "ws://192.168.1.200:8080/ws");
})();

// ------------------------------------------------------------------ httpURL
//
// The probe asks the same place over HTTP. wss must map to https: asking the
// TLS port in the clear would fail and be reported as "no answer", accusing a
// server that is working.
(function testHttpURL() {
  const cases = [
    ["wss://deniz.example.ch/ws", "https://deniz.example.ch/ws"],
    ["ws://192.168.1.200:8080/ws", "http://192.168.1.200:8080/ws"],
    ["wss://deniz.example.ch:8080/ws", "https://deniz.example.ch:8080/ws"],
    ["", ""],
    ["https://not-a-socket/ws", ""]
  ];
  for (const [ws, want] of cases) {
    const win = makeWindow();
    win.CONFIG.DAEMON_WS = ws;
    load(win, "wire.js");
    check("httpURL(" + JSON.stringify(ws) + ")", win.Wire.httpURL(), want);
  }
})();

// ----------------------------------------------------------------- classify
//
// The whole point of the probe: these five answers are five different jobs for
// the person holding the phone, and the WebSocket reports all of them as the
// same nothing.
(function testClassify() {
  const win = load(makeWindow(), "wire.js");
  const c = win.Wire.classify;
  check("401 is a rejected token, not a bad address", c(401).kind, "badtoken");
  check("400 means the daemon read the token and liked it", c(400).kind, "ok");
  check("426 likewise", c(426).kind, "ok");
  check("404 means something else is on that hostname", c(404).kind, "notwad");
  check("502 means the proxy is up and wad is down", c(502).kind, "daemondown");
  check("503 likewise", c(503).kind, "daemondown");
  check("0 means nothing answered at all", c(0).kind, "unreachable");
  check("200 is not something wad's /ws ever says", c(200).kind, "odd");
})();

// -------------------------------------------------------------------- probe
//
// The probe must send the token, must ask over HTTP rather than WebSocket, and
// must report a transport failure as unreachable rather than throwing.
(function testProbe() {
  const seen = {};
  function XHR() {
    this.mozSystem = true;
    this.open = function (m, u) { seen.method = m; seen.url = u; };
    this.send = function () { const self = this; seen.sent = true; self.status = 401; self.onload(); };
  }
  const win = makeWindow({ XHR: XHR });
  win.CONFIG.DAEMON_WS = "wss://deniz.example.ch/ws";
  win.CONFIG.TOKEN = "abc def";           // a space, to prove it is encoded
  load(win, "wire.js");

  let got = null;
  win.Wire.probe(function (r) { got = r; });
  check("the probe asks over https", seen.url,
    "https://deniz.example.ch/ws?token=abc%20def");
  check("with GET", seen.method, "GET");
  check("and reports a 401 as a bad token", got.kind, "badtoken");
  check("and says whether it was privileged", got.mozSystem, true);

  // A dead network: onerror fires, nothing throws, the verdict is honest.
  function DeadXHR() {
    this.mozSystem = false;
    this.open = function () {};
    this.send = function () { this.onerror(); };
  }
  const win2 = makeWindow({ XHR: DeadXHR });
  win2.CONFIG.DAEMON_WS = "wss://nowhere.invalid/ws";
  load(win2, "wire.js");
  let got2 = null;
  win2.Wire.probe(function (r) { got2 = r; });
  check("a transport failure is unreachable", got2.kind, "unreachable");
  check("and admits it could not run privileged", got2.mozSystem, false);

  // No address at all must not attempt a request.
  const win3 = makeWindow({ XHR: function () { throw new Error("must not construct"); } });
  win3.CONFIG.DAEMON_WS = "";
  load(win3, "wire.js");
  let got3 = null;
  win3.Wire.probe(function (r) { got3 = r; });
  check("no address is answered without asking anyone", got3.kind, "noaddress");
})();

// --------------------------------------------------------------------- diag
//
// "Never opened" is the fact that separates a phone that has never been set up
// correctly from one that is merely disconnected right now, and it is the one
// the second handset needed.
(function testDiag() {
  const win = makeWindow();
  win.CONFIG.DAEMON_WS = "wss://deniz.example.ch/ws";
  win.CONFIG.TOKEN = "45ce788f1d880c1458fde9f6bd611537";
  load(win, "wire.js");

  const d = win.Wire.diag();
  check("a fresh app has never opened a socket", d.everOpened, false);
  check("and has not tried yet", d.attempts, 0);
  check("the address is reported for comparison", d.address,
    "wss://deniz.example.ch/ws");
  check("the token is described, not printed", d.tokenLength, 32);
  check("only its tail is shown", d.tokenTail, "1537");

  // One failed dial: the socket constructor throws nothing here, but our fake
  // WebSocket never calls onopen, so this is the real-world shape of a phone
  // pointed at the wrong host.
  win.Wire.connect();
  const d2 = win.Wire.diag();
  check("an attempt is counted", d2.attempts, 1);
  check("and it still has never opened", d2.everOpened, false);
})();

if (failures) {
  console.log("\n" + failures + " failure(s)");
  process.exit(1);
}
console.log("addr: all checks passed");
