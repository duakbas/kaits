package calls

import (
	"math"
	"testing"
)

// µ-law has a NEGATIVE ZERO. 0x7F and 0xFF both decode to linear 0, so the
// code set is 256 wide but only covers 255 levels, and one of the two zero
// codes cannot survive a round trip through a linear sample that has no sign.
// That is the standard, not a defect — but it is the one place the obvious
// invariant does not hold, so it is worth stating exactly rather than leaving
// a reader to discover it from a failing test later.
const ulawNegativeZero = 0x7F

// Every OTHER code decodes to a linear value that encodes back to itself. This
// is what pins the segment arithmetic down: get an exponent or a mantissa
// shift wrong and codes start landing on their neighbours, which no amount of
// listening to a test tone would localise.
func TestEveryULawCodeSurvivesARoundTrip(t *testing.T) {
	for i := 0; i < 256; i++ {
		u := byte(i)
		back := EncodeULawSample(DecodeULawSample(u))
		if u == ulawNegativeZero {
			// Decodes to 0, which has no sign left to distinguish it, so it
			// re-encodes as positive zero. Anything else would be a bug.
			if back != 0xFF {
				t.Errorf("negative zero re-encoded to %#02x, want positive zero 0xff", back)
			}
			continue
		}
		if back != u {
			t.Errorf("code %#02x decoded to %d and re-encoded to %#02x",
				u, DecodeULawSample(u), back)
		}
	}
}

// Known answers from the published µ-law table. Properties prove the codec is
// self-consistent; only fixed vectors prove it is the same codec everyone
// else implements, which is what matters when the other end is Gecko.
func TestULawMatchesThePublishedTable(t *testing.T) {
	for _, tc := range []struct {
		code byte
		want int16
	}{
		{0x00, -32124}, // most negative
		{0x80, 32124},  // most positive
		{0x7F, 0},      // negative zero
		{0xFF, 0},      // positive zero
		{0xFE, 8},
		{0x7E, -8},
	} {
		if got := DecodeULawSample(tc.code); got != tc.want {
			t.Errorf("decode(%#02x) = %d, want %d", tc.code, got, tc.want)
		}
	}
	// And the direction the encoder is actually used in.
	if got := EncodeULawSample(0); got != 0xFF {
		t.Errorf("encode(0) = %#02x, want 0xff", got)
	}
	if got := EncodeULawSample(32767); got != 0x80 {
		t.Errorf("encode(full scale) = %#02x, want 0x80", got)
	}
	if got := EncodeULawSample(-32768); got != 0x00 {
		t.Errorf("encode(-full scale) = %#02x, want 0x00", got)
	}
}

// 256 codes, 255 distinct levels, and the single collision is the signed zero.
// Any other duplicate is a wasted code and a flat spot in the transfer curve.
func TestULawLevelsAreDistinctApartFromSignedZero(t *testing.T) {
	seen := map[int16]byte{}
	collisions := 0
	for i := 0; i < 256; i++ {
		v := DecodeULawSample(byte(i))
		if prev, dup := seen[v]; dup {
			collisions++
			if v != 0 {
				t.Errorf("codes %#02x and %#02x both decode to %d", prev, byte(i), v)
			}
		}
		seen[v] = byte(i)
	}
	if collisions != 1 {
		t.Errorf("got %d colliding codes, want exactly 1 (the signed zero)", collisions)
	}
	if len(seen) != 255 {
		t.Errorf("got %d distinct levels, want 255", len(seen))
	}
}

// µ-law is companded: quantisation error grows with amplitude, and near
// silence it has to be small. These bounds are what make it usable for speech —
// if the quiet end were coarse, the codec would hiss.
func TestQuantisationErrorStaysWithinTheCompandingCurve(t *testing.T) {
	for _, tc := range []struct{ amp, maxErr int }{
		{100, 16},     // quiet: fine steps
		{1000, 128},   // conversational
		{8000, 512},   // loud
		{32000, 4096}, // near full scale, where the steps are coarsest
	} {
		for v := -tc.amp; v <= tc.amp; v += 7 {
			got := int(DecodeULawSample(EncodeULawSample(int16(v))))
			if e := got - v; e > tc.maxErr || e < -tc.maxErr {
				t.Errorf("at amplitude %d: %d round-tripped to %d (error %d, max %d)",
					tc.amp, v, got, e, tc.maxErr)
			}
		}
	}
}

// Silence is the case a listener notices immediately: if zero does not stay
// near zero, every gap between words carries a tone.
func TestSilenceStaysSilent(t *testing.T) {
	for _, v := range []int16{0, 1, -1, 2, -2} {
		got := DecodeULawSample(EncodeULawSample(v))
		if got > 8 || got < -8 {
			t.Errorf("%d round-tripped to %d, which is audible in a gap", v, got)
		}
	}
}

// Monotonicity. A transfer curve that goes backwards anywhere is distortion
// that no error bound would catch, because the magnitude can still be small.
func TestEncodingIsMonotonic(t *testing.T) {
	prev := math.MinInt
	for v := -32768; v <= 32767; v += 3 {
		got := int(DecodeULawSample(EncodeULawSample(int16(v))))
		if got < prev {
			t.Fatalf("at %d the decoded value fell from %d to %d", v, prev, got)
		}
		prev = got
	}
}

// Loud samples clip rather than wrapping. Wrapping would turn a shout into
// noise at the opposite polarity, which is the single most audible failure
// this codec can have.
func TestLoudSamplesClipRatherThanWrap(t *testing.T) {
	loudPos := DecodeULawSample(EncodeULawSample(32767))
	loudNeg := DecodeULawSample(EncodeULawSample(-32768))
	if loudPos < 30000 {
		t.Errorf("full-scale positive came back as %d", loudPos)
	}
	if loudNeg > -30000 {
		t.Errorf("full-scale negative came back as %d", loudNeg)
	}
	// Same sign as they went in, which is what "clip" means and "wrap" doesn't.
	if loudPos < 0 || loudNeg > 0 {
		t.Errorf("polarity inverted at full scale: %d / %d", loudPos, loudNeg)
	}
}

// Frame helpers: one byte per sample, which is the arithmetic the RTP side
// depends on — 20 ms at 8 kHz must be exactly 160 bytes or the packetiser's
// timestamps drift against the clock.
func TestFrameHelpersAreOneBytePerSample(t *testing.T) {
	pcm := make([]int16, 160)
	for i := range pcm {
		pcm[i] = int16(i * 37)
	}
	enc := EncodeULaw(pcm)
	if len(enc) != 160 {
		t.Fatalf("160 samples encoded to %d bytes, want 160", len(enc))
	}
	dec := DecodeULaw(enc)
	if len(dec) != 160 {
		t.Fatalf("160 bytes decoded to %d samples, want 160", len(dec))
	}
	for i := range dec {
		if e := int(dec[i]) - int(pcm[i]); e > 256 || e < -256 {
			t.Errorf("sample %d: %d -> %d", i, pcm[i], dec[i])
		}
	}
	if len(EncodeULaw(nil)) != 0 || len(DecodeULaw(nil)) != 0 {
		t.Error("empty frames should stay empty")
	}
}
