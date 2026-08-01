// EDIT THIS FILE. These two values point the app at your daemon.
//
// While developing in a desktop browser, run the daemon locally and use:
//   ws://localhost:8080/ws
// On the real phone over the internet, you MUST use wss:// (TLS) or KaiOS
// may refuse the connection and your token travels in the clear:
//   wss://your-domain/ws
//
// TOKEN must match WAD_TOKEN you set when starting the daemon.

window.CONFIG = {
  DAEMON_WS: "ws://localhost:8080/ws",
  TOKEN: "changeme",

  // Reconnect backoff (ms). KaiOS backgrounds the app aggressively, so the
  // socket WILL drop; this controls how fast we retry.
  RECONNECT_MIN: 1000,
  RECONNECT_MAX: 15000,

  // Show the on-screen key log from startup. You rarely need this: pressing *
  // three times toggles it on the phone at any point, which is easier than
  // rebuilding. Errors appear on that strip regardless of this setting.
  DEBUG_KEYS: false,

  // Buzz when a message arrives in the chat you already have open. Off by
  // default: you're looking straight at it, so the buzz tells you nothing the
  // screen didn't. Flip it if you'd rather feel every message.
  VIBRATE_IN_OPEN_CHAT: false,

  // How the app alerts you. "auto" follows the phone's own notification
  // profile — silent mode silences the beep, vibration-off stops the buzz —
  // and falls back to the values below when the phone won't tell us (always
  // the case in a desktop browser, and possibly on the phone, since reading
  // settings is permission-gated).
  //
  // "always" and "never" ignore the phone and do what they say.
  //
  // Fallbacks when nothing is known: vibrate yes, beep no. A missed buzz beats
  // a browser tab beeping at you in a meeting.
  NOTIFY_VIBRATE: "auto",
  NOTIFY_SOUND: "auto",

  // Play a near-silent loop while the app is in the background.
  //
  // KaiOS kills a backgrounded app as soon as something else wants the memory,
  // and with push measured dead the app holding its own socket is the ONLY
  // thing that delivers a message here — so being reaped is not a cosmetic
  // problem, it's the notification design failing. An app that is playing
  // audio is treated as perceptibly doing something and moves up the priority
  // list, which is how a music player survives backgrounding.
  //
  // It costs battery, and it is a demotion of risk rather than a guarantee.
  // The setup screen reports how long the app actually survived each time it
  // was backgrounded, so this can be judged on measurements instead of faith:
  // turn it off, use the phone for a day, and compare.
  KEEPALIVE: true,

  // Which audio channel the keepalive plays on. This is the one knob that
  // decides whether the trick works AND whether it is antisocial:
  //
  //   "normal"  — non-exclusive. Should not stop your music or any other
  //               sound. May or may not be enough for the platform to count
  //               the app as busy; if kills continue with the keepalive on and
  //               playing, that is what this being too polite looks like.
  //   "content" — the media channel. Certain to count, and certain to stop
  //               whatever else is using it every time you leave the app.
  //
  // Start on "normal". Only reach for "content" if the Settings screen shows
  // the keepalive playing and the kill rate has not moved.
  KEEPALIVE_CHANNEL: "normal"
};
