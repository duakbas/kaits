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
| **Send** photos or documents | ✅ 📎 in the composer, or press Left |
| **Record** a voice note | ❌ playback only |
| **Call audio** | ❌ signalling only — it rings, there's no sound |
| Settings screen | ✅ daemon address + token, entered on the phone |
| Polls | ❌ not rendered at all, let alone votable |
| Contact cards | ❌ not rendered at all |
| **Send** a location, or share live location | ❌ received ones render + open in maps |

Pin, mute, archive and delete are **real account changes**, not local
preferences: they sync to your phone and every other linked device, and delete
is not undoable.

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

Still unanswered on device: Opus decoding for voice notes, H.264 video, animated
stickers, and real notification latency end to end.

**Calls are blocked, not deferred.** Three independent reasons: `audio-capture`
is a privileged permission and this is a `web`-type app; the KaiStore agreement
forbids VoIP over the cellular network; and the most experienced KaiOS app
developers report voice calls are not achievable on this platform at all. The
first is the only one that might move.

## Don't commit the database

`wa-session.db` **is** your logged-in WhatsApp session, and
`wa-session.db.history.db` is every message. `.gitignore` covers them from any
directory, but glance at `git status` before committing anyway.
