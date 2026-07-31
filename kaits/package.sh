#!/usr/bin/env bash
# package.sh — build the KaiStore-submittable zip for the app.
#
# A packaged app is a zip with manifest.webapp AT THE ROOT, not inside a folder.
# Zipping the directory itself produces kaits/manifest.webapp and the portal
# rejects it, so this stages the files and zips their contents.
#
# The portal also rejects a build whose version it has already seen, so a
# re-upload needs a new one.
#
#   ./kaits/package.sh            -> kaits/build/kaits.zip  (version 0.0.1)
#   ./kaits/package.sh 0.0.2      for a re-upload
#
# NOTE: js/config.js is baked into the package, and it contains WAD_TOKEN. The
# zip is a credential. Don't pass it around.

set -euo pipefail

VERSION="${1:-0.0.1}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD="$HERE/build"
STAGE="$BUILD/app"
OUT="$BUILD/kaits.zip"

# config.js is only a fallback now — the app asks for the daemon address on
# first run and stores it on the phone, so the packaged default rarely matters.
# Still worth saying out loud, because a localhost default means the setup
# screen is the ONLY way the app will ever connect.
DAEMON="$(sed -n 's/.*DAEMON_WS:[[:space:]]*"\([^"]*\)".*/\1/p' "$HERE/js/config.js" | head -1)"
case "$DAEMON" in
  *localhost*|*127.0.0.1*)
    echo "note: config.js still defaults to $DAEMON"
    echo "      that's fine — the app asks for the address on first run."
    ;;
esac

rm -rf "$BUILD"
mkdir -p "$STAGE"

cp -R "$HERE/index.html" "$HERE/sw.js" "$HERE/css" "$HERE/js" "$HERE/icons" "$STAGE/"

# Version is the only field that changes per build, so patch it rather than
# keeping a second copy of the manifest that can drift from the real one.
sed 's|"version": "[^"]*"|"version": "'"$VERSION"'"|' "$HERE/manifest.webapp" > "$STAGE/manifest.webapp"
python3 -c "import json,sys; d=json.load(open('$STAGE/manifest.webapp')); assert d['version']=='$VERSION', d['version']; print('manifest ok:', d['name'], d['version'])"

( cd "$STAGE" && zip -q -r "$OUT" . -x ".*" )

echo "built $OUT"
NAMES="$(unzip -Z1 "$OUT" 2>/dev/null || unzip -l "$OUT" | awk 'NF>=4 {print $NF}')"
if printf '%s\n' "$NAMES" | tr -d '\r' | grep -qx 'manifest.webapp'; then
  echo "OK: manifest.webapp is at the zip root."
else
  echo "PROBLEM: manifest.webapp is not at the zip root; the portal will reject this."
  exit 1
fi
echo "default daemon in config.js: ${DAEMON:-unset} (overridden by on-phone setup)"
echo
echo "The token is entered on the phone now, so the zip carries no secret."
