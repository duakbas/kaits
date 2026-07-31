# Orsa WA — KaiOS app (thin client)

The phone half. Talks to the `wad` daemon over one WebSocket. No WhatsApp
protocol lives here — the daemon does all of that. This app is chat list +
thread + an incoming-call screen, driven by the D-pad and softkeys.

## Files

```
manifest.webapp     app manifest (privileged; permission set for msg + future push/audio)
index.html          entry point, screen markup, script load order
css/app.css         sized for 240x320 QVGA, dark, single green accent
js/config.js        >>> EDIT THIS <<< daemon URL + token
js/wire.js          WebSocket client; MUST stay in sync with daemon protocol.go
js/nav.js           D-pad + softkey handling (the KaiOS-specific part)
js/app.js           screen logic and state
icons/              placeholder app icons (original mark, replace freely)
```

## Test it in a desktop browser FIRST (no phone, no KaiOS)

This is the fastest "does it run" check.

1. Start the daemon somewhere reachable (localhost is fine).
2. Edit `js/config.js`: set `DAEMON_WS` to `ws://localhost:8080/ws` and
   `TOKEN` to your `WAD_TOKEN`.
3. Serve the app folder:
   ```
   cd kaits
   python3 -m http.server 8000
   ```
4. Open `http://localhost:8000` in Firefox or Chrome. Resize the window
   narrow (~240px) to see the real layout.
5. Open devtools console. You should see `wire: connecting` then `wire: open`.
   The status dot in the header goes green.

Keyboard maps to phone keys for testing:
- Arrow Up/Down — move focus in the chat list
- Enter — open focused chat / send message
- **F1 = left softkey, F2 = right softkey** (Back / Send, Reject / Answer)
- Backspace / Esc — back

If messages arrive at the daemon they appear in the list. Type in a thread
and press F2 (or Enter) to send.

## What works vs. not (matches the daemon)

Works: connect + reconnect, chat list, threads, receive text/media label,
send text (optimistic echo), incoming-call ring screen + answer/reject
signalling.

Not yet: media rendering (daemon serves a URL; wiring the <img> is a TODO),
chat list is only as good as the daemon's getchats (currently returns empty —
list fills in as live messages arrive), and calls RING but carry no audio
until the WebRTC + meowcaller leg exists.

## Then: KaiOS

Once it behaves in the browser, load it via WebIDE (old Firefox ≤59) pointed
at the KaiOSRT simulator or a real debug-enabled phone. `console.log` shows up
in `adb logcat`. Keep this app strictly on the personal side — the icons are
original but the whole thing is an unofficial WhatsApp client.

## Keep wire.js in sync

If you change a message type string in the daemon's `internal/ws/protocol.go`,
change it in `js/wire.js` too. They are the same contract written twice.
