// settings.js — where the daemon lives, stored on the phone.
//
// config.js is edited on a computer and baked into the package, which is wrong
// twice: a LAN address changes whenever DHCP feels like it, and the token would
// ride along inside every zip uploaded to a submission portal. Both problems go
// away if the phone asks once and remembers.
//
// Loaded immediately after config.js, so anything stored here overrides the
// defaults before wire.js reads them.

(function () {
  var KEY = "kaits.daemon";
  var PREFS = "kaits.prefs";
  var DEFAULT_PORT = "8080";

  // Preferences that belong to the person holding the phone, not to the build.
  // config.js is the default; this is the answer, and it survives an update
  // because it lives on the phone rather than in the package.
  var prefDefaults = {
    // Off by default: the eased scroll is a matter of taste, and the taste that
    // matters here found it worse than an instant jump.
    smoothscroll: false
  };

  function loadPrefs() {
    try {
      var raw = JSON.parse(localStorage.getItem(PREFS) || "{}");
      return raw && typeof raw === "object" ? raw : {};
    } catch (e) { return {}; }
  }
  var prefs = loadPrefs();

  function load() {
    try {
      return JSON.parse(localStorage.getItem(KEY) || "null");
    } catch (e) {
      return null; // private mode, or corrupt — treat as unconfigured
    }
  }

  // normalize turns what someone can realistically type on a T9 keypad into a
  // WebSocket URL. "192.168.1.200" is the whole input worth optimising for;
  // everything else is a convenience for people who paste.
  function normalize(input) {
    var h = String(input || "").trim();
    if (!h) return "";
    // Whether a scheme was typed decides whether we may invent a port. A bare
    // "192.168.1.200" is the case worth helping with; someone who typed
    // wss://host means 443, and forcing :8080 onto it would break a perfectly
    // good address.
    var hadScheme = /^wss?:\/\//i.test(h);
    if (!hadScheme) h = "ws://" + h;

    // Split off scheme so a colon in the host can be told from the scheme's.
    var scheme = h.slice(0, h.indexOf("://") + 3);
    var rest = h.slice(scheme.length).replace(/\/+$/, "");

    var pathAt = rest.indexOf("/");
    var hostport = pathAt >= 0 ? rest.slice(0, pathAt) : rest;
    var path = pathAt >= 0 ? rest.slice(pathAt) : "";

    if (!hadScheme && hostport.indexOf(":") < 0) hostport += ":" + DEFAULT_PORT;
    if (!path) path = "/ws";

    return scheme + hostport + path;
  }

  var stored = load();
  if (stored && stored.url && window.CONFIG) {
    window.CONFIG.DAEMON_WS = stored.url;
    window.CONFIG.TOKEN = stored.token || "";
  }

  window.Settings = {
    // configured() is what decides whether the app opens on the chat list or on
    // the setup screen. Deliberately based on stored values rather than on
    // CONFIG, so a packaged default never counts as "set up".
    configured: function () { return !!(stored && stored.url); },

    current: function () {
      return {
        url: (stored && stored.url) || "",
        token: (stored && stored.token) || "",
        // What to show in the input: the bare host:port, since that's what was
        // typed and what anyone would want to edit.
        host: ((stored && stored.url) || "")
          .replace(/^wss?:\/\//i, "")
          .replace(/\/ws$/, "")
      };
    },

    save: function (host, token) {
      var url = normalize(host);
      if (!url) return null;
      stored = { url: url, token: String(token || "") };
      try {
        localStorage.setItem(KEY, JSON.stringify(stored));
      } catch (e) { /* not persisted; still applies for this run */ }
      if (window.CONFIG) {
        window.CONFIG.DAEMON_WS = stored.url;
        window.CONFIG.TOKEN = stored.token;
      }
      return stored;
    },

    clear: function () {
      stored = null;
      try { localStorage.removeItem(KEY); } catch (e) {}
    },

    // ---- preferences ----
    pref: function (name) {
      return Object.prototype.hasOwnProperty.call(prefs, name)
        ? !!prefs[name]
        : !!prefDefaults[name];
    },

    setPref: function (name, on) {
      prefs[name] = !!on;
      try { localStorage.setItem(PREFS, JSON.stringify(prefs)); } catch (e) {}
      return prefs[name];
    },

    togglePref: function (name) {
      return window.Settings.setPref(name, !window.Settings.pref(name));
    },

    // Exposed for the setup screen's preview line, so what will be connected to
    // is visible before committing to it.
    preview: normalize
  };
})();
