//go:build cgo && !nog722

package calls

// G.722 (wideband ADPCM, 16 kHz audio at 64 kbit/s) through spandsp.
//
// The middle option between Opus and G.711: the 2720 Flip's Gecko turned out
// to reject an Opus-only offer, and G.722 is the other wideband codec Firefox
// has carried in WebRTC. It's simple enough that the phone can't get it
// wrong, and it costs nothing to offer alongside the others.
//
// Note for RTP: G.722's clock rate is signalled as 8000 (an RFC 3551 error
// kept for compatibility) although it carries 16 kHz audio, so timestamps
// advance by 160 per 20 ms, not 320.

/*
#cgo pkg-config: spandsp
#include <stdint.h>
#include <spandsp/telephony.h>
#include <spandsp/g722.h>
*/
import "C"

import (
	"errors"
	"unsafe"
)

const g722Available = true

type g722Encoder struct{ s *C.g722_encode_state_t }
type g722Decoder struct{ s *C.g722_decode_state_t }

func newG722Encoder() (*g722Encoder, error) {
	s := C.g722_encode_init(nil, 64000, 0)
	if s == nil {
		return nil, errors.New("g722 encoder init failed")
	}
	return &g722Encoder{s: s}, nil
}

// Encode: 16 kHz samples in, one byte per two samples out.
func (e *g722Encoder) Encode(pcm []int16) []byte {
	if len(pcm) == 0 {
		return nil
	}
	out := make([]byte, len(pcm)/2+1)
	n := C.g722_encode(e.s, (*C.uint8_t)(unsafe.Pointer(&out[0])),
		(*C.int16_t)(unsafe.Pointer(&pcm[0])), C.int(len(pcm)))
	return out[:n]
}

func (e *g722Encoder) Close() {
	if e != nil && e.s != nil {
		C.g722_encode_free(e.s)
		e.s = nil
	}
}

func newG722Decoder() (*g722Decoder, error) {
	s := C.g722_decode_init(nil, 64000, 0)
	if s == nil {
		return nil, errors.New("g722 decoder init failed")
	}
	return &g722Decoder{s: s}, nil
}

func (d *g722Decoder) Decode(b []byte) []int16 {
	if len(b) == 0 {
		return nil
	}
	out := make([]int16, len(b)*2)
	n := C.g722_decode(d.s, (*C.int16_t)(unsafe.Pointer(&out[0])),
		(*C.uint8_t)(unsafe.Pointer(&b[0])), C.int(len(b)))
	return out[:n]
}

func (d *g722Decoder) Close() {
	if d != nil && d.s != nil {
		C.g722_decode_free(d.s)
		d.s = nil
	}
}
