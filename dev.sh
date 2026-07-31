#!/usr/bin/env bash
# dev.sh — run both halves for local development.
#
# The app is a static site and the daemon is a separate process, so getting the
# thing running normally means two terminals and remembering two commands. This
# starts both, streams the daemon's output (which is where the QR code and every
# useful log line appears), and shuts the static server down again on Ctrl-C
# rather than leaving it orphaned on port 8000.
#
#   ./dev.sh              start both
#   ./dev.sh --pull       update from git first
#
# Environment variables are passed through to the daemon, so the one-shot modes
# still work: WAD_MIGRATE_LIDS=1 ./dev.sh, and so on. See wad/RUN.md.

set -euo pipefail

APP_PORT="${APP_PORT:-8000}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

if [ "${1:-}" = "--pull" ]; then
  echo "==> updating from git"
  git pull --rebase origin "$(git rev-parse --abbrev-ref HEAD)"
fi

# A stale server on the port would silently serve OLD code from another
# checkout, which is a genuinely confusing way to lose an afternoon.
if command -v lsof >/dev/null 2>&1 && lsof -i ":$APP_PORT" >/dev/null 2>&1; then
  echo "!!  Something is already listening on port $APP_PORT."
  echo "    That's probably a leftover server, and it will serve stale code."
  echo
  echo "    Free it:   lsof -ti:$APP_PORT | xargs kill"
  echo "    Or move:   APP_PORT=8001 ./dev.sh"
  exit 1
fi

APP_PID=""
cleanup() {
  [ -n "$APP_PID" ] || return 0
  kill "$APP_PID" 2>/dev/null || true
  # Give it a moment, then insist. A server that outlives us holds the port and
  # serves stale code to the next run.
  for _ in 1 2 3 4 5; do
    kill -0 "$APP_PID" 2>/dev/null || return 0
    sleep 0.2
  done
  kill -9 "$APP_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "==> serving the app on http://localhost:$APP_PORT"
# exec inside the subshell matters: without it $! is the SUBSHELL's pid, and
# killing that leaves python reparented and still holding the port. exec
# replaces the subshell with python itself, so $! is the process we need.
( cd kaits && exec python3 -m http.server "$APP_PORT" >/dev/null 2>&1 ) &
APP_PID=$!

echo "==> starting the daemon (Ctrl-C stops both)"
echo
echo "    Open http://localhost:$APP_PORT in FIREFOX — Chrome refuses to"
echo "    subscribe for push without an applicationServerKey, which KaiOS"
echo "    doesn't use. Hard-reload with the cache disabled."
echo
# NOT exec: that replaces this shell and takes the EXIT trap with it, which
# would leave the static server running after Ctrl-C — the exact orphan the
# port check above then complains about on the next run.
cd wad
./run.sh
