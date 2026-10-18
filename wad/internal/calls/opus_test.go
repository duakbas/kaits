//go:build cgo && !noopus

package calls

import (
	"math"
	"testing"
)

// The point of Opus here: a 6 kHz component — sibilants, the top of the
// voice — survives. G.711 removes everything above ~3.4 kHz.
func TestOpusCarriesWideband(t *testing.T) {
	enc, err := newOpusEncoder(LegRate, opusBitrate)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	dec, err := newOpusDecoder(LegRate)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()

	tone := NewTone(6000, LegRate)
	var in, out float64
	for f := 0; f < 50; f++ {
		b := make([]int16, LegFrame)
		tone.Fill(b)
		pkt, err := enc.Encode(b)
		if err != nil {
			t.Fatal(err)
		}
		pcm, err := dec.Decode(pkt, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(pcm) != LegFrame {
			t.Fatalf("decoded %d samples, want %d", len(pcm), LegFrame)
		}
		if f < 10 {
			continue // encoder warm-up
		}
		for i := range b {
			in += float64(b[i]) * float64(b[i])
			out += float64(pcm[i]) * float64(pcm[i])
		}
	}
	db := 10 * math.Log10(out/in)
	if db < -3 {
		t.Errorf("6 kHz came through at %.1f dB; wideband should keep it", db)
	}
}

// A lost packet is concealed rather than dropped: the decoder still produces
// a frame of audio for it.
func TestOpusConcealsALostPacket(t *testing.T) {
	dec, err := newOpusDecoder(LegRate)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	pcm, err := dec.Decode(nil, LegFrame, false)
	if err != nil || len(pcm) != LegFrame {
		t.Fatalf("PLC gave %d samples, err %v", len(pcm), err)
	}
}
