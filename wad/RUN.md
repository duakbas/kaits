# wad — WhatsApp daemon (localhost run guide)

The server half. Pairs as a WhatsApp linked device, speaks the small JSON
protocol to the app over a WebSocket. This guide is just "get it running on
your Mac against the app." Full architecture notes are in the code comments.

## Prereqs (macOS, one time)

```
brew install go
xcode-select --install     # C compiler — the sqlite session store uses CGO
```

## Build

From inside this `wad/` folder:

```
./setup.sh
```

That adds whatsmeow at its real latest version, resolves deps, writes
`go.sum`, and builds. **Do not run `go build` before `setup.sh`** — the repo
ships without pinned whatsmeow/go.sum on purpose, so a cold build fails until
tidy runs. That's expected, not a bug.

If setup.sh stops complaining about `cc`, run `xcode-select --install` and
re-run it.

## Run

```
./run.sh
```

or manually:

```
WAD_TOKEN=changeme go run ./cmd/wad
```

- Listens on `:8080`. The app connects to `ws://localhost:8080/ws`.
- `WAD_TOKEN` must match `TOKEN` in the app's `js/config.js`. Both default to
  `changeme`, so out of the box they already agree.
- First run prints a **QR code in the terminal**. Open WhatsApp on your phone
  › Settings › Linked devices › Link a device, and scan it.
- The session is saved to `wa-session.db` in the current folder. Keep it
  private — it *is* your logged-in session. Delete it to re-pair from scratch.

## Confirm it works

1. `./run.sh`, scan the QR. Terminal should log `wa: pairing success` then a
   Connected event.
2. Send yourself a WhatsApp message from another chat. You should see a
   `message` frame logged.
3. Now start the app (`python3 -m http.server 8000` in the `wa-kaios/` folder,
   open localhost:8000). Its header dot goes green and messages show up.

## What runs vs. not

Runs: pairing, persistent history, chat list, send/receive text, receive
photo/video/gif/sticker/voice, reply, delete, forward, avatars, pinned chats.
Not yet: sending media, recording voice notes, and all call *audio* (call
events ring the app, but there's no media path yet).

## Contact names and LIDs

Modern WhatsApp addresses people by LID (`<id>@lid`) rather than by phone
number, and the same person gets a *different LID in every group*. Names are
resolved by reading whatsmeow's own `whatsmeow_lid_map` and `whatsmeow_contacts`
tables directly (read-only, alongside the store API), because:

- the lid map table holds every mapping, while `GetPNForLID` only answers for
  the ones the running session has activated; and
- a name you saved in your address book syncs keyed by the *phone* JID, while
  the message needing that name arrives keyed by a LID — so both the JID and
  its phone↔LID counterpart have to be checked.

On startup the daemon logs `direct LID resolver ready (N lid mappings, M
contacts)`. If it instead logs `direct LID resolver disabled`, whatsmeow's
schema has moved; name resolution falls back to the older, patchier path and
some senders will show as raw numbers until `internal/wa/lidmap.go` is updated.

Messages already stored under an unresolved LID aren't rewritten automatically.
To repair them in place, run once:

```
WAD_MIGRATE_LIDS=1 go run ./cmd/wad
```

That merges duplicate `@lid` chats into their phone JID, then re-resolves chat,
sender and quoted-reply names that are still bare numbers, and exits. It's safe
to re-run — each pass only touches rows that are still unresolved.

If names are *still* raw numbers — or a contact shows their own WhatsApp name in
groups while the DM shows the name you saved — the mapping genuinely isn't in
whatsmeow's tables. Reading the tables can't fix a row that was never written.
Fetch the missing data:

```
WAD_RESYNC=1 go run ./cmd/wad
```

That does three things before the repair passes:

1. asks WhatsApp to re-send the whole contact list (app-state full sync);
2. **looks up the LID of every contact that doesn't have one** and stores the
   pairs — this is the step that fixes the "different name in every group"
   symptom, because those contacts had a saved name on their phone row and no
   row linking it to the LID their group messages arrive under;
3. drops the resolver's caches so nothing stale survives.

Step 2 is one server round-trip per batch of 50 contacts, paced to stay well
under any rate limit, so it takes a little while on a large address book. The
log reports how many contacts were asked about and how many new mappings landed.

All of this happens **without unlinking**. Re-pairing is not the fix: your
messages live in our own `.history.db`, which a fresh pairing doesn't
repopulate — WhatsApp caps how much history a linked device receives, so you'd
end up with less, not more.

### Checking directly

The tables are plain SQLite, so you can look:

```bash
# what WhatsApp knows this person by
sqlite3 wa-session.db "SELECT their_jid, full_name, push_name FROM whatsmeow_contacts WHERE full_name LIKE '%name%';"
# is their LID linked to their number?
sqlite3 wa-session.db "SELECT * FROM whatsmeow_lid_map WHERE pn='<their number>';"
# how many pairs are known at all
sqlite3 wa-session.db "SELECT COUNT(*) FROM whatsmeow_lid_map;"
```

A contact with a `full_name` on their `@s.whatsapp.net` row but no `lid_map`
row is exactly the case step 2 above repairs.

## Chat actions write to the account

Pin, mute, archive and delete go out as **app-state patches** (`SendAppState`),
the same mechanism the official clients use. They are not local preferences:

- muting here mutes on your phone and every other linked device;
- **delete is a real WhatsApp chat delete and cannot be undone from here** (the
  app asks for confirmation first);
- archiving unpins, because WhatsApp does that server-side.

Our own db is updated only *after* WhatsApp accepts the patch, so a rejected
write can't leave the app showing state the account doesn't have — the daemon
replies with an `error` frame plus a fresh `chatlist` to resync.

## Saving contacts

There are two separate things, because WhatsApp has no contact-write API — the
address book syncs one way, from a phone *into* the account:

1. **In-app nickname** — stored in our own `local_contacts` table (not
   whatsmeow's, which its next contact sync would wipe). It wins over every
   other name and is applied retroactively to stored chats and messages.
2. **Phone address book** — on KaiOS only, the app writes the number via
   `navigator.mozContacts`, falling back to a `new` / `webcontacts/contact`
   web activity that opens the Contacts app prefilled. The manifest declares
   the `contacts` permission for this. Neither API exists in a desktop browser,
   so that action is hidden during development and only the nickname applies.

## Old photos that won't load

An attachment can only be decrypted with keys that ride on the original message
(direct path, media key, two hashes). Those are stored per message now, so media
received from here on stays viewable across restarts. Messages stored *before*
that have no keys, and no repair pass can invent them — their photos are
un-fetchable from what's on disk.

They can be asked for again. WhatsApp lets a linked device request history on
demand, and the reply carries full message data, so the keys get captured as a
side effect:

```
WAD_REFETCH_MEDIA=1 go run ./cmd/wad
```

It reports how many attachments lack keys, asks the phone for the history around
them, waits ~90s for replies, then reports how many keys it gained.
`WAD_REFETCH_MAX=n` bounds the number of requests (default 40).

This depends on the primary phone cooperating: **it has to be online**, and it
may simply decline to serve history that old. If the count doesn't move, that's
what happened — re-running later is harmless. Requests are paced, because this
is exactly the kind of chatter that gets an unofficial client throttled.

## "Last active 2 days ago" on the phone

A linked device that just connects is passive: it receives everything, but the
server never marks it active. The phone's "Linked devices" screen keeps showing
a stale *last active*, and other users see `-` where your push name should be.

Presence is what fixes that, and the daemon now sends it on every connect. The
log line to look for:

```
wa: presence sent as "available"
```

`available` also means you appear **online to your contacts** while the daemon
runs, and read receipts get sent. If that's not wanted:

| `WAD_PRESENCE` | effect |
|---|---|
| unset / `available` | appear online, send read receipts, device shows active |
| `unavailable` | register the push name but stay invisible |
| `off` | send nothing; the device will keep looking idle |

Note that this only lasts while the daemon is connected. One-shot modes
(`WAD_MIGRATE_LIDS`, `WAD_REFETCH_MEDIA`) connect, work, and exit, so they won't
keep the device looking live.

## What happens while the daemon is off

The daemon only receives messages while it's running. WhatsApp buffers for an
offline linked device and replays on reconnect, so short outages — closing the
laptop, restarting the daemon — cost nothing.

That buffer expires, though. Past it, messages are never delivered to this
device at all, and the hole is invisible: the next message that arrives stores
normally, so nothing looks wrong until you scroll.

On-demand history only reads *backwards* from an anchor, so a gap can't be
filled until something newer than it exists. The daemon uses that: when a live
message lands more than 6 hours after the last one stored for its chat, it asks
the phone for the messages in between. Once per chat per run, so a chat that has
simply been quiet doesn't generate requests. You'll see it in the log:

```
wa: <chat> has a 3d gap since the last stored message; requesting the missing history
```

This is best-effort for the same reasons as the media refetch — it needs the
phone online and willing. It also can't help with an outage that is still
ongoing: nothing is missing until something newer arrives to reveal it.

## Env vars

| var | default | meaning |
|-----|---------|---------|
| `WAD_TOKEN` | `changeme` | shared secret; must match the app |
| `WAD_ADDR`  | `:8080`    | listen address |
| `WAD_DB`    | `wa-session.db` | session store path |
| `WAD_MIGRATE_LIDS` | unset | `1` = run the one-shot LID/name repair, then exit |
| `WAD_RESYNC` | unset | `1` = full contact resync, then the repair, then exit |
| `WAD_REFETCH_MEDIA` | unset | `1` = ask the phone to re-send history so old attachments become downloadable |
| `WAD_REFETCH_MAX` | `40` | max history requests one refetch run may send |
| `WAD_PRESENCE` | `available` | `unavailable` = invisible, `off` = send no presence |
| `WAD_INCLUDE_STATUS` | unset | `1` = keep status/"Updates" posts instead of ignoring them |

## Push notifications (daemon off-device)

The phone can't hold a socket open while the app is closed — on KaiOS there is
one always-on connection on the whole device, the OS's push channel, shared by
every app. So the daemon plays the part every other app's backend plays: it
stays connected to WhatsApp, and when a message arrives it POSTs to the phone's
push endpoint to wake the app's service worker.

**This only works with the daemon somewhere always-on** — a VPS, a Raspberry
Pi, a machine that doesn't sleep. A daemon that isn't running received nothing,
so there is nothing to wake for. (Running the daemon *on* the phone is a
different architecture that doesn't need push at all: it would hold the
connection itself and notify directly.)

The push carries **no payload**. KaiOS allows subscribing without an
`applicationServerKey`, and without VAPID only empty pushes are permitted —
which suits this design, because the app re-syncs from the daemon on connect
anyway. The push is a doorbell, not a delivery: message content never passes
through Mozilla's or KaiOS's push infrastructure, and there are no keys to
manage.

Flow:

1. the app registers `sw.js` and subscribes, getting an endpoint back;
2. it hands the endpoint to the daemon (`pushsub`), stored in `push_subs`;
3. a message arrives while no app is connected → the daemon POSTs to it;
4. the OS wakes `sw.js`, which fetches `/notify-summary` for the wording and
   shows a notification;
5. tapping it focuses or launches the app, which connects and syncs.

### Testing notifications without a phone

**Use Firefox, not Chrome.** Chrome's push goes through FCM and *requires* an
`applicationServerKey`; our subscribe call deliberately omits one, so Chrome
throws instead of returning an endpoint and you never get past step 1. Firefox
uses Mozilla autopush — the same lineage KaiOS's push service descends from —
and allows keyless subscription exactly as KaiOS does, so the whole loop works
there: subscribe → endpoint → daemon POSTs → worker wakes → notification.

For the notification rendering itself, there's a shortcut that needs no push at
all and works in any browser. With the app open, in the console:

```js
App.testNotify()
```

That runs the real service-worker path against live unread state, so grouping,
tag replacement and click-to-open can all be checked without waiting for someone
to message you.

What still can't be tested off-device: whether KaiOS's notification tray honours
`tag` replacement and `renotify` buzzing, and whether the OS wakes a genuinely
closed app.

### One notification per conversation

The phone shows a bubble per chat, tagged with the chat's JID. A second message
in the same conversation **replaces** that bubble rather than stacking a new
one, and `renotify` makes the replacement buzz — so an updating notification
still announces itself. A different conversation gets its own bubble.

The body is the chat's own preview while there's one unread ("Alex: on my way"),
and switches to a count once there's more than one. So a chat collapses to a
single line only after it becomes a conversation.

Wake-ups are coalesced **per chat**, not globally: every chat that gets a
message wakes the phone immediately, and only repeats within the same chat
inside 3 seconds are folded together (`pushCoalesce` in `internal/wa/push.go`).
Nothing is lost when they are — the chat's bubble shows the higher count either
way; you just get one buzz instead of two for messages that arrived nearly
together.

Tapping a notification opens that conversation, whether the app was already
running (the worker messages it) or had to be launched (the JID rides in the URL
fragment).

Muted chats never wake the phone, and neither do your own messages. Bubbles for
chats that are no longer unread — read on another device — are closed on the
next wake. Endpoints that return 404/410 are dropped, since that means the
subscription is dead.

**The endpoints are capability URLs**: anyone holding one can wake the device.
They live in the gitignored history db, and only a shortened form is logged.

`/notify-summary` is token-guarded like the socket — it reports who is messaging
you, which isn't public.
