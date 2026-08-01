#!/usr/bin/env bash
# mkbundle.sh — build the zip you copy to the server and unpack.
#
#   ./wad/deploy/mkbundle.sh                 # includes the live session
#   ./wad/deploy/mkbundle.sh --no-session    # code only, pair with a QR there
#   ./wad/deploy/mkbundle.sh -o ~/wad.zip
#
# Then, on the server:
#
#   unzip wad-setup-*.zip && cd wad-setup && sudo ./install.sh your.hostname
#
# ─── read this before you send the file anywhere ─────────────────────────────
# The zip contains wa-session.db, and that file IS the linked-device
# registration for your WhatsApp account. Anyone who unpacks it can read and
# send your messages. It is not a backup and not a credential you can rotate.
# Move it over scp, not over anything that keeps a copy, and delete it from
# both ends once the server is up.
# ─────────────────────────────────────────────────────────────────────────────

set -euo pipefail

WAD="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPLOY="$WAD/deploy"
FROM="$PWD"
OUT=""
WITH_SESSION=1

die() { echo "ERROR: $*" >&2; exit 1; }
say() { echo; echo "==> $*"; }

BIN=""

while [ $# -gt 0 ]; do
  case "$1" in
    --no-session) WITH_SESSION=0; shift ;;
    --binary) BIN="${2:-}"; [ -n "$BIN" ] || die "--binary needs a path"; shift 2 ;;
    -o) OUT="${2:-}"; [ -n "$OUT" ] || die "-o needs a path"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

# macOS ships shasum, Linux sha256sum. The output format is identical, which is
# what lets `sha256sum -c` on the server check a file written on a Mac.
if command -v sha256sum >/dev/null 2>&1; then sha() { sha256sum "$@"; }
elif command -v shasum >/dev/null 2>&1; then sha() { shasum -a 256 "$@"; }
else die "no sha256sum or shasum — cannot checksum the session"; fi
command -v zip >/dev/null 2>&1 || die "no zip installed"

STAMP="$(date +%Y%m%d-%H%M)"
[ -n "$OUT" ] || OUT="$FROM/wad-setup-$STAMP.zip"
case "$OUT" in /*) ;; *) OUT="$FROM/$OUT" ;; esac

cd "$WAD"

# ---- 1. the session ---------------------------------------------------------
# Done first, and fatally, because everything else is reproducible and this
# isn't. A bundle that quietly turned out to have no account in it would only
# be discovered on the server, after the local daemon had been stopped.
shopt -s nullglob
SESSION=(wa-session.db*)
shopt -u nullglob

if [ "$WITH_SESSION" -eq 1 ]; then
  if [ ${#SESSION[@]} -eq 0 ]; then
    die "no wa-session.db in $WAD.
    Run this from a paired setup, or pass --no-session to build a
    code-only bundle and pair on the server with a QR."
  fi
  # Copying a database out from under a running daemon gives you a file that
  # opens perfectly and is missing the end of itself.
  if pgrep -f "[w]ad/cmd/wad|[/]wad$" >/dev/null 2>&1; then
    die "a daemon looks like it is still running — stop it first"
  fi
fi

# ---- 2. staging -------------------------------------------------------------
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
STAGE="$TMP/wad-setup"
mkdir -p "$STAGE"

say "setup files"
install -m 755 "$DEPLOY/install.sh" "$STAGE/install.sh"
install -m 644 "$DEPLOY/wad.service" "$STAGE/wad.service"
install -m 644 "$DEPLOY/Caddyfile"   "$STAGE/Caddyfile"
echo "    install.sh, wad.service, Caddyfile"

# ---- 3. source --------------------------------------------------------------
# The working tree rather than a git archive, deliberately: what goes to the
# server should be the code you have been running, including whatever you have
# not committed yet.
say "source"
SRC="$TMP/src"
mkdir -p "$SRC"
tar -cf - go.mod go.sum cmd internal | tar -C "$SRC" -xf -

# Vendoring means the server never has to reach proxy.golang.org, and it builds
# the exact dependency versions this machine did rather than whatever resolves
# on the day. Costs about 6 MB in the zip.
VENDORED=no
if command -v go >/dev/null 2>&1; then
  if ( cd "$SRC" && go mod vendor >/dev/null 2>&1 ); then
    VENDORED=yes
    echo "    dependencies vendored ($(du -sh "$SRC/vendor" | cut -f1))"
  else
    echo "    WARNING: go mod vendor failed; the server will fetch modules itself"
  fi
else
  echo "    no local Go, so no vendoring — the server will fetch modules itself"
fi
# COPYFILE_DISABLE stops macOS tar from sprinkling ._ files through the archive.
COPYFILE_DISABLE=1 tar -C "$SRC" -czf "$STAGE/src.tar.gz" .
echo "    src.tar.gz ($(du -h "$STAGE/src.tar.gz" | cut -f1))"

# A binary built here, when this machine can produce one the server will run.
# It saves the server the Go toolchain, gcc and the build cache — the better
# part of a gigabyte, which on a box that is nearly full is the difference
# between this working and not. The source still ships either way, so the
# server can fall back to building if the binary turns out to be wrong.
#
# CGO rules out cross-compiling: the SQLite driver needs a C toolchain for the
# target, so a Mac cannot produce a Linux binary without one. Hence "when the
# builder is already Linux, on the same architecture", and nothing cleverer.
if [ -n "$BIN" ]; then
  [ -f "$BIN" ] || die "--binary $BIN does not exist"
  case "$(file -b "$BIN" 2>/dev/null)" in
    *x86-64*)  BARCH=amd64 ;;
    *aarch64*) BARCH=arm64 ;;
    *) BARCH=amd64; echo "    could not identify the architecture, assuming amd64" ;;
  esac
  install -m 755 "$BIN" "$STAGE/wad-linux-$BARCH"
  echo "    wad-linux-$BARCH ($(du -h "$BIN" | cut -f1)) from $BIN"
elif command -v go >/dev/null 2>&1 && [ "$(go env GOOS)" = linux ]; then
  BARCH="$(go env GOARCH)"
  if ( cd "$SRC" && go build -o "$STAGE/wad-linux-$BARCH" ./cmd/wad 2>/dev/null ); then
    chmod 755 "$STAGE/wad-linux-$BARCH"
    echo "    wad-linux-$BARCH ($(du -h "$STAGE/wad-linux-$BARCH" | cut -f1)) built here"
    echo "    NOTE: this only runs on a server whose glibc is not older than this"
    echo "    machine's. install.sh checks, and builds from source if it isn't."
  else
    rm -f "$STAGE/wad-linux-$BARCH"
    echo "    could not build here; the server will build from src.tar.gz"
  fi
else
  echo "    not building a binary here (needs Linux); the server will build"
fi

# ---- 4. session -------------------------------------------------------------
if [ "$WITH_SESSION" -eq 1 ]; then
  say "session"
  mkdir -p "$STAGE/session"
  for f in "${SESSION[@]}"; do
    printf '    %-28s %s\n' "$f" "$(du -h "$f" | cut -f1)"
  done
  case " ${SESSION[*]} " in
    *" wa-session.db-wal "*)
      echo
      echo "    NOTE: a -wal sidecar is present, so the last shutdown was not clean"
      echo "    or a checkpoint is pending. It is going in too — that file is where"
      echo "    your most recent messages are." ;;
  esac
  cp "${SESSION[@]}" "$STAGE/session/"
  BEFORE="$(sha "${SESSION[@]}" | awk '{print $1}' | sort)"
  ( cd "$STAGE/session" && sha "${SESSION[@]}" > SHA256SUMS )
  AFTER="$(awk '{print $1}' "$STAGE/session/SHA256SUMS" | sort)"
  [ "$BEFORE" = "$AFTER" ] || die "the copy into the bundle does not match the original"
  echo "    copied and verified"
else
  say "session"
  echo "    skipped (--no-session) — the server will print a QR on first run"
fi

# ---- 5. provenance ----------------------------------------------------------
{
  echo "built:  $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
  echo "host:   $(hostname 2>/dev/null || echo unknown)"
  if git -C "$WAD" rev-parse --git-dir >/dev/null 2>&1; then
    echo "commit: $(git -C "$WAD" rev-parse --short HEAD)$(git -C "$WAD" diff --quiet 2>/dev/null || echo ' (+ uncommitted changes)')"
    echo "branch: $(git -C "$WAD" rev-parse --abbrev-ref HEAD)"
  fi
  echo "vendored: $VENDORED"
  echo "session:  $([ "$WITH_SESSION" -eq 1 ] && echo "${#SESSION[@]} file(s)" || echo none)"
} > "$STAGE/VERSION.txt"

# ---- 6. README --------------------------------------------------------------
cat > "$STAGE/README.txt" <<'EOF'
wad — server setup bundle
=========================

On the server, as a user with sudo:

    sudo ./install.sh your.hostname

That is the whole thing. It installs Go and gcc, builds the daemon, creates a
`wad` user, puts your existing WhatsApp session in place, installs and starts a
systemd service, installs Caddy and gets a TLS certificate for the hostname,
and opens 80 and 443 in the firewall. It is safe to re-run.

When it finishes it prints the two lines to type into the phone:

    Server address   wss://your.hostname/ws
    Token            (generated during install)

On the phone that is: chat list -> scroll to the bottom -> Settings.


If 80 and 443 already belong to something else
----------------------------------------------

Put wad on its own port and leave the existing site completely alone:

    sudo ./install.sh your.hostname --port 8080 \
         --cert /etc/letsencrypt/live/your.hostname/fullchain.pem \
         --key  /etc/letsencrypt/live/your.hostname/privkey.pem

The certificate has to be one that already exists on the box, because nothing
can obtain a new one here: the ACME challenge is answered on port 80 or 443,
which is exactly what is occupied. Find the one the existing site uses with

    ls /etc/letsencrypt/live/ ; grep -rn ssl_certificate /etc/nginx/

`--no-tls` skips all of that and serves plain ws://, which also means the token
crosses the network in readable text on every connect. On a box with a public
hostname that is worth avoiding.

install.sh will not overwrite a Caddy config that was already serving
something. If it finds one, it writes its own to /etc/caddy/wad.caddy and tells
you the one line to add.


Before you run it
-----------------

* The hostname must be a real DNS name already pointing at this server's public
  IP. Let's Encrypt verifies it over port 80, so that has to be reachable from
  the internet. The script warns if the name resolves somewhere else, and Caddy
  keeps retrying, so fixing DNS afterwards also works.

* Debian or Ubuntu, with systemd. It uses apt.


THIS BUNDLE CONTAINS YOUR WHATSAPP ACCOUNT
------------------------------------------

session/wa-session.db is not a backup of your account — it IS the linked-device
registration. Anyone who unpacks this zip can read and send your messages, and
there is no password to change afterwards; the only fix is unlinking the device
from your phone.

So: move it with scp, not through anything that keeps a copy of what passes
through it, and delete the zip from both machines once the server is running.

And do not start the old daemon on your laptop again. One account, one daemon.
Two daemons sharing one session file look to WhatsApp like a single device
behaving like two, and the usual outcome is being logged out.


What is in here
---------------

    install.sh      the whole setup, described above
    src.tar.gz      the daemon's source; install.sh builds it on the server
    wad.service     the systemd unit it installs
    Caddyfile       the reverse-proxy config, with the hostname substituted in
    session/        your session and message databases, plus SHA256SUMS
    VERSION.txt     what this was built from, and when


Afterwards
----------

    sudo systemctl status wad
    sudo journalctl -u wad -f          # the QR renders here if it ever unlinks
    sudo journalctl -u caddy -n 30     # if TLS is not coming up

The daemon binds to 127.0.0.1:8080 on purpose and 8080 is deliberately left
closed in the firewall: Caddy terminates TLS in front of it. The token is the
only thing between the internet and the account, so it must not cross the
network in the clear.

Back up /opt/wad/wa-session.db* on some schedule. They are the account and every
message, and nothing else on that box is irreplaceable.
EOF

# ---- 7. zip -----------------------------------------------------------------
say "packing"
rm -f "$OUT"
( cd "$TMP" && zip -q -r -X "$OUT" wad-setup )
# The account is in there. Do not leave it group- or world-readable, especially
# on a shared machine or a synced folder.
chmod 600 "$OUT"

echo "    $OUT ($(du -h "$OUT" | cut -f1))"
echo
echo "────────────────────────────────────────────────────────"
echo "Next:"
echo "    scp '$OUT' you@your.server:"
echo "    ssh you@your.server"
echo "    unzip $(basename "$OUT") && cd wad-setup && sudo ./install.sh your.hostname"
if [ "$WITH_SESSION" -eq 1 ]; then
  echo
  echo "This zip contains your WhatsApp account. Do not put it anywhere that"
  echo "keeps copies, and delete it from both machines when the server is up."
fi
echo "────────────────────────────────────────────────────────"
