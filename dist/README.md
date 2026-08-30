# dist

The built app, so it can be fetched from GitHub instead of passed around.

**Current: 1.6.0 (privileged)** — [`kaits.zip`](kaits.zip)

Download it raw:

    https://github.com/duakbas/kaits/raw/main/dist/kaits.zip

The path never changes, so this URL is always the current build. Which version
you actually have is in `manifest.webapp` inside the zip:

    unzip -p kaits.zip manifest.webapp | grep version

## Installing it

WebIDE → Install Packaged App → pick the unzipped folder, or push the zip with
the KaiOS debugging tools. A packaged app must be a zip with `manifest.webapp`
at the root — that is the format, not a wrapper around it, which is why
`kaits/package.sh` stages the files and zips their contents rather than zipping
the directory.

A privileged build cannot be installed over the air from a web page; that path
is store-only. This file is here to be downloaded and sideloaded.

## Rebuilding it yourself

    ./kaits/package.sh 1.5.1 privileged     # -> kaits/build/kaits.zip
    cp kaits/build/kaits.zip dist/kaits.zip

Bump the version every time. The KaiStore portal rejects a version it has
already seen, and on the phone an unchanged version can install over itself
without actually replacing anything.

## 1.6.0

Videos play in the full-screen viewer — Enter plays and pauses, Left and Right
seek. Stickers display at all now (the daemon converts them; this browser has
no WebP decoder). A sticker picker on the attach menu, offering the stickers
you have been sent.

Left on the D-pad moves the caret again instead of opening the attach menu;
attach is the left softkey.

Needs the matching daemon — the sticker and live-location work is on that side.

## 1.5.0

Bubbles only — the black rounded plate behind the icon is gone, and the shapes
are scaled up to fill the margin it was using.

Fixes the Settings screen rewriting a stored address. It prefilled the field
with the scheme stripped, and saving re-derived `ws://host:8080/ws` from that —
so opening Settings on a working `wss://` server and pressing save silently
downgraded it to plaintext on a port that is deliberately closed, and the app
went offline with nothing to say why.
