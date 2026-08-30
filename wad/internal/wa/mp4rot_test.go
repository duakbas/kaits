package wa

import (
	"encoding/binary"
	"testing"
)

// buildMP4 assembles the smallest file this parser has to understand: a top
// level box it should skip over, then moov > trak > tkhd carrying a display
// matrix. Built here rather than committed as a binary, so what is being
// asserted is readable.
func buildMP4(version byte, a, b int32) []byte {
	box := func(typ string, payload []byte) []byte {
		out := make([]byte, 8, 8+len(payload))
		binary.BigEndian.PutUint32(out[0:4], uint32(8+len(payload)))
		copy(out[4:8], typ)
		return append(out, payload...)
	}

	// version+flags, times/ids, then layer/group/volume padding, then a 9-entry
	// matrix of which only the first two values are read.
	var tkhd []byte
	tkhd = append(tkhd, version, 0, 0, 0)
	if version == 1 {
		tkhd = append(tkhd, make([]byte, 32)...)
	} else {
		tkhd = append(tkhd, make([]byte, 20)...)
	}
	tkhd = append(tkhd, make([]byte, 16)...)
	m := make([]byte, 36)
	binary.BigEndian.PutUint32(m[0:4], uint32(a))
	binary.BigEndian.PutUint32(m[4:8], uint32(b))
	tkhd = append(tkhd, m...)
	tkhd = append(tkhd, make([]byte, 8)...) // width, height

	return append(
		box("ftyp", []byte("isom0000")),
		box("moov", box("trak", box("tkhd", tkhd)))...,
	)
}

// A portrait video from a phone is stored landscape with a rotation matrix, and
// this engine ignores the matrix. Reading it is the whole fix, so each of the
// four right angles has to come out right.
func TestVideoRotationReadsTheMatrix(t *testing.T) {
	const one = 1 << 16
	cases := []struct {
		name string
		a, b int32
		want int
	}{
		{"upright", one, 0, 0},
		{"quarter turn", 0, one, 90},
		{"upside down", -one, 0, 180},
		{"three quarters", 0, -one, 270},
	}
	for _, c := range cases {
		for _, version := range []byte{0, 1} {
			got := VideoRotation(buildMP4(version, c.a, c.b))
			if got != c.want {
				t.Errorf("%s (tkhd v%d) = %d, want %d", c.name, version, got, c.want)
			}
		}
	}
}

// Everything unrecognised must come out as zero. Guessing at a matrix we do not
// understand would tip a correct video over, which is worse than leaving a
// wrong one alone — and a truncated or hostile file must not panic.
func TestVideoRotationIsZeroWhenUnsure(t *testing.T) {
	const one = 1 << 16
	// A scale rather than a rotation.
	if got := VideoRotation(buildMP4(0, 2*one, 0)); got != 0 {
		t.Errorf("a scaling matrix gave %d, want 0", got)
	}

	full := buildMP4(0, 0, one)
	for _, n := range []int{0, 1, 8, 12, 20, len(full) - 40, len(full) - 1} {
		if n < 0 || n > len(full) {
			continue
		}
		if got := VideoRotation(full[:n]); got != 0 && n < len(full) {
			t.Errorf("truncated to %d bytes gave %d, want 0", n, got)
		}
	}
	if got := VideoRotation(nil); got != 0 {
		t.Errorf("nil gave %d", got)
	}
	// A box claiming to be enormous must not send the scan off the end.
	evil := []byte{0xff, 0xff, 0xff, 0xff, 'm', 'o', 'o', 'v'}
	if got := VideoRotation(evil); got != 0 {
		t.Errorf("oversized box gave %d", got)
	}
}
