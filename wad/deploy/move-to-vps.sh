#!/usr/bin/env bash
# move-to-vps.sh — move the daemon and its account to a server, in the one
# order that is safe.
#
# The dangerous parts of this move are not the parts that look dangerous:
#
#   wa-session.db IS the linked-device registration. Two daemons running
#   against one copy is not "a spare" — WhatsApp sees one device behaving like
#   two, and the usual outcome is the session being invalidated. So the local
#   daemon has to be stopped and STAY stopped.
#
#   SQLite is in WAL mode, so the database is not one file. A -wal sidecar can
#   hold committed transactions that are not yet in the main file. Copying only
#   wa-session.db from a daemon that did not exit cleanly gives you a database
#   that opens fine and is quietly missing your most recent messages. Every
#   wa-session.db* file comes along.
#
# Usage:
#   ./deploy/move-to-vps.sh you@vps.example.com [/opt/wad]

set -euo pipefail

REMOTE="${1:-}"
REMOTE_DIR="${2:-/opt/wad}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ -z "$REMOTE" ]; then
  echo "usage: $0 user@host [remote-dir]" >&2
  exit 2
fi

cd "$HERE"

# 1. Refuse to copy a live database.
if pgrep -f "[w]ad/cmd/wad|[/]wad$" >/dev/null 2>&1; then
  echo "A daemon looks like it is still running here." >&2
  echo "Stop it first — copying a live session database is how you get a" >&2
  echo "half-written one, and running two against one account is worse." >&2
  exit 1
fi

shopt -s nullglob
FILES=(wa-session.db*)
shopt -u nullglob
if [ ${#FILES[@]} -eq 0 ]; then
  echo "No wa-session.db here. Run this from the wad directory of a paired setup." >&2
  exit 1
fi

echo "==> files to move:"
for f in "${FILES[@]}"; do printf '    %-32s %s\n' "$f" "$(du -h "$f" | cut -f1)"; done
case " ${FILES[*]} " in
  *" wa-session.db-wal "*)
    echo
    echo "    NOTE: a -wal file is present, which means the last shutdown was not"
    echo "    clean or a checkpoint is pending. It is being copied too, which is"
    echo "    what keeps your most recent messages."
    ;;
esac

echo
echo "==> checksums before"
BEFORE="$(shasum -a 256 "${FILES[@]}" 2>/dev/null || sha256sum "${FILES[@]}")"
echo "$BEFORE" | sed 's/^/    /'

echo
echo "==> creating $REMOTE_DIR on $REMOTE"
ssh "$REMOTE" "sudo mkdir -p '$REMOTE_DIR' && sudo chown \$(id -u):\$(id -g) '$REMOTE_DIR'"

echo "==> copying"
scp "${FILES[@]}" "$REMOTE:$REMOTE_DIR/"

echo
echo "==> checksums after, on the server"
AFTER="$(ssh "$REMOTE" "cd '$REMOTE_DIR' && (sha256sum ${FILES[*]} 2>/dev/null || shasum -a 256 ${FILES[*]})")"
echo "$AFTER" | sed 's/^/    /'

# Compare hashes only — the two tools format their output differently.
norm() { awk '{print $1}' | sort; }
if [ "$(printf '%s\n' "$BEFORE" | norm)" != "$(printf '%s\n' "$AFTER" | norm)" ]; then
  echo >&2
  echo "MISMATCH. The copy is not identical — do NOT start the remote daemon." >&2
  echo "Re-run the copy; a truncated session database will fail to pair and" >&2
  echo "may take the local one with it." >&2
  exit 1
fi

echo
echo "==> verified identical."
cat <<EOF

Next, on the server:

  1. Build it (needs Go and gcc — the SQLite driver uses CGO):
       git clone <your repo> && cd <repo>/wad && go build -o wad ./cmd/wad
       sudo mv wad $REMOTE_DIR/

  2. Set a real token. It is currently whatever you used locally, and
     "changeme" is not a token:
       openssl rand -hex 16

  3. Install the service and TLS config from wad/deploy/, then:
       sudo systemctl enable --now wad

  4. Point the phone at wss://your-host/ws with the same token, on the
     app's Settings screen.

Do NOT start the local daemon again. One account, one daemon.
EOF
