package wa

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// A 1x1 lossless WebP, built here rather than committed as a binary.
//
// There is no WebP encoder in the standard library or in x/image, so the
// alternative was a checked-in fixture nobody could read or regenerate. This is
// the format written out: a RIFF/WEBP container around a VP8L chunk whose
// payload is the signature byte, 14 bits of width-1, 14 of height-1, an alpha
// hint and a version — all zero for a 1x1 — then the smallest legal bitstream:
// no transform, no colour cache, no meta-Huffman, and five single-symbol
// prefix codes, which between them describe one transparent pixel.
func tinyWebP() []byte {
	payload := []byte{0x2F, 0x00, 0x00, 0x00, 0x00, 0x88, 0x88, 0x08}
	chunk := append([]byte("VP8L"), le32(len(payload))...)
	chunk = append(chunk, payload...)
	if len(payload)%2 == 1 {
		chunk = append(chunk, 0)
	}
	out := append([]byte("RIFF"), le32(4+len(chunk))...)
	out = append(out, []byte("WEBP")...)
	return append(out, chunk...)
}

func le32(n int) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(n))
	return b
}

// The whole point: a sticker has to leave the daemon as something Gecko 48 can
// decode, and that engine predates WebP by seventeen releases.
func TestTranscodeStickerProducesPNG(t *testing.T) {
	out, animated, ok := TranscodeSticker(tinyWebP())
	if !ok {
		t.Fatal("a still WebP was not converted; the phone would show a broken image")
	}
	if animated {
		t.Error("a still image was reported as animated")
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not a valid PNG: %v", err)
	}
	if got := img.Bounds(); got.Dx() != 1 || got.Dy() != 1 {
		t.Errorf("bounds = %v, want 1x1", got)
	}
}

// Everything that is not a still WebP has to be REFUSED rather than mangled,
// because the caller's fallback is what saves the display. Converting a JPEG
// into a PNG here would be work for nothing; failing to notice an animation
// would mean logging an error for every animated sticker anyone sends.
func TestTranscodeStickerRefusesWhatItCannotConvert(t *testing.T) {
	// An animation: a VP8X container carrying an ANIM chunk.
	anim := append([]byte("RIFF"), le32(30)...)
	anim = append(anim, []byte("WEBPVP8X")...)
	anim = append(anim, le32(10)...)
	anim = append(anim, []byte("\x02\x00\x00\x00\x00\x00")...)
	anim = append(anim, []byte("ANIM")...)
	if _, animated, ok := TranscodeSticker(anim); ok || !animated {
		t.Errorf("animated WebP: ok=%v animated=%v, want false/true", ok, animated)
	}

	// Not WebP at all — a PNG, which the phone can already display.
	var buf bytes.Buffer
	png.Encode(&buf, image1x1())
	if _, animated, ok := TranscodeSticker(buf.Bytes()); ok || animated {
		t.Errorf("PNG input: ok=%v animated=%v, want false/false", ok, animated)
	}

	// Truncated: a real WebP header with the bitstream cut off. Must fail
	// cleanly — the caller falls back to the still preview.
	full := tinyWebP()
	if _, animated, ok := TranscodeSticker(full[:len(full)-3]); ok || animated {
		t.Errorf("truncated WebP: ok=%v animated=%v, want false/false", ok, animated)
	}

	if _, _, ok := TranscodeSticker(nil); ok {
		t.Error("nil input was converted")
	}
	if _, _, ok := TranscodeSticker([]byte("RIFF")); ok {
		t.Error("a four-byte file was converted")
	}
}

// IsWebP decides by container, not by the mime string the sender claimed — a
// sticker sent with a wrong or missing mime still has to be converted.
func TestIsWebPReadsTheContainer(t *testing.T) {
	if !IsWebP(tinyWebP()) {
		t.Error("a real WebP was not recognised")
	}
	if IsWebP([]byte("RIFF\x00\x00\x00\x00AVI ")) {
		t.Error("a RIFF file that is not WebP was recognised as one")
	}
	if IsWebP(nil) || IsWebP([]byte("WEBP")) {
		t.Error("recognised something too short to be a WebP")
	}
}

func image1x1() *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, 1, 1))
	m.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	return m
}
