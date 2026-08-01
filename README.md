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
| **Send** an arbitrary document | ❌ at `web` level — no app claims the type; needs the `privileged` build's SD-card access |
| **Record** a voice note | ❌ playback only — needs a `privileged` build, see below |
| **Call audio** | ❌ signalling only — it rings, there's no sound |
| Settings screen | ✅ daemon address, token, and preference switches |
| Polls | ❌ not rendered at all, let alone votable |
| Contact cards | ❌ not rendered at all |
| Open a link in a message | ✅ links are marked in the text; centre key opens, several give a chooser |
| **Send** a location | ✅ 📍 in the attach menu, GPS with a coarse fallback indoors |
| **Share live location** | ✅ 15 min / 1 h / 8 h, stoppable; update rendering unverified |

There is no pointer on this phone, so a link cannot be clicked. Links are
marked in the message text, and selecting the message puts **OPEN** on the
centre key — the same way selecting a photo puts VIEW there. Several links in
one message give a chooser rather than a guess. Message text is turned into
nodes rather than markup, so a message containing HTML stays text: anyone who
can message you can otherwise inject into the app, and a packaged app is not a
safe place to be casual about that.

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
so a photo picked through a Files app still arrives as a photo.

What it deliberately does *not* do is fall through to the media pickers. That
made "File" open the camera roll, which is a lie about what was picked and
redundant besides — Photo, Video and Audio are their own entries in the same
menu.

For documents it now tries five shapes before concluding anything: `*/*`,
`application/*`, `text/*`, a list of concrete types, and finally a request with
**no type filter at all** — a handler whose filter cannot fail should match
that one, which ought to produce the system's own chooser. Wildcards in a
*request* do not match a handler's filter on this platform, which is why `*/*`
fails even though Gallery handles `image/*`, so each shape has to be asked
separately.

If every activity is refused it falls back to a plain `<input type="file">` —
the ordinary "upload a file" control. That was previously reachable only in a
desktop browser, because the presence of `MozActivity` was treated as proof the
better path existed, so the most familiar way to answer this question was never
tried on the phone at all. Whether Gecko 48 answers it there with a real picker
or routes it back into the same activity system is untested; it costs nothing
to try and it is the last thing tried.

`device-storage:sdcard` remains in the privileged build's permission set as the
route that does not depend on any of this.

Location asks for GPS first and falls back to the coarse provider. GPS indoors
on a feature phone means no fix at all — you can sit through the entire timeout
and get nothing — while cell towers and Wi-Fi answer in a second or two to
within a few hundred metres. The accuracy travels with the message, so the
recipient's map draws an honest circle rather than a false pin, and the app
says "approximate" when that is what it sent. A denied permission stops there
rather than re-prompting.

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

Each heartbeat also stamps what the app was *doing*, because "killed after 47
minutes" is a fact and "killed after 47 minutes with the keepalive playing and
the socket open" is a diagnosis. Three fields, each answering a question that
would otherwise cost another day of waiting:

- **keepalive** — `playing`, `primed`, `unprimed`, `interrupted` or `off`. If it
  reads `unprimed` the mechanism never started, which is a different bug from
  it starting and not helping.
- **the verdict itself** — a goodbye is *not* proof the user closed it. KaiOS
  gives a content process notice before killing it, so `pagehide` fires on the
  way out; treating that as a clean exit filed system kills as "closed" and
  would have held the kill rate at zero on a phone that was killing the app.
  What separates them is whether anyone was looking: a person closes an app
  they can see, the system kills one they cannot.
- **socket** — `open` or `closed`. A live app whose socket quietly died
  delivers no messages either, and from the outside the two are identical: you
  simply stop getting notifications. This is what tells them apart.
- **msgs while hidden** — how many arrived since it left the screen.

The daemon has an independent view worth checking alongside this: it logs
`ws: phone connected` and `ws: phone disconnected after …`, so its log gives
the time of death without relying on the phone having survived to report it.

    grep 'ws: phone' <the daemon's output>

**The error on that disconnect line is the diagnosis.** `read: no route to
host` or `read: operation timed out` are not the app closing anything — they
are the *phone* becoming unreachable, which is KaiOS dropping Wi-Fi when the
screen goes off. The app is very likely still running; its socket is pointed at
a network the phone is no longer on. A genuine kill looks different: a clean
close, EOF, or going-away.

That distinction matters because the fixes have nothing in common. Against a
sleeping radio the app reconnects the instant anything changes in its favour —
`online`, or the app becoming visible — rather than waiting out a backoff that
is itself throttled while backgrounded. Exactly one dial is allowed in flight,
because a kick racing a pending retry produces two sockets, and the daemon
adopting the newer one logs a disconnect that looks just like the connection
dropping by itself.

The daemon also notices its own machine sleeping. Run on a laptop, a closed lid
freezes it, and from the phone that is indistinguishable from everything else
being broken — so a ticker that should fire every 30 seconds firing minutes
late is reported as what it is, with a pointer at [`wad/DEPLOY.md`](wad/DEPLOY.md).

`js/keepalive.js` is the mitigation: an inaudible tone made only while the app
is out of sight.

It is **synthesised, not a looped file**, and that distinction was earned. The
first version looped a one-second clip, and it was audible — not as a tone, but
as a click, once a second, 3600 times an hour. The tone itself is 80 dB below
anything hearable; the click was the element restarting, the decoder tearing
down and coming back while an amplifier that gates on silence popped each time
it woke. A continuously running oscillator has no loop point to click at, by
construction: the gain is ramped up when the app leaves the screen and down
when it returns, and nothing ever starts or stops. `audio/keepalive.wav`
(generated by `audio/mkkeepalive.py`) survives as the fallback for an engine
with no Web Audio, and is thirty seconds rather than one so that even then the
clicks are thirty times rarer. An app that is playing audio counts as perceptibly
doing something and moves up the priority list; that is how a music player
survives backgrounding.

**The channel is the whole question, and it is a trade in both directions.**
`content` is the media channel: certain to count, and exclusive — starting
playback on it stops whatever else is using it, so your music would pause every
time you left the app. `normal` is the default, non-exclusive channel: it
should not stop anything else, but the platform may not count it as the app
doing something for the user, in which case the keepalive buys nothing at all.

This ships on `normal`, because being useless is a better failure than being
antisocial. Which one is actually true on this handset is a measurement and not
something the code can know, so `KEEPALIVE_CHANNEL` in `js/config.js` switches
it, the Settings screen shows the channel the engine *accepted* (not the one
asked for) next to the kill rate, and the symptom of `normal` being too polite
is precise: the keepalive reports "playing" and the kill rate doesn't move.

If a higher-priority channel does take the speaker — a call, an alarm — the
element is interrupted, and the keepalive notes it and waits rather than
fighting to restart. That count is on the Settings screen too.

**Turning the volume down is not the answer, and turning it to zero is the one
thing that breaks it.** The platform decides an app is doing something for the
user from whether it is *audible*, and Gecko works that out from the stream —
so digital silence, a muted element and volume 0 are three ways to be counted
inaudible, which is exactly the state the priority manager disregards. Silent
and working and silent and pointless look identical from the outside.

So the two knobs are set in opposite directions on purpose: the file is 1% of
full scale (−40 dBFS decoded) so nothing analysing samples mistakes it for
silence, and `KEEPALIVE_VOLUME` is 0.01 on top, putting −80 dBFS at the output.
The volume is clamped above zero in code, because an edit to `config.js` that
set it to 0 would otherwise disable the mechanism while appearing to leave it
on.

**The frequency is chosen for headphones, which are the demanding case.** "A
phone speaker can't reproduce low frequencies" is true and irrelevant the
moment anything is plugged in — headphones reproduce 110 Hz perfectly well.
What protects the headphone case is the ear, not the driver. At −80 dBFS the
tone lands around 20 dB SPL on headphones at full volume, and the threshold of
hearing depends steeply on frequency:

| tone | threshold | margin |
|---|---|---|
| 110 Hz | ~27 dB SPL | **+7 dB** — thin |
| 63 Hz | ~38 dB SPL | +18 dB |
| 20 Hz | ~78 dB SPL | **+58 dB** |

So the loop is 20 Hz, at the bottom edge of hearing where the ear is some 50 dB
less sensitive than it is a couple of hundred hertz up. An exact number of
cycles fits a one-second loop, so the seam stays continuous.

0 Hz would be inaudible too, but it is a DC offset rather than a sound: the
output path blocks DC, so it may reach the hardware as literally nothing while
*also* being the kind of constant signal an audibility check dismisses — the
one failure that matters, silent and uncounted. It displaces the speaker cone
while it runs and pops when it stops. A tone below hearing gets the same
silence without either gamble.

**It is on by default, and that took three goes to settle.**

It does *not* save the app from another app. The daemon logged this:

    ws: read closed: websocket: close 1001 (going away): Child was killed
    ws: phone disconnected after 1m7s

immediately after YouTube was opened — and the same happened with the switch
on. `Child was killed` is the platform saying it outright. Shorts takes the
phone's own UI down too, and staying in *any* app long enough evicts everything
else. Against that the system is reclaiming whatever it can reach and an app's
priority is beside the point. Reopening afterwards is the correct response, not
a workaround.

But that is not the case this app lives in. It lives in a phone sitting idle
waiting for a message, and there it appears to help:

| build | tone | idle survival |
|---|---|---|
| ≤ 1.2.3 | on (audible — it was reported clicking) | hours; a message delivered 2 h in |
| 1.3.2 | off | 4 m 45 s |

Suggestive rather than conclusive: those are different sessions on different
days with different things running, not a controlled pair. What makes it worth
acting on is that it agrees with what the phone's owner observed directly, and
the cost of being wrong is some battery.

So it is on by default, with the switch on the Settings screen next to the kill
rate for anyone who would rather have the battery.

**Two unrelated things are called "keepalive" in these logs.** This one is
ours — the tone, `js/keepalive.js`, shown as "Stay alive in background". The
`[wa WARN] Keepalive timed out` lines in the daemon's output come from
whatsmeow and are about the daemon's own connection to WhatsApp's servers
timing out its ping. They have nothing to do with the phone, the app, or this
setting.

There is no `CONFIG` master switch any more, and its removal is the important
part. It used to be ANDed into the phone's preference, so a build shipping
`KEEPALIVE: false` left the Settings row reading "On" from the stored
preference while the keepalive was silently disabled — the same screen showed
"On" in the toggle and `keepalive: off` in the diagnostics two lines below. The
phone's switch is now the only control, and a test asserts it, because a
setting that lies about its own state is worse than no setting at all.

The three answers in order were "on, obviously", then "off, nothing was ever
observed being killed", then "off, it loses to YouTube", and now this. Only the
last one is based on testing the case that matters with the switch in both
positions. The middle one was also built on a recorder bug that was filing
kills as clean exits, so its evidence was worth less than it appeared.

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
