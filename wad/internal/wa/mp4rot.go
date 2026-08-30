package wa

import "encoding/binary"

// Reading the rotation out of an MP4.
//
// A video shot in portrait on a phone is almost never stored in portrait. It is
// stored landscape with a rotation matrix in the track header, and every modern
// player applies that matrix on the way to the screen. Gecko 48 does not — it
// predates rotation support — so the phone plays a portrait video on its side,
// which is exactly what was reported.
//
// The app cannot work this out for itself; the matrix is in the file it is
// streaming and there is no API to ask. So the daemon reads it and says so, and
// the app tips the picture over with a CSS transform.
//
// This walks the box tree rather than decoding anything, so it needs only the
// first few kilobytes in principle — though moov is at the end of some files,
// which is why the caller hands over the whole thing it already has.

// VideoRotation returns 0, 90, 180 or 270 — the degrees clockwise a player must
// rotate this video to display it upright. Unknown or absent means 0, because
// the safe failure is the picture we would have drawn anyway.
func VideoRotation(data []byte) int {
	moov := findBox(data, "moov")
	if moov == nil {
		return 0
	}
	// The first video track wins. A file with several is not something this
	// phone is going to play well regardless.
	for rest := moov; len(rest) > 0; {
		trak, next := nextBox(rest, "trak")
		if trak == nil {
			return 0
		}
		if tkhd := findBox(trak, "tkhd"); tkhd != nil {
			if deg, ok := rotationFromTKHD(tkhd); ok && deg != 0 {
				return deg
			}
		}
		rest = next
	}
	return 0
}

// rotationFromTKHD reads the top-left 2x2 of the 3x3 display matrix, which is
// all that distinguishes the four right-angle rotations.
func rotationFromTKHD(payload []byte) (int, bool) {
	if len(payload) < 4 {
		return 0, false
	}
	version := payload[0]
	// version/flags, then the timestamps and ids, then layer/group/volume and
	// their padding. The only difference between the versions is 32- versus
	// 64-bit times.
	off := 4 + 20 + 16
	if version == 1 {
		off = 4 + 32 + 16
	}
	if len(payload) < off+8 {
		return 0, false
	}
	a := int32(binary.BigEndian.Uint32(payload[off : off+4]))
	b := int32(binary.BigEndian.Uint32(payload[off+4 : off+8]))

	// 16.16 fixed point, so 1.0 is 0x00010000. Only the signs and which of the
	// two is non-zero matter.
	const one = 1 << 16
	switch {
	case a == one && b == 0:
		return 0, true
	case a == 0 && b == one:
		return 90, true
	case a == -one && b == 0:
		return 180, true
	case a == 0 && b == -one:
		return 270, true
	}
	// Some other transform — a scale, a flip, something exotic. Not ours to
	// interpret, and guessing would tip a correct video over.
	return 0, false
}

// findBox returns the PAYLOAD of the first box of this type at this level.
func findBox(data []byte, typ string) []byte {
	box, _ := nextBox(data, typ)
	return box
}

// nextBox scans forward for a box of the given type, returning its payload and
// everything after it, so a caller can keep looking for siblings.
func nextBox(data []byte, typ string) (payload, rest []byte) {
	for len(data) >= 8 {
		size := int(binary.BigEndian.Uint32(data[0:4]))
		name := string(data[4:8])
		header := 8

		switch {
		case size == 1:
			// 64-bit size. Anything needing it is far larger than a phone
			// video, but a malformed file must not walk off the end.
			if len(data) < 16 {
				return nil, nil
			}
			big := binary.BigEndian.Uint64(data[8:16])
			if big > uint64(len(data)) {
				return nil, nil
			}
			size, header = int(big), 16
		case size == 0:
			// Runs to the end of the file.
			size = len(data)
		case size < 8 || size > len(data):
			return nil, nil
		}

		if name == typ {
			return data[header:size], data[size:]
		}
		data = data[size:]
	}
	return nil, nil
}
