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

That merges duplicate `@lid` chats into their phone JID, then re-resolves chat
and sender names that are still bare numbers, and exits. It's safe to re-run —
each pass only touches rows that are still unresolved.

## Env vars

| var | default | meaning |
|-----|---------|---------|
| `WAD_TOKEN` | `changeme` | shared secret; must match the app |
| `WAD_ADDR`  | `:8080`    | listen address |
| `WAD_DB`    | `wa-session.db` | session store path |
| `WAD_MIGRATE_LIDS` | unset | `1` = run the one-shot LID/name repair, then exit |
