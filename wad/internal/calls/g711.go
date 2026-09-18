package calls

// G.711 µ-law, which is the whole of the daemon's codec responsibility on the
// phone leg.
//
// The obvious pairing for WebRTC is Opus, and it is the expensive one: there is
// no pure-Go Opus ENCODER, so the daemon would need cgo and libopus — a system
// dependency in install.sh and another way for a build to fail on a box we
// cannot see. PCMU costs a 256-entry table and nothing else, it is 64 kbit/s,
// and it is telephone-grade, which is exactly what this is: a phone call on a
// feature phone. Firefox has spoken PCMU since long before 48.
//
// This is the Sun reference implementation (g711.c, public domain), ported
// rather than reinvented. The encoder works on the top 14 bits: µ-law's
// segments are defined on a 14-bit magnitude, so the low two bits of a 16-bit
// sample are discarded before anything else happens.

const (
	// ulawBias is added to every magnitude before the segment search. It is
	// what makes the smallest segment behave, and it has to be subtracted
	// again on the way back out.
	ulawBias = 0x84
	// ulawClip is the largest magnitude representable once shifted to 14 bits.
	// Anything louder is a clipped sample either way.
	ulawClip = 8159
)

// segEnd is the top of each of the eight µ-law segments, on the biased 14-bit
// magnitude. The segment a sample lands in is its exponent.
var segEnd = [8]int32{0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF, 0x1FFF}

// EncodeULawSample turns one 16-bit linear sample into one µ-law byte.
func EncodeULawSample(pcm int16) byte {
	v := int32(pcm) >> 2 // µ-law segments are defined on 14 bits

	// The sign is carried by inverting the whole byte at the end, which is why
	// the mask differs rather than the value.
	var mask int32
	if v < 0 {
		v = -v
		mask = 0x7F
	} else {
		mask = 0xFF
	}
	if v > ulawClip {
		v = ulawClip
	}
	v += ulawBias >> 2

	seg := ulawSegment(v)
	if seg >= 8 {
		// Only reachable if the clip above were removed; kept because the
		// reference has it and a silent out-of-range shift would be worse.
		return byte(0x7F ^ mask)
	}
	u := (seg << 4) | ((v >> (seg + 1)) & 0x0F)
	return byte(u ^ mask)
}

// DecodeULawSample turns one µ-law byte back into a 16-bit linear sample.
func DecodeULawSample(u byte) int16 {
	x := int32(^u)
	t := ((x & 0x0F) << 3) + ulawBias
	t <<= uint((x & 0x70) >> 4)
	if x&0x80 != 0 {
		return int16(ulawBias - t)
	}
	return int16(t - ulawBias)
}

func ulawSegment(v int32) int32 {
	for i, end := range segEnd {
		if v <= end {
			return int32(i)
		}
	}
	return 8
}

// EncodeULaw encodes a whole frame. One byte out per sample in, which is what
// makes the RTP side trivial: 20 ms at 8 kHz is 160 samples and 160 bytes.
func EncodeULaw(pcm []int16) []byte {
	out := make([]byte, len(pcm))
	for i, s := range pcm {
		out[i] = EncodeULawSample(s)
	}
	return out
}

// DecodeULaw decodes a whole frame.
func DecodeULaw(u []byte) []int16 {
	out := make([]int16, len(u))
	for i, b := range u {
		out[i] = DecodeULawSample(b)
	}
	return out
}
