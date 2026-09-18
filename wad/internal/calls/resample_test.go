package calls

import (
	"math"
	"testing"
)

func sine16(freq float64, rate int, n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(0.5 * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
	}
	return out
}

// rms of the back half, so the filter's settling transient is excluded.
func rms16(v []int16) float64 {
	if len(v) == 0 {
		return 0
	}
	v = v[len(v)/2:]
	var acc float64
	for _, s := range v {
		x := float64(s) / 32768
		acc += x * x
	}
	return math.Sqrt(acc / float64(len(v)))
}

func rmsF(v []float32) float64 {
	if len(v) == 0 {
		return 0
	}
	v = v[len(v)/2:]
	var acc float64
	for _, s := range v {
		acc += float64(s) * float64(s)
	}
	return math.Sqrt(acc / float64(len(v)))
}

// THE test. A 6 kHz tone in a 16 kHz signal has nowhere to live at 8 kHz, and
// a decimator without a filter does not drop it — it folds it down to 2 kHz,
// into the middle of speech, as a metallic warble that tracks the voice and
// cannot be removed afterwards.
//
// A "take every other sample" implementation passes every other test in this
// file and fails this one by about 40 dB.
func TestDownsamplingRejectsWhatWouldAlias(t *testing.T) {
	const rate = 16000
	n := rate // one second

	pass := NewDown2().Process(sine16(1000, rate, n)) // well inside the band
	fold := NewDown2().Process(sine16(6000, rate, n)) // would alias to 2 kHz

	pr, fr := rms16(pass), rms16(fold)
	if pr < 0.2 {
		t.Fatalf("a 1 kHz tone came through at rms %.4f; the passband is broken", pr)
	}
	// 6 kHz is a full octave-and-a-half into the stopband; anything less than
	// 30 dB down would be plainly audible.
	ratio := 20 * math.Log10(fr/pr)
	if ratio > -30 {
		t.Errorf("6 kHz was only %.1f dB below the passband (rms %.5f vs %.5f); "+
			"it will alias to 2 kHz and be heard", ratio, fr, pr)
	}
}

// The same in reverse: zero-stuffing creates an image above 4 kHz, and if it
// is not filtered away the far end gets a harsh bright copy mixed in.
func TestUpsamplingSuppressesTheImage(t *testing.T) {
	const inRate = 8000
	n := inRate

	// A 1 kHz tone at 8 kHz.
	in := make([]int16, n)
	for i := range in {
		in[i] = int16(16000 * math.Sin(2*math.Pi*1000*float64(i)/float64(inRate)))
	}
	out := NewUp2().Process(in)

	// Measure energy above 4 kHz in the 16 kHz output by correlating against
	// the image frequency the process would produce: 8000-1000 = 7 kHz.
	image := goertzel(out, 7000, 16000)
	fund := goertzel(out, 1000, 16000)
	if fund <= 0 {
		t.Fatal("the fundamental did not survive upsampling")
	}
	ratio := 20 * math.Log10(image/fund)
	if ratio > -30 {
		t.Errorf("the 7 kHz image is only %.1f dB below the tone; "+
			"the reconstruction filter is not working", ratio)
	}
}

// goertzel measures the magnitude at one frequency — enough to answer "is this
// component present" without pulling in an FFT.
func goertzel(x []float32, freq float64, rate int) float64 {
	k := 2 * math.Cos(2*math.Pi*freq/float64(rate))
	var s0, s1, s2 float64
	for _, v := range x {
		s0 = float64(v) + k*s1 - s2
		s2, s1 = s1, s0
	}
	return math.Sqrt(s1*s1 + s2*s2 - k*s1*s2)
}

// Level must survive the trip. A converter with the wrong gain makes every
// call quiet or clipped, and the interpolation x2 is exactly the sort of
// factor that goes missing.
func TestLevelIsPreservedInBothDirections(t *testing.T) {
	const rate = 16000
	in := sine16(800, rate, rate)
	down := NewDown2().Process(in)

	inRMS := rmsF(in)
	downRMS := rms16(down)
	if d := 20 * math.Log10(downRMS/inRMS); d > 1.5 || d < -1.5 {
		t.Errorf("downsampling changed the level by %.2f dB", d)
	}

	up := NewUp2().Process(down)
	upRMS := rmsF(up)
	if d := 20 * math.Log10(upRMS/downRMS); d > 1.5 || d < -1.5 {
		t.Errorf("upsampling changed the level by %.2f dB "+
			"(the interpolation gain is probably missing)", d)
	}
}

// Frames are processed one at a time in the real thing, 50 or 16 times a
// second. A converter that restarts its filter at each boundary puts a step
// discontinuity there, and the resulting buzz is indistinguishable from packet
// loss — it would send someone hunting the network instead of this file.
func TestFilterStateCarriesAcrossFrames(t *testing.T) {
	const rate = 16000
	whole := sine16(800, rate, 960*4)

	atOnce := NewDown2().Process(whole)

	inChunks := []int16{}
	d := NewDown2()
	for i := 0; i+960 <= len(whole); i += 960 {
		inChunks = append(inChunks, d.Process(whole[i:i+960])...)
	}

	if len(atOnce) != len(inChunks) {
		t.Fatalf("chunked gave %d samples, whole gave %d", len(inChunks), len(atOnce))
	}
	for i := range atOnce {
		if atOnce[i] != inChunks[i] {
			t.Fatalf("sample %d differs (%d vs %d): the filter is being restarted "+
				"at frame boundaries", i, atOnce[i], inChunks[i])
		}
	}

	// And the same for the other direction.
	src := make([]int16, 160*8)
	for i := range src {
		src[i] = int16(12000 * math.Sin(2*math.Pi*700*float64(i)/8000))
	}
	upWhole := NewUp2().Process(src)
	u := NewUp2()
	upChunks := []float32{}
	for i := 0; i+160 <= len(src); i += 160 {
		upChunks = append(upChunks, u.Process(src[i:i+160])...)
	}
	for i := range upWhole {
		if upWhole[i] != upChunks[i] {
			t.Fatalf("upsample sample %d differs: state is not carried", i)
		}
	}
}

// The arithmetic the two legs depend on: 60 ms at 16 kHz is 960 samples and
// becomes three 20 ms frames of 160 at 8 kHz.
func TestFrameSizesLineUpBetweenTheLegs(t *testing.T) {
	d := NewDown2()
	got := d.Process(make([]float32, 960))
	if len(got) != 480 {
		t.Errorf("960 samples at 16k became %d at 8k, want 480", len(got))
	}
	if 480 != 3*PhoneFrame {
		t.Errorf("480 is not three phone frames of %d", PhoneFrame)
	}

	u := NewUp2()
	up := u.Process(make([]int16, PhoneFrame))
	if len(up) != 2*PhoneFrame {
		t.Errorf("%d samples at 8k became %d at 16k, want %d",
			PhoneFrame, len(up), 2*PhoneFrame)
	}

	// Odd lengths must not lose or duplicate samples across calls.
	d2 := NewDown2()
	total := 0
	for i := 0; i < 10; i++ {
		total += len(d2.Process(make([]float32, 161)))
	}
	if total != 1610/2 {
		t.Errorf("ten 161-sample frames produced %d output samples, want %d",
			total, 1610/2)
	}
}

// Silence in, silence out — no DC offset, no ringing that never settles.
func TestSilenceStaysSilentThroughConversion(t *testing.T) {
	down := NewDown2().Process(make([]float32, 960))
	for i, s := range down {
		if s != 0 {
			t.Fatalf("downsampled silence has sample %d = %d", i, s)
		}
	}
	up := NewUp2().Process(make([]int16, 160))
	for i, s := range up {
		if s != 0 {
			t.Fatalf("upsampled silence has sample %d = %v", i, s)
		}
	}
}

// Loud input must clip rather than wrap. float32 from meowcaller is nominally
// -1..1 but nothing enforces that, and an int16 that wraps turns a shout into
// full-scale noise of the opposite sign.
func TestOverloadClipsRatherThanWrapping(t *testing.T) {
	in := make([]float32, 960)
	for i := range in {
		in[i] = 4.0 // four times full scale
	}
	out := NewDown2().Process(in)

	// Once the filter has settled, four times full scale has to sit pinned at
	// the top rather than having wrapped round to the bottom.
	//
	// Only the settled part is asserted. The leading samples are the filter's
	// step response, and a windowed sinc rings below zero before it arrives —
	// scaled here by the 4x overload, so the ringing is large in absolute
	// terms and still entirely correct. Asserting on it would be asserting on
	// the side-lobe amplitude of the window, which is not what this test is
	// about.
	for i := resampTaps; i < len(out); i++ {
		if out[i] != 32767 {
			t.Fatalf("sample %d is %d after settling, want it clipped at 32767", i, out[i])
		}
	}
}

// The clipping itself, tested where it lives rather than through a filter.
// float32 from meowcaller is nominally -1..1 and nothing enforces it; an int16
// conversion that wraps turns a shout into full-scale noise of the opposite
// sign, which is the loudest possible way for this to fail.
func TestClampSaturatesInsteadOfWrapping(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want int16
	}{
		{0, 0},
		{1000, 1000},
		{-1000, -1000},
		{32767, 32767},
		{-32768, -32768},
		{32768, 32767},       // one past the top
		{-32769, -32768},     // one past the bottom
		{4 * 32767, 32767},   // a serious overload
		{-4 * 32767, -32768}, // and the other way
		{1e9, 32767},
		{-1e9, -32768},
	} {
		if got := clamp16(tc.in); got != tc.want {
			t.Errorf("clamp16(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
