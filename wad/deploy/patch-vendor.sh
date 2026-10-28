#!/usr/bin/env bash
# patch-vendor.sh — two small patches to vendored libraries:
#   1. whatsmeow: live location is a media type, which upstream does not know.
#   2. meowcaller: inbound call audio keeps its RTP timestamp (see below).
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

patch_whatsmeow() {
  F="$SRC/vendor/go.mau.fi/whatsmeow/send.go"
  if [ ! -f "$F" ]; then
    echo "    no vendored whatsmeow; skipping the live-location patch"
    return 0
  fi

  if grep -q '"livelocation"' "$F"; then
    echo "    whatsmeow already knows livelocation"
    return 0
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
    return 1
  fi

  mv "$F.new" "$F"
  echo "    patched whatsmeow: live location stanzas now carry mediatype"
}

# ---------------------------------------------------------------------------
# meowcaller: hand inbound call audio over WITH its RTP timestamp.
#
# meowcaller decodes each WhatsApp audio packet and pushes it through its own
# playout buffer, which then calls the sink with bare PCM. Two problems for wad:
#
#   - the timestamp is dropped, so wad cannot tell a lost packet from a late
#     one, and has to re-clock and zero-fill (the pops);
#   - the buffer resets on any out-of-order or duplicate packet (the unsigned
#     timestamp delta wraps to "huge gap"), dropping a frame and re-prefilling
#     120 ms — a stall, then a burst.
#
# The patch adds an optional TimedAudioSink interface. If the sink implements
# it, frames go straight to it with their RTP timestamp and the playout buffer
# is skipped; any other sink (the WAV recorder, the example players) gets the
# unchanged behaviour. wad then forwards each frame to the phone stamped with
# WhatsApp's own timestamp and lets the phone's jitter buffer do the rest.
patch_meowcaller() {
  local D="$SRC/vendor/github.com/purpshell/meowcaller"
  local F="$D/engine_media.go"
  if [ ! -f "$F" ]; then
    echo "    no vendored meowcaller; skipping the timed-sink patch"
    return 0
  fi
  if [ -f "$D/wad_timed.go" ]; then
    echo "    meowcaller already has the timed sink"
    return 0
  fi

  local ANCHOR='			playoutStarted, playoutErr := audioPlayout.Push(audio.Timestamp, audio.PCM, sink)'
  if [ "$(grep -cxF "$ANCHOR" "$F")" != 1 ]; then
    echo "ERROR: the meowcaller timed-sink patch did not apply." >&2
    echo "engine_media.go no longer has exactly one playout Push line; the" >&2
    echo "inbound audio path has changed. Re-do the patch against the new code." >&2
    return 1
  fi

  awk -v anchor="$ANCHOR" '
    $0 == anchor {
      print "\t\t\t// Added by wad: a TimedAudioSink gets each frame with its RTP"
      print "\t\t\t// timestamp, bypassing the playout buffer (see wad_timed.go)."
      print "\t\t\tvar playoutStarted bool"
      print "\t\t\tvar playoutErr error"
      print "\t\t\tif tsk, ok := sink.(TimedAudioSink); ok {"
      print "\t\t\t\tplayoutErr = tsk.WriteTimedFrame(audio.Timestamp, audio.PCM)"
      print "\t\t\t} else {"
      print "\t\t\t\tplayoutStarted, playoutErr = audioPlayout.Push(audio.Timestamp, audio.PCM, sink)"
      print "\t\t\t}"
      next
    }
    { print }
  ' "$F" > "$F.new"

  cat > "$D/wad_timed.go" <<'GOEOF'
package meowcaller

// Added by wad (deploy/patch-vendor.sh).
//
// TimedAudioSink is an AudioSink that wants inbound audio as it arrives, with
// the RTP timestamp it was sent with (16 kHz units), instead of re-timed by
// the playout buffer. Frames can arrive late, out of order, duplicated, or not
// at all; the sink owns all of that.
type TimedAudioSink interface {
	AudioSink
	WriteTimedFrame(rtpTimestamp uint32, frame []float32) error
}
GOEOF
  mv "$F.new" "$F"
  echo "    patched meowcaller: inbound audio carries its RTP timestamp"
}

patch_whatsmeow
patch_meowcaller
