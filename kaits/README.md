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

## When it doesn't connect

A WebSocket that never opens tells you nothing. A wrong hostname, a port
nothing listens on, a certificate the phone won't accept and a token off by
one character all fail in exactly the same silent way, and the app sits on
"No chats yet. Waiting for messages…" for ever.

So the Settings screen asks the daemon directly, over plain HTTP, every time
you open it, and prints what it learned:

```
address: wss://your.host/ws
token: 32 chars, ends 1537
socket: NEVER opened (7 attempts)
  last close: 1006 (died below the websocket — dns, tls, port, or an http error)
server: reachable, but TOKEN REJECTED (401) — the address is right, the token is not
```

That `server:` line is the diagnosis. It works because `/ws` checks the token
*before* attempting the upgrade, so an ordinary GET separates the two failures:

| What comes back | What it means |
| --- | --- |
| `400 Bad Request` | address and token are both right — the fault is elsewhere |
| `401 unauthorized` | you reached the daemon; the token is wrong |
| `404` | that hostname serves something else, or the path isn't `/ws` |
| `502` / `503` | the reverse proxy is up and `wad` is not |
| nothing at all | hostname, port, or TLS |

The same table works from any browser, including the phone's own, which is the
way to check a handset the app isn't installed on yet:

    https://your.host/ws?token=YOUR_TOKEN

The probe needs the `systemXHR` permission, because the daemon sends no CORS
headers and the app is not same-origin with it. In a desktop browser it has no
such permission, so a failure there may be the browser rather than the server —
the panel says so when that's the case.

### The one the browser hides from you

If the phone shows **"your connection is not secure"** on ordinary sites — try
`x.com`, whose certificate chain is in every root store there is — then it is
refusing TLS across the board, and the app has no way around it. In the browser
you can click through the warning. A `wss://` socket from a packaged app
cannot: there is no interstitial and no "proceed anyway", so it fails at the
TLS layer, closes 1006 and never says why. A phone can therefore appear to
browse the web perfectly while this app never connects once.

The usual cause is the clock. A handset that has sat with a flat battery
resumes at its manufacture date, every certificate reads as not-yet-valid, and
the *time of day* looks entirely normal — it's the year that's wrong. Settings
→ Date & time.

The app checks this itself now, against the build stamp baked in at package
time: it cannot be running before it was built, so a date earlier than that is
proof the clock is wrong, and the settings screen says so above everything
else. That is the only such check possible offline, which is the situation a
wrong clock creates.

Two things worth knowing while reading that panel:

- **`socket: NEVER opened`** is categorically different from a dropout. It
  means this handset has not once reached the daemon, so the address, the
  token or the network has never been right — not that the connection is
  flaky.
- **`waiting to send: N`** counts on every launch that finds no socket, because
  the push endpoint is queued at boot. A number that keeps climbing across
  restarts is the same "never connected" story told another way.

Run `node test/addr_test.js` after touching anything that builds, stores or
interprets that address.

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
