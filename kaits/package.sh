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
#   ./kaits/package.sh            -> kaits/build/kaits.zip  (version 0.0.2)
#   ./kaits/package.sh 0.0.3      for a re-upload
#   ./kaits/package.sh 0.0.3 privileged
#
# The second argument picks the app type. "web" is the default and is what has
# been shipping; "privileged" additionally asks for the permissions that gate
# recording a voice note, reading the phone's ringer profile, reading and
# writing the address book, and putting our beep on the notification audio
# channel. All four are already coded against and currently sit dormant behind
# a feature check.
#
# Asking costs a review. If a privileged build is rejected or held, rebuild
# without the argument and you are back to the fast path — nothing else in the
# package differs, and the app itself is identical either way.
#
# NOTE: js/config.js is baked into the package, and it contains WAD_TOKEN. The
# zip is a credential. Don't pass it around.

set -euo pipefail

VERSION="${1:-0.0.2}"
APPTYPE="${2:-web}"

case "$APPTYPE" in
  web|privileged) ;;
  *) echo "app type must be 'web' or 'privileged', got '$APPTYPE'" >&2; exit 2 ;;
esac

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

cp -R "$HERE/index.html" "$HERE/sw.js" "$HERE/css" "$HERE/js" "$HERE/icons" \
      "$HERE/audio" "$STAGE/"

# The keepalive loop is generated, not hand-written, and the app is silently
# less able to survive backgrounding without it — a missing file here would
# show up as "it still gets killed" rather than as an error.
if [ ! -f "$STAGE/audio/keepalive.wav" ]; then
  echo "PROBLEM: audio/keepalive.wav is missing; run kaits/audio/mkkeepalive.py" >&2
  exit 1
fi

# Stamp the build version into the package so the running app can say which
# build it is. Without this, "did the update land?" is unanswerable from the
# phone, and you end up testing a fix against the old code.
printf '\nwindow.KAITS_VERSION = "%s";\n' "$VERSION" >> "$STAGE/js/config.js"

# Version and app type are the only fields that change per build, so patch them
# rather than keeping a second copy of the manifest that can drift from the
# real one.
python3 - "$HERE/manifest.webapp" "$STAGE/manifest.webapp" "$VERSION" "$APPTYPE" <<'PY'
import json, sys

src, dst, version, apptype = sys.argv[1:5]
m = json.load(open(src))
m["version"] = version
m["type"] = apptype

# Permissions that a web-type app is not granted. Each one is already coded
# against with a feature check, so a refused permission costs a feature and
# never breaks the build.
PRIVILEGED = {
    "audio-capture": {
        "description": "Record a voice message when you press and hold to record",
    },
    "audio-channel-notification": {
        "description": "Play the new-message alert on the notification channel "
                       "so it follows the phone's volume and silent mode",
    },
    "contacts": {
        "description": "Show your saved contact names instead of raw phone "
                       "numbers, and save a new number to the address book",
        "access": "readwrite",
    },
    "settings": {
        "description": "Follow the phone's ringer and vibration settings when "
                       "alerting about a new message",
        "access": "readonly",
    },
}

if apptype == "privileged":
    m["permissions"].update(PRIVILEGED)

json.dump(m, open(dst, "w"), indent=2)

# A privileged package is served under a CSP of script-src 'self', so an inline
# script or an inline event handler would load fine in testing and then be
# silently dead on the phone. Cheaper to refuse to build than to debug that.
print("manifest ok:", m["name"], m["version"], "(%s)" % m["type"])
print("permissions:", ", ".join(sorted(m["permissions"])))
PY

# A privileged package runs under an enforced CSP — script-src 'self',
# object-src 'none' — which the portal applies for us and which cannot be
# relaxed. An inline script or an inline event handler works everywhere you'd
# test it and is then silently dead on the phone, so check here rather than
# find out from a screenshot of a blank screen.
if [ "$APPTYPE" = "privileged" ]; then
  BAD=0
  if grep -rlP '<script(?![^>]*\bsrc=)' --include='*.html' "$STAGE" 2>/dev/null | grep -q .; then
    echo "PROBLEM: inline <script> found; privileged CSP forbids it:" >&2
    grep -rnP '<script(?![^>]*\bsrc=)' --include='*.html' "$STAGE" >&2 || true
    BAD=1
  fi
  if grep -rn --include='*.html' -iE '<[^>]+\son[a-z]+[[:space:]]*=' "$STAGE" >/dev/null 2>&1; then
    echo "PROBLEM: inline event handler attribute found; privileged CSP forbids it:" >&2
    grep -rn --include='*.html' -iE '<[^>]+\son[a-z]+[[:space:]]*=' "$STAGE" >&2
    BAD=1
  fi
  if grep -rn --include='*.js' -E '(^|[^a-zA-Z_.])eval[[:space:]]*\(|new[[:space:]]+Function[[:space:]]*\(' "$STAGE" >/dev/null 2>&1; then
    echo "PROBLEM: eval or Function() found; privileged CSP forbids it:" >&2
    grep -rn --include='*.js' -E '(^|[^a-zA-Z_.])eval[[:space:]]*\(|new[[:space:]]+Function[[:space:]]*\(' "$STAGE" >&2
    BAD=1
  fi
  [ "$BAD" -eq 0 ] && echo "CSP check: no inline script, handler or eval."
  [ "$BAD" -eq 0 ] || exit 1
fi

# Editor leftovers get copied in with the directories and would otherwise be
# uploaded — a config.js.bak sitting next to config.js is exactly the sort of
# thing that ships a stale token or a half-edited file to a store.
find "$STAGE" \( -name '*.bak' -o -name '*.orig' -o -name '*.rej' -o -name '*~' \
  -o -name '*.swp' -o -name '.DS_Store' -o -name 'mk*.py' \) -delete

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
