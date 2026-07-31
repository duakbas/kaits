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
#   ./pushtest/package.sh        -> pushtest/build/pushtest.zip

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BUILD="$HERE/build"
STAGE="$BUILD/app"
OUT="$BUILD/pushtest.zip"

rm -rf "$BUILD"
mkdir -p "$STAGE/icons"

cp "$HERE/index.html" "$STAGE/"
cp "$HERE/sw.js"      "$STAGE/"
cp "$ROOT/wa-kaios/icons/icon-56.png"  "$STAGE/icons/"
cp "$ROOT/wa-kaios/icons/icon-112.png" "$STAGE/icons/"

# Same permissions as the hosted manifest, root-relative paths.
cat > "$STAGE/manifest.webapp" <<'JSON'
{
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
  "default_locale": "en",
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
if unzip -l "$OUT" | grep -qE ' manifest\.webapp$'; then
  echo "OK: manifest.webapp is at the zip root."
else
  echo "PROBLEM: manifest.webapp is not at the zip root; the portal will reject this."
  exit 1
fi
echo
echo "Before uploading, check CATCHER is set in pushtest/index.html — the phone"
echo "posts its push endpoint there, and it is baked into the package."
