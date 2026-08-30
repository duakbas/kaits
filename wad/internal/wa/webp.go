package wa

import (
	"bytes"
	"image/png"

	"golang.org/x/image/webp"
)

// Stickers are WebP, and this phone cannot read WebP.
//
// Gecko 48 predates WebP support by seventeen releases, so a sticker <img>
// resolves, downloads, and renders as a broken image — the one media type
// WhatsApp uses that the browser here has no decoder for. Everything else
// (JPEG, PNG, H.264 in MP4) it handles.
//
// So the daemon converts. It is the only party in this system with a decoder,
// it already has the bytes in hand for the download, and converting here means
// the app needs no knowledge of any of it: a sticker arrives as a PNG and is an
// image like any other.
//
// Animated stickers are not converted. golang.org/x/image/webp decodes still
// VP8 and VP8L and refuses animations outright, and there is no animated format
// this engine could show anyway — an animated GIF would mean a VP8 decoder plus
// a GIF encoder plus frame compositing, for a result the size of a video on a
// phone that gets killed for being large. The caller falls back to the still
// preview WhatsApp ships inside every sticker message.

// IsWebP reports whether these bytes are a WebP file, by container rather than
// by the mime string the sender chose to claim.
func IsWebP(b []byte) bool {
	return len(b) >= 12 && bytes.Equal(b[0:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP"))
}

// isAnimatedWebP reports whether the file is an animation, which is worth
// telling apart from a corrupt one: a decode failure on an animation is
// expected and normal, and logging it as an error would be noise on every
// animated sticker anyone sends.
//
// The ANIM chunk appears in the extended (VP8X) form only, near the start.
func isAnimatedWebP(b []byte) bool {
	head := b
	if len(head) > 64 {
		head = head[:64]
	}
	return bytes.Contains(head, []byte("ANIM")) || bytes.Contains(head, []byte("ANMF"))
}

// TranscodeSticker turns a still WebP into a PNG this phone can display.
//
// Returns ok=false for anything it cannot convert — an animation, a format
// x/image doesn't know, a truncated file — and the caller is expected to fall
// back rather than to fail: a still preview beats a broken image, and a broken
// image beats an error.
func TranscodeSticker(data []byte) (out []byte, animated bool, ok bool) {
	if !IsWebP(data) {
		return nil, false, false
	}
	if isAnimatedWebP(data) {
		return nil, true, false
	}
	img, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false, false
	}
	var buf bytes.Buffer
	// Default compression: a sticker is a few hundred pixels square and the
	// difference between fastest and best is measured in kilobytes, while this
	// runs once per sticker per view on a machine with nothing else to do.
	if err := png.Encode(&buf, img); err != nil {
		return nil, false, false
	}
	return buf.Bytes(), false, true
}
