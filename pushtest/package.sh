#!/usr/bin/env bash
# package.sh — build the KaiStore-submittable zip for the push test.
#
# A packaged app is a zip with manifest.webapp AT THE ROOT — not inside a
# folder. Zipping the directory itself produces pushtest/manifest.webapp and the
# submission portal rejects it, which is the single most common way to waste an
# upload. This builds a staging dir and zips its *contents*.
#
# The hosted layout uses /pushtest/... paths because it's served from the repo
# root; a packaged app is its own root, so the manifest is rewritten here rather
# than kept in two places that drift.
#
# The catcher address is substituted at BUILD time rather than edited into
# index.html, so the source keeps its placeholder and there is no "did I
# remember to change it" step to forget. Forgetting is silent and costs you a
# whole upload-install-open cycle to discover, so this refuses to build without
# one.
#
# The submission portal rejects a build whose version it has already seen, so
# every re-upload needs a new one. Second argument, defaulting to 1.0.0.
#
#   ./pushtest/package.sh 192.168.1.200            -> pushtest/build/pushtest.zip
#   ./pushtest/package.sh 192.168.1.200 1.0.1      (bumped, for a re-upload)
#   ./pushtest/package.sh http://host:9999         (full URL also fine)

set -euo pipefail

CATCHER="${1:-}"
VERSION="${2:-1.0.0}"
if [ -z "$CATCHER" ]; then
  echo "usage: $0 <catcher-ip-or-url>"
  echo
  echo "The phone posts its push endpoint there. Run 'python3 pushtest/catch.py"
  echo "serve' first — it prints the address to use."
  exit 1
fi
# Bare IP or host:port gets the scheme and default port filled in.
case "$CATCHER" in
  http://*|https://*) ;;
  *:*)                CATCHER="http://$CATCHER" ;;
  *)                  CATCHER="http://$CATCHER:9999" ;;
esac

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BUILD="$HERE/build"
STAGE="$BUILD/app"
OUT="$BUILD/pushtest.zip"

rm -rf "$BUILD"
mkdir -p "$STAGE/icons"

# Substitute the catcher address into the copy, never the source.
sed 's|^var CATCHER = .*|var CATCHER = "'"$CATCHER"'";|' "$HERE/index.html" > "$STAGE/index.html"
if ! grep -q "var CATCHER = \"$CATCHER\";" "$STAGE/index.html"; then
  echo "could not substitute CATCHER — has the declaration in index.html changed?"
  exit 1
fi
cp "$HERE/sw.js" "$STAGE/"
cp "$ROOT/kaits/icons/icon-56.png"  "$STAGE/icons/"
cp "$ROOT/kaits/icons/icon-112.png" "$STAGE/icons/"

# Same permissions as the hosted manifest, root-relative paths.
cat > "$STAGE/manifest.webapp" <<JSON
{
  "version": "$VERSION",
  "name": "Push Test",
  "description": "Does a Web Push wake a KaiOS app from standby?",
  "type": "web",
  "launch_path": "/index.html",
  "icons": {
    "56": "/icons/icon-56.png",
    "112": "/icons/icon-112.png"
  },
  "developer": {
    "name": "you"
  },
  "permissions": {
    "push": {
      "description": "Wake this app when the test rings it"
    },
    "serviceworker": {
      "description": "Receive the push while the app is closed"
    },
    "desktop-notification": {
      "description": "Show that the push arrived"
    }
  },
  "messages": [
    { "serviceworker-notification": "/index.html" }
  ],
  "locales": {
    "en-US": {
      "name": "Push Test",
      "subtitle": "Web Push wake-up test",
      "description": "Does a Web Push wake a KaiOS app from standby?"
    }
  },
  "default_locale": "en-US",
  "cursor": false
}
JSON

# -r from inside the stage dir so paths are relative to the zip root.
( cd "$STAGE" && zip -q -r "$OUT" . -x ".*" )

echo "built $OUT"
echo
echo "contents (manifest.webapp must be at the top, with no folder prefix):"
unzip -l "$OUT" | sed 's/^/  /'
echo
# Check names, not the formatted table. Matching against `unzip -l` output means
# depending on its column layout and trailing whitespace, which differ between
# macOS and GNU unzip — and a false "PROBLEM" on a perfectly good package is
# worse than no check at all.
NAMES="$(unzip -Z1 "$OUT" 2>/dev/null || unzip -l "$OUT" | awk 'NF>=4 {print $NF}')"
if printf '%s\n' "$NAMES" | tr -d '\r' | grep -qx 'manifest.webapp'; then
  echo "OK: manifest.webapp is at the zip root."
else
  echo "PROBLEM: manifest.webapp is not at the zip root; the portal will reject this."
  exit 1
fi
echo
echo "catcher baked in: $CATCHER"
echo "Make sure 'python3 pushtest/catch.py serve' is running there when you open"
echo "the app on the phone."
