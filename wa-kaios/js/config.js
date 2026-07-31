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

  // Log every key the app doesn't already handle. Hardware key names differ
  // between KaiOS devices and builds, so when a button does nothing on your
  // phone, turn this on and read the console rather than guessing.
  DEBUG_KEYS: false,

  // Buzz when a message arrives in the chat you already have open. Off by
  // default: you're looking straight at it, so the buzz tells you nothing the
  // screen didn't. Flip it if you'd rather feel every message.
  VIBRATE_IN_OPEN_CHAT: false
};
