# whatsapp-kaios

A self-hosted WhatsApp client for a KaiOS feature phone.

Personal project, not a product. It pairs as a WhatsApp **linked device** — the
same mechanism WhatsApp Web uses — and renders a chat UI sized for a 240×320
screen driven entirely by a D-pad and two softkeys.

> **Unofficial client.** This talks to WhatsApp through a reverse-engineered
> library. It goes against WhatsApp's terms of service, it can get the account
> banned, and it breaks whenever Meta changes something. Use an account you can
> afford to lose. Don't distribute it.

---

## How it's put together

Two halves, talking over one WebSocket:

```
 KaiOS phone                    your machine / the phone itself
┌──────────────┐   JSON over   ┌──────────────┐   whatsmeow   ┌──────────┐
│   wa-kaios   │ ◄───────────► │     wad      │ ◄───────────► │ WhatsApp │
│  (HTML/JS)   │   WebSocket   │  (Go daemon) │               └──────────┘
└──────────────┘               └──────────────┘
```

**`wad`** — the daemon. Owns the WhatsApp session, all protocol work, and
persistence. Speaks a small JSON protocol and nothing else.

**`wa-kaios`** — the app. A thin client: it renders UI and speaks that protocol.
It never touches WhatsApp protocol directly, which is what keeps it small enough
to run on Gecko 48.

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

wa-kaios/                     the app
  index.html                  screens
  js/config.js                >>> EDIT THIS <<< daemon URL + token
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
cd wa-kaios
python3 -m http.server 8000
```

Open `localhost:8000`, devtools → device toolbar → 240×320. Defaults in
`js/config.js` already match the daemon's, so local dev needs no config.

**Keys in the browser:** arrows = D-pad, Enter = select/send, **F1/F2 =
softkeys**, Backspace/Esc = back.

Full guide, environment variables, and the LID repair passes:
[`wad/RUN.md`](wad/RUN.md).

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
| Settings screen | ❌ planned |
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

Works end to end on the desktop dev setup. **Not yet run on real hardware** —
the target is an Energizer E282SC+ (MediaTek MT6739, KaiOS 2.5). Questions only
the device can answer: Opus decoding for voice notes, H.264 video, animated
stickers, and whether Web Push can wake the app from standby — which decides
whether background notifications and ringing are possible at all.

Calls need a real audio path (meowcaller plus Opus↔MLOW transcoding) and are
deliberately deferred.

## Don't commit the database

`wa-session.db` **is** your logged-in WhatsApp session, and
`wa-session.db.history.db` is every message. `.gitignore` covers them from any
directory, but glance at `git status` before committing anyway.
