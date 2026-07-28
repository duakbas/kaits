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

Runs: pairing, receive text/media(as label), send text.
Stubbed: `/media/` download (returns 501), `getchats` (returns empty — the
app's list fills from live messages), image send, and all call *audio*
(call events ring the app but there's no media path yet).

## Env vars

| var | default | meaning |
|-----|---------|---------|
| `WAD_TOKEN` | `changeme` | shared secret; must match the app |
| `WAD_ADDR`  | `:8080`    | listen address |
| `WAD_DB`    | `wa-session.db` | session store path |
