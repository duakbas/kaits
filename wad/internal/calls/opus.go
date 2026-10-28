//go:build cgo && !noopus

package calls

// Opus for the phone leg, through libopus.
//
// The first version of calling used G.711 so the daemon needed no codec
// library. It worked, and it sounded like 1972: G.711 is 8 kHz, so everything
// above 3.4 kHz — the part of speech that makes s, f and t distinguishable and
// a voice sound present — was cut before it reached the phone. WhatsApp's own
// leg (MLow via meowcaller) is 16 kHz, and so is VoLTE's AMR-WB, which is why
// a normal call on the same handset sounded better.
//
// Opus at 16 kHz carries the whole wideband signal with no resampling at all:
// meowcaller's 16 kHz in, 16 kHz out. Gecko has spoken Opus over WebRTC since
// the first release that had WebRTC, so offering it alone is safe.
//
// libopus is a system package (libopus-dev); deploy-wad installs it. The
// daemon already needs cgo for sqlite, so this costs a header, not a toolchain.

/*
#cgo pkg-config: opus
#include <opus.h>

static int wad_opus_enc_ctl(OpusEncoder *e, int bitrate, int fec, int loss, int complexity) {
	int r;
	if ((r = opus_encoder_ctl(e, OPUS_SET_BITRATE(bitrate))) != OPUS_OK) return r;
	if ((r = opus_encoder_ctl(e, OPUS_SET_INBAND_FEC(fec))) != OPUS_OK) return r;
	if ((r = opus_encoder_ctl(e, OPUS_SET_PACKET_LOSS_PERC(loss))) != OPUS_OK) return r;
	if ((r = opus_encoder_ctl(e, OPUS_SET_COMPLEXITY(complexity))) != OPUS_OK) return r;
	if ((r = opus_encoder_ctl(e, OPUS_SET_SIGNAL(OPUS_SIGNAL_VOICE))) != OPUS_OK) return r;
	return opus_encoder_ctl(e, OPUS_SET_DTX(0));
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

const opusAvailable = true

type opusEncoder struct{ e *C.OpusEncoder }
type opusDecoder struct{ d *C.OpusDecoder }

func newOpusEncoder(rate, bitrate int) (*opusEncoder, error) {
	var err C.int
	e := C.opus_encoder_create(C.opus_int32(rate), 1, C.OPUS_APPLICATION_VOIP, &err)
	if err != C.OPUS_OK || e == nil {
		return nil, fmt.Errorf("opus encoder: %s", C.GoString(C.opus_strerror(err)))
	}
	// FEC with an assumed 5% loss: mobile data drops packets, and FEC lets
	// the phone rebuild a lost one from the next instead of a click.
	//
	// Off by default since passthrough (WAD_CALL_OPUS_FEC): at a fixed bitrate
	// FEC costs the main audio 0.4-0.6 PESQ, and packet duplication protects
	// against loss better without touching quality. See passthru.go.
	fec, loss := 0, 0
	if callAudio.fec {
		fec, loss = 1, 5
	}
	if r := C.wad_opus_enc_ctl(e, C.int(bitrate), C.int(fec), C.int(loss), 10); r != C.OPUS_OK {
		C.opus_encoder_destroy(e)
		return nil, fmt.Errorf("opus encoder ctl: %s", C.GoString(C.opus_strerror(r)))
	}
	return &opusEncoder{e: e}, nil
}

// Encode one frame (len(pcm) must be a valid Opus frame size at the rate).
func (o *opusEncoder) Encode(pcm []int16) ([]byte, error) {
	if len(pcm) == 0 {
		return nil, nil
	}
	out := make([]byte, 1275)
	n := C.opus_encode(o.e, (*C.opus_int16)(unsafe.Pointer(&pcm[0])), C.int(len(pcm)),
		(*C.uchar)(unsafe.Pointer(&out[0])), C.opus_int32(len(out)))
	if n < 0 {
		return nil, fmt.Errorf("opus encode: %s", C.GoString(C.opus_strerror(C.int(n))))
	}
	return out[:n], nil
}

func (o *opusEncoder) Close() {
	if o != nil && o.e != nil {
		C.opus_encoder_destroy(o.e)
		o.e = nil
	}
}

func newOpusDecoder(rate int) (*opusDecoder, error) {
	var err C.int
	d := C.opus_decoder_create(C.opus_int32(rate), 1, &err)
	if err != C.OPUS_OK || d == nil {
		return nil, fmt.Errorf("opus decoder: %s", C.GoString(C.opus_strerror(err)))
	}
	return &opusDecoder{d: d}, nil
}

// Decode one packet. pkt == nil means "this one was lost": libopus fills the
// gap by concealment (fec=0) or rebuilds it from the next packet (fec=1).
func (o *opusDecoder) Decode(pkt []byte, frame int, fec bool) ([]int16, error) {
	out := make([]int16, 5760) // 120 ms at 48 kHz: the largest Opus frame
	var p *C.uchar
	if len(pkt) > 0 {
		p = (*C.uchar)(unsafe.Pointer(&pkt[0]))
	}
	maxN := len(out)
	if pkt == nil || fec {
		maxN = frame
	}
	f := 0
	if fec {
		f = 1
	}
	n := C.opus_decode(o.d, p, C.opus_int32(len(pkt)),
		(*C.opus_int16)(unsafe.Pointer(&out[0])), C.int(maxN), C.int(f))
	if n < 0 {
		return nil, fmt.Errorf("opus decode: %s", C.GoString(C.opus_strerror(C.int(n))))
	}
	return out[:n], nil
}

func (o *opusDecoder) Close() {
	if o != nil && o.d != nil {
		C.opus_decoder_destroy(o.d)
		o.d = nil
	}
}
