package calls

import "math"

// A generated tone, which is the whole of the stage-3 test.
//
// The point of playing a tone rather than forwarding WhatsApp audio is that it
// separates two failures that otherwise arrive together: "WebRTC to a 2016
// browser engine does not work" and "the WhatsApp call library does not work".
// The tone needs no account, rings nothing at Meta, and can be run as often as
// you like — which matters, because the account risk is the one thing in this
// feature that cannot be undone.

// Tone is a sine generator that keeps its phase across calls, so consecutive
// frames join without a click. A discontinuity every 20 ms is itself an
// audible buzz, and one that would look exactly like packet loss.
type Tone struct {
	step  float64 // radians per sample
	phase float64
	amp   float64
}

// NewTone makes a generator at the given frequency and sample rate.
func NewTone(freqHz float64, rate int) *Tone {
	return &Tone{
		step: 2 * math.Pi * freqHz / float64(rate),
		// A quarter of full scale. Loud enough to be unmistakable on a phone
		// speaker, quiet enough not to clip through µ-law's coarse top end.
		amp: 8000,
	}
}

// Fill writes the next len(buf) samples.
func (t *Tone) Fill(buf []int16) {
	for i := range buf {
		buf[i] = int16(t.amp * math.Sin(t.phase))
		t.phase += t.step
		// Wrap rather than growing without bound: a float64 phase accumulating
		// for an hour loses precision, and the tone would drift sharp.
		if t.phase > 2*math.Pi {
			t.phase -= 2 * math.Pi
		}
	}
}

// Warble alternates between two tones, which is a much better test signal than
// a steady one.
//
// A continuous tone tells you only that something arrived. A warble tells you
// the stream is CONTINUOUS: every dropout, every buffer stall and every clock
// mismatch turns into an obviously wrong rhythm, and you can hear it from
// across the room without instrumenting anything. It is also unmistakably ours
// — a steady tone could be the network, the phone, or a stuck codec.
type Warble struct {
	lo, hi   *Tone
	rate     int
	halfLen  int
	position int
}

// NewWarble alternates between two frequencies every halfMS milliseconds.
func NewWarble(loHz, hiHz float64, halfMS int, rate int) *Warble {
	return &Warble{
		lo:      NewTone(loHz, rate),
		hi:      NewTone(hiHz, rate),
		rate:    rate,
		halfLen: rate * halfMS / 1000,
	}
}

// Fill writes the next len(buf) samples, switching tone at each boundary.
func (w *Warble) Fill(buf []int16) {
	for i := 0; i < len(buf); {
		src := w.lo
		if (w.position/w.halfLen)%2 == 1 {
			src = w.hi
		}
		// How many samples until the next switch.
		room := w.halfLen - (w.position % w.halfLen)
		n := len(buf) - i
		if n > room {
			n = room
		}
		src.Fill(buf[i : i+n])
		i += n
		w.position += n
		// Keep the position from growing forever; two halves is the period.
		if w.position >= 2*w.halfLen {
			w.position -= 2 * w.halfLen
		}
	}
}
