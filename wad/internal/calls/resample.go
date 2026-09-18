package calls

import "math"

// Rate conversion between the two legs.
//
//	WhatsApp (meowcaller)   16 kHz mono float32, 960-sample frames (60 ms)
//	phone (WebRTC/G.711)     8 kHz mono int16,   160-sample frames (20 ms)
//
// Exactly 2:1, and 60 ms is exactly three 20 ms frames, so the arithmetic is
// clean in both directions. What is not clean is doing it naively.
//
// WHY THERE IS A FILTER HERE AT ALL
//
// Halving the rate by taking every other sample is the obvious implementation
// and it is wrong in a way that is loud. Anything above 4 kHz in the 16 kHz
// signal has nowhere to go in an 8 kHz one, and instead of disappearing it
// FOLDS: a 6 kHz component comes back as 2 kHz, right in the middle of speech,
// as a warbling metallic tone that follows the voice around. It cannot be
// removed afterwards, because by then it is indistinguishable from signal. So
// the energy above 4 kHz has to be taken out BEFORE the decimation.
//
// The same applies in reverse. Stuffing a zero between every pair of samples
// produces a spectral image above 4 kHz that has to be filtered away, or the
// far end receives a bright, harsh copy of the voice mixed with the real one.
//
// The filter is a windowed sinc: linear phase (so it delays the signal without
// smearing it), 33 taps, which at these rates is a few microseconds of CPU per
// frame and far cheaper than the alternative of explaining the noise later.
//
// STATE IS THE OTHER HALF
//
// Both converters keep the tail of the previous frame. A filter restarted at
// every frame boundary sees a step from silence into the signal 50 times a
// second, and that buzz is indistinguishable from packet loss — the same class
// of bug as a tone generator that resets its phase, and just as misleading.

// resampTaps is the length of the half-band low-pass. Odd, so there is a
// centre tap and the delay is a whole number of samples.
const resampTaps = 33

// lowpass builds a windowed-sinc low-pass with cutoff fc, expressed as a
// fraction of the sample rate it runs at. Hamming window: the stopband
// rejection is around 53 dB, which is far more than telephone speech needs and
// costs nothing extra.
func lowpass(n int, fc float64) []float64 {
	h := make([]float64, n)
	mid := float64(n-1) / 2
	var sum float64
	for i := range h {
		x := float64(i) - mid
		var s float64
		if x == 0 {
			s = 2 * fc
		} else {
			s = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		// Hamming.
		w := 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/float64(n-1))
		h[i] = s * w
		sum += h[i]
	}
	// Unity gain at DC, so a constant signal comes through unchanged and the
	// conversion neither quietens nor amplifies the call.
	for i := range h {
		h[i] /= sum
	}
	return h
}

// Down2 converts 16 kHz float32 to 8 kHz int16.
type Down2 struct {
	taps []float64
	hist []float64 // a sliding window: the current sample and the taps-1 before it
	odd  bool      // whether the next input sample is the one we keep
}

func NewDown2() *Down2 {
	return &Down2{
		// Cutoff at a quarter of the INPUT rate, which is the Nyquist
		// frequency of the output rate: 4 kHz at 16 kHz in.
		taps: lowpass(resampTaps, 0.25),
		hist: make([]float64, resampTaps),
	}
}

// Process filters and decimates. Input of any length is accepted; the output
// is half as long, give or take the odd sample carried into the next call.
//
// float32 in is meowcaller's format, nominally -1..1; int16 out is what G.711
// encodes from.
func (d *Down2) Process(in []float32) []int16 {
	out := make([]int16, 0, len(in)/2+1)
	for _, s := range in {
		d.hist = append(d.hist[1:], float64(s))
		if d.odd {
			out = append(out, clamp16(d.convolve()*32767))
		}
		d.odd = !d.odd
	}
	return out
}

func (d *Down2) convolve() float64 {
	var acc float64
	for i, t := range d.taps {
		acc += t * d.hist[i]
	}
	return acc
}

// Up2 converts 8 kHz int16 to 16 kHz float32.
type Up2 struct {
	taps []float64
	hist []float64
}

func NewUp2() *Up2 {
	return &Up2{
		// Cutoff at a quarter of the OUTPUT rate — the same 4 kHz, expressed
		// in the 16 kHz domain the filter now runs in.
		taps: lowpass(resampTaps, 0.25),
		hist: make([]float64, resampTaps),
	}
}

// Process zero-stuffs and filters. Output is twice as long as the input.
func (u *Up2) Process(in []int16) []float32 {
	out := make([]float32, 0, len(in)*2)
	for _, s := range in {
		// The real sample, then the zero between it and the next.
		//
		// The x2 is the interpolation gain: half the samples are zeros, so the
		// filtered result has half the energy it should. Without it the call
		// arrives 6 dB quiet.
		u.hist = append(u.hist[1:], float64(s)/32768)
		out = append(out, float32(u.convolve()*2))

		u.hist = append(u.hist[1:], 0)
		out = append(out, float32(u.convolve()*2))
	}
	return out
}

func (u *Up2) convolve() float64 {
	var acc float64
	for i, t := range u.taps {
		acc += t * u.hist[i]
	}
	return acc
}

func clamp16(v float64) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}
