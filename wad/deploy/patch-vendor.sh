#!/usr/bin/env bash
# patch-vendor.sh — teach the vendored whatsmeow that live location is a media
# type, which upstream does not.
#
# WHY THIS EXISTS
#
# A WhatsApp message stanza carries a "mediatype" attribute, and whatsmeow sets
# it from getMediaTypeFromMessage(). That function knows about image, sticker,
# document, audio, ptt, video, gif, vcard and a dozen more — and has no case for
# LiveLocationMessage, so it returns "" and send.go then omits the attribute
# entirely:
#
#     if mediaType := getMediaTypeFromMessage(message); mediaType != "" {
#         plaintextNode.Attrs["mediatype"] = mediaType
#     }
#
# So every live-location stanza we send, the opening one included, goes out
# unlabelled, while a real client labels its own "livelocation". That is the
# most plausible reason a share arrives and immediately reads as ended, and why
# nothing we send afterwards is ever applied to it: to the receiving client
# these were never part of a live session at all.
#
# The library exposes no way to set a stanza attribute — SendRequestExtra's
# AdditionalNodes adds CHILD nodes, and MsgMetaInfo carries bot and thread
# targets. So the choice is patch it or do without, and this is one case in one
# switch.
#
# It runs against the vendored tree inside the bundle rather than against the
# module cache, so nothing outside this build is touched: the patched copy
# exists only in the tarball the server compiles.
#
#   ./patch-vendor.sh /path/to/src   (called by mkbundle.sh; safe to re-run)

set -euo pipefail

SRC="${1:-}"
[ -n "$SRC" ] || { echo "usage: $0 <source-dir-with-vendor>" >&2; exit 2; }

F="$SRC/vendor/go.mau.fi/whatsmeow/send.go"
if [ ! -f "$F" ]; then
  echo "    no vendored whatsmeow; skipping the live-location patch"
  exit 0
fi

if grep -q '"livelocation"' "$F"; then
  echo "    whatsmeow already knows livelocation"
  exit 0
fi

# awk rather than sed -i, which differs between GNU and BSD, and this runs on
# whichever laptop builds the bundle. Only the default: inside the one function
# is touched — tracking the function by name rather than matching "default:"
# anywhere, of which send.go has several.
awk '
  /^func getMediaTypeFromMessage\(/ { infn = 1 }
  infn && /^\tdefault:$/ && !done {
    print "\tcase msg.LiveLocationMessage != nil:"
    print "\t\t// Added by wad: upstream has no case for this, so the stanza"
    print "\t\t// went out with no mediatype attribute and a receiving client"
    print "\t\t// had no reason to treat it as part of a live session."
    print "\t\treturn \"livelocation\""
    done = 1
  }
  infn && /^}$/ { infn = 0 }
  { print }
' "$F" > "$F.new"

if ! grep -q '"livelocation"' "$F.new"; then
  rm -f "$F.new"
  echo "ERROR: the live-location patch did not apply." >&2
  echo "whatsmeow's getMediaTypeFromMessage has probably changed shape. Check" >&2
  echo "whether upstream now handles LiveLocationMessage itself — if it does," >&2
  echo "delete this script and the call to it in mkbundle.sh." >&2
  exit 1
fi

mv "$F.new" "$F"
echo "    patched whatsmeow: live location stanzas now carry mediatype"
