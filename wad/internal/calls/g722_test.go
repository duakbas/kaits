//go:build cgo && !nog722

package calls

import (
	"math"
	"testing"
)

// G.722 keeps a 6 kHz tone that G.711 would cut — the reason it's offered.
func TestG722CarriesWideband(t *testing.T) {
	enc, err := newG722Encoder()
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	dec, err := newG722Decoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	tone := NewTone(6000, LegRate)
	var in, out float64
	for f := 0; f < 50; f++ {
		b := make([]int16, LegFrame)
		tone.Fill(b)
		pkt := enc.Encode(b)
		if len(pkt) != LegFrame/2 {
			t.Fatalf("20 ms encoded to %d bytes, want %d", len(pkt), LegFrame/2)
		}
		pcm := dec.Decode(pkt)
		if len(pcm) != LegFrame {
			t.Fatalf("decoded %d samples, want %d", len(pcm), LegFrame)
		}
		if f < 5 {
			continue
		}
		for i := range b {
			in += float64(b[i]) * float64(b[i])
			out += float64(pcm[i]) * float64(pcm[i])
		}
	}
	if db := 10 * math.Log10(out/in); db < -3 {
		t.Errorf("6 kHz came through at %.1f dB", db)
	}
}
