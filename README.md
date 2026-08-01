# Kaits

A self-hosted chat client for a KaiOS feature phone.

Personal project, not a product. Two halves: a daemon that owns a messaging
account and speaks a small JSON protocol, and an app that renders a chat UI
sized for a 240×320 screen, driven entirely by a D-pad and two softkeys.

The daemon that exists today is `wad`, which connects to WhatsApp as a **linked
device** — the same mechanism WhatsApp Web uses. Nothing in the app knows that;
it speaks the protocol and nothing else, so a second daemon for another service
would need no changes on the phone.

> **Unofficial client.** This talks to WhatsApp through a reverse-engineered
> library. It goes against WhatsApp's terms of service, it can get the account
> banned, and it breaks whenever Meta changes something. Use an account you can
> afford to lose. Don't distribute it.

---

## How it's put together

Two halves, talking over one WebSocket:

```
 KaiOS phone                    your machine
┌──────────────┐   JSON over   ┌──────────────┐   whatsmeow   ┌──────────┐
│    kaits     │ ◄───────────► │     wad      │ ◄───────────► │ WhatsApp │
│  (HTML/JS)   │   WebSocket   │  (Go daemon) │               └──────────┘
└──────────────┘               └──────────────┘
```

**`wad`** — the daemon. Owns the WhatsApp session, all protocol work, and
persistence. Speaks a small JSON protocol and nothing else.

**`kaits`** — the app. A thin client: it renders UI and speaks that protocol.
It never touches a messaging protocol directly, which is what keeps it small
enough to run on Gecko 48 — and what would let a different daemon reuse it.

The daemon is pure Go (CGO only for SQLite), so it cross-compiles to the phone's
aarch64 — the eventual goal is running both halves on-device. During development
it runs on a desktop and the app is served over HTTP.

### Layout

```
wad/                          the daemon
  cmd/wad/main.go             entrypoint, HTTP routes, frame router
  internal/ws/                the wire protocol + WebSocket hub
  internal/wa/
    client.go                 whatsmeow events, name resolution
    lidmap.go                 LID ↔ phone resolution (see below)
    history.go                our own SQLite store
    chatactions.go            pin / mute / archive / delete
    profile.go                contact + group info
    media.go                  media cache, avatars
  internal/calls/             call signalling (audio is stubbed)

kaits/                     the app
  index.html                  screens
  js/settings.js              daemon address, entered on the phone
  js/config.js                defaults only; the phone overrides them
  js/wire.js                  protocol client (mirrors internal/ws/protocol.go)
  js/nav.js                   D-pad + softkey handling
  js/app.js                   screen logic
  js/life.js                  lifecycle flight recorder (survives being killed)
  js/keepalive.js             near-silent loop that resists background reaping
```

`js/wire.js` and `internal/ws/protocol.go` are one contract written twice.
Change one, change the other.

---

## Running it

Both halves at once:

```bash
./dev.sh --pull
```

That updates from git, serves the app on `localhost:8000`, and runs the daemon
in the foreground — which is where the pairing QR and every useful log line
appears. Ctrl-C stops both. Environment variables pass through, so the one-shot
modes still work (`WAD_MIGRATE_LIDS=1 ./dev.sh`).

Open it in **Firefox**: Chrome refuses to subscribe for push without an
`applicationServerKey`, which KaiOS doesn't use, so push looks broken there.

To run the two halves separately:

**Daemon:**

```bash
cd wad
./setup.sh          # first time: resolves whatsmeow, builds
./run.sh            # or: WAD_TOKEN=changeme go run ./cmd/wad
```

First run prints a QR in the terminal. On your phone: WhatsApp → Settings →
Linked devices → Link a device → scan.

**App:**

```bash
cd kaits
python3 -m http.server 8000
```

Open `localhost:8000`, devtools → device toolbar → 240×320. The app asks for
the daemon's address on first run and remembers it; `js/config.js` only holds
the defaults it starts from.

**Keys in the browser:** arrows = D-pad, Enter = select/send, **F1/F2 =
softkeys**, Backspace/Esc = back.

Full guide, environment variables, and the LID repair passes:
[`wad/RUN.md`](wad/RUN.md). To run the daemon on a server so the phone works
away from your Wi-Fi: [`wad/DEPLOY.md`](wad/DEPLOY.md).

> Chrome caches JS hard. After editing app JS, hard-reload with devtools open
> and "Disable cache" ticked, or you'll debug code that isn't running.

---

## About LIDs, because it explains a lot of the code

Modern WhatsApp addresses people by **LID** (`<id>@lid`) rather than phone
number, and the same person gets a *different LID in every group*. Meanwhile the
name you saved in your address book syncs keyed by their **phone** JID. So the
naive lookup — take the sender's address, ask for a name — misses, and you get a
screen full of 15-digit numbers.

The daemon reads whatsmeow's own `whatsmeow_lid_map` and `whatsmeow_contacts`
tables directly (read-only, alongside the normal API) and checks both an address
and its phone↔LID counterpart. Names resolve in this order:

1. a nickname you saved in this app
2. your address book
3. the name the contact set for themselves on WhatsApp
4. the bare phone number

Where both the phone and LID rows carry a name, the **phone** row wins — that's
where your saved name lives, and picking the other one is how the same person
ends up displayed differently in different groups.

Messages stored before all this keep whatever name they were saved with.
`WAD_MIGRATE_LIDS=1` repairs them in place; `WAD_RESYNC=1` pulls a fresh contact
list from WhatsApp first and then repairs. Both are one-shot, safe to re-run, and
neither requires unlinking. See [`wad/RUN.md`](wad/RUN.md).

---

## What works

| | |
|---|---|
| Pair via QR | ✅ |
| Persistent history across restarts | ✅ 35k+ messages |
| Send / receive text | ✅ |
| Receive photo, video, GIF, sticker, voice note | ✅ |
| Reply, forward, delete own message | ✅ |
| Reply privately / DM a group sender | ✅ |
| Contact + group info screen | ✅ |
| Save a contact (in-app; phone address book on KaiOS) | ✅ |
| Pin / mute / archive / delete chat | ✅ syncs to the account |
| Mentions, avatars, per-person colours in groups | ✅ |
| **Send** photos, video or audio | ✅ 📎 in the composer, or press Left |
| **Send** an arbitrary document | ⚠️ only if some app on the phone claims the type |
| **Record** a voice note | ❌ playback only — needs a `privileged` build, see below |
| **Call audio** | ❌ signalling only — it rings, there's no sound |
| Settings screen | ✅ daemon address + token, entered on the phone |
| Polls | ❌ not rendered at all, let alone votable |
| Contact cards | ❌ not rendered at all |
| **Send** a location | ✅ 📍 in the attach menu |
| **Share live location** | ✅ 15 min / 1 h / 8 h, stoppable; update rendering unverified |

Pin, mute, archive and delete are **real account changes**, not local
preferences: they sync to your phone and every other linked device, and delete
is not undoable.

Attaching goes through a `pick` activity, which only opens if some app on the
phone has registered as a handler for that MIME type. Photos, video and audio
have owners — Gallery, Video, Music. Arbitrary documents often don't: a stock
KaiOS build has no Files app, so nothing answers `*/*` and the picker never
appears. The attach menu therefore tries a list of types in turn rather than
asking for `*/*` alone, and says so plainly when the phone genuinely has no
app that can pick a file. Whatever comes back is sent as what it actually is,
so a photo picked through "File" still arrives as a photo.

Saving a contact is two separate things, because WhatsApp has no contact-write
API — the address book syncs one way, phone → account. The in-app nickname is
stored by the daemon; on KaiOS the app *also* writes the number to the phone's
address book via `mozContacts`, falling back to a `webcontacts/contact` activity.

---

## Status

Runs on real hardware. Installed on an Energizer E282SC (KaiOS 2.5) through the
submission portal's **Test Device** route — an IMEI whitelist that installs via
KaiStore, needing no debug access, which matters because that handset is
debug-locked with unauthorised ADB.

**Background push does not work, and it isn't our fault.** KaiOS's own push
service (`notification.kaiostech.com`) accepts a push with HTTP 200 and never
delivers it — including for an endpoint that has been explicitly unsubscribed,
which a service tracking its subscriptions would reject. Keyless subscribe
works, so the payload-free design is sound; there is simply nothing at the other
end. See [`pushtest/README.md`](pushtest/README.md) for the full measurement.

The fallback works instead: the app holds its WebSocket and raises notifications
itself, which needs no push service. That depends on the app staying alive with
the phone shut — measured at over 20 minutes idle, surviving both the camera and
a phone call. Backgrounded timers get throttled to minutes, but the app runs at
full speed the moment anything wakes the device, which is what an arriving
message does.

### Being killed in the background

That measurement was a good day. In practice the app is sometimes reaped within
seconds of leaving it, and sooner if you then go and use another app — which is
the process priority manager doing its job: a backgrounded app is the cheapest
thing to kill when something else wants memory.

This can't be investigated from inside a running app, because a killed process
runs none of your code on the way out. So `js/life.js` writes a heartbeat to
localStorage every 15 seconds while backgrounded, and a goodbye on a clean exit.
On the next launch it reads back the wreckage: a goodbye means you closed it, no
goodbye means it was killed, and the last heartbeat says when. The setup screen
reports the last session, the kill rate across recent sessions, and a list of
them — which turns "I think it closes soon" into a number.

`js/keepalive.js` is the mitigation: a near-silent one-second loop
(`audio/keepalive.wav`, generated by `audio/mkkeepalive.py`) played only while
the app is out of sight. An app that is playing audio counts as perceptibly
doing something and moves up the priority list; that is how a music player
survives backgrounding. It plays on the `normal` audio channel rather than
`content`, deliberately — starting `content` playback interrupts whatever else
is using that channel, so a keepalive on it would pause your music every time
you left the app, which is a worse bug than the one being fixed.

It costs battery and it is a demotion of risk, not a guarantee. `KEEPALIVE` in
`js/config.js` turns it off, and since the app now measures its own survival,
that question is settled by running it both ways for a day rather than by
argument.

**The bigger lever was memory.** Within the same priority class the largest
process is the one killed, and this app was enormous for what it is: bubbles
rendered the full-size photo, and CSS `max-width` changes how an image is
*drawn*, not how it is *decoded*. A 1600×1200 photo in a 160px bubble still
costs about 7.7 MB of decoded bitmap, so a thread with six photos carried ~46 MB
of pixels — against a few kilobytes for a page with no images, which is exactly
why `pushtest/` ran for hours where the real app died in seconds.

WhatsApp ships a small preview inside every media message, and the daemon
already stored those for locations. It now stores them for photos, video and
stickers too, serves them at `/thumb/<msgid>`, and the app renders *those* in
bubbles — fetching the full file only when a photo is opened full-screen. Video
elements get the preview as a `poster` with `preload="none"`, so nothing of the
video is decoded until it is played. Messages received before this have no
stored preview and fall back to the full file exactly as before.

Still unanswered on device: Opus decoding for voice notes, H.264 video, animated
stickers, and real notification latency end to end.

**Calls are blocked, not deferred.** Three independent reasons: `audio-capture`
is a privileged permission and this is a `web`-type app; the KaiStore agreement
forbids VoIP over the cellular network; and the most experienced KaiOS app
developers report voice calls are not achievable on this platform at all. The
first is the only one that might move.

### The privileged build

Four things the app already has code for are dormant because a `web`-type app
isn't granted the permission: recording a voice note (`audio-capture`), reading
the phone's ringer profile (`settings`), reading and writing the address book
(`mozContacts`), and putting the alert tone on the notification audio channel.
Each is written behind a feature check, so today they simply don't fire.

Only one of the three reasons above applies to any of them. Recording a voice
note is not VoIP — it's `getUserMedia`, an encode, and an upload — so the
KaiStore cellular clause has nothing to say about it, and neither does the
"calls aren't achievable" report. The single gate is the app type, and the app
type is a request that has never been made.

    ./kaits/package.sh 1.1.9 privileged

builds the same app with `"type": "privileged"` and those permissions declared.
The zip is otherwise byte-for-byte the same work; if the submission is refused,
rebuild without the argument and nothing is lost but the round trip. The build
refuses to produce a privileged package containing an inline `<script>`, an
inline event handler, or `eval` — a privileged package runs under an enforced
`script-src 'self'`, and each of those would work in every browser you'd test
in and be silently dead on the phone.

Whatever the phone can encode is good enough, incidentally: if `MediaRecorder`
there yields something other than Ogg/Opus, `wad` runs on a real machine and
can transcode before sending. The encoder is not the gate either.

`geolocation` needed none of this — it's available to a `web`-type app, so it
is declared in the manifest and sending a location is built and shipping.

### What "live location" does and doesn't do

A pin is one message and behaves exactly as you'd expect. A live share is a
session: an opening `LiveLocationMessage` declaring the duration, then updates
every 30 seconds carrying an increasing sequence number and the elapsed time,
each pointing back at the opening message.

The daemon owns that session, not the app — it holds the connection, it assigns
the sequence numbers, and its clock keeps running while the phone's timers are
throttled to minutes with the screen off. The app produces fixes; when the
daemon says the share is over, the app stops, and *that* is what switches the
GPS back off.

**Unverified:** whether receiving clients animate the pin from those updates or
render them as separate cards. The opening message is the part to trust; the
update path is built to the shape of the protocol and has not been watched from
a second phone. Everything else here is defensive by choice — 0,0 is refused
(it's a failed fix reported as a success, not a place in the Atlantic), the
duration is clamped to eight hours, updates are never queued through a
reconnect because a replayed position is a lie, and the thread shows a bar for
as long as a share is running so it can't drain the battery unseen.

## Prior art worth reading

[strukturart/flop](https://github.com/strukturart/flop) (MIT) is a KaiOS app
that already does several things sitting in the ❌ column above: sharing a
location, sharing a *live* location, and recording as well as playing voice
messages. It's the closest thing to a worked example of those APIs on this
exact platform, so it's the first place to look when any of them come up
rather than guessing at what Gecko 48 supports.

MIT means its code *can* be reused with attribution, but the useful thing here
is the technique, not the lines: how it gets a fix out of `navigator.geolocation`
on a feature phone, how it drives `getUserMedia` and what it does with the
result, how it handles the audio channel. Those get adapted to our own screens
and our own daemon protocol.

[cyan-2048/solid-telekram](https://github.com/cyan-2048/solid-telekram) (GPL-3.0)
is the other reference — the full-screen media viewer and the eased list
scrolling here are both modelled on how it behaves. Its licence would attach to
distribution; this client isn't distributed, and nothing was copied verbatim
regardless.

## Don't commit the database

`wa-session.db` **is** your logged-in WhatsApp session, and
`wa-session.db.history.db` is every message. `.gitignore` covers them from any
directory, but glance at `git status` before committing anyway.
