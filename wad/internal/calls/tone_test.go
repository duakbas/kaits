package calls

import "testing"

// Frequency, measured the way a listener would judge it: by counting cycles.
func TestToneRunsAtTheRequestedFrequency(t *testing.T) {
	for _, freq := range []float64{440, 1000, 300} {
		tone := NewTone(freq, PhoneRate)
		buf := make([]int16, PhoneRate) // exactly one second
		tone.Fill(buf)

		crossings := 0
		for i := 1; i < len(buf); i++ {
			if buf[i-1] < 0 && buf[i] >= 0 {
				crossings++
			}
		}
		// One rising zero crossing per cycle, so crossings ≈ frequency.
		if d := float64(crossings) - freq; d > 2 || d < -2 {
			t.Errorf("%.0f Hz tone produced %d cycles in a second", freq, crossings)
		}
	}
}

// Phase has to carry across calls. Fill is called once per 20 ms packet, and a
// generator that restarted each time would put a step discontinuity at every
// packet boundary — an audible buzz at 50 Hz that looks exactly like packet
// loss and would send someone hunting the network instead of this file.
func TestToneDoesNotClickBetweenFrames(t *testing.T) {
	tone := NewTone(440, PhoneRate)
	prev := make([]int16, PhoneFrame)
	tone.Fill(prev)

	// The most a 440 Hz sine at this amplitude can move between two adjacent
	// samples, with room to spare.
	const maxStep = 8000 * 2 * 3.1416 * 440 / PhoneRate * 1.5

	for frame := 0; frame < 50; frame++ {
		next := make([]int16, PhoneFrame)
		tone.Fill(next)
		step := int(next[0]) - int(prev[len(prev)-1])
		if step < 0 {
			step = -step
		}
		if float64(step) > maxStep {
			t.Fatalf("frame %d starts %d away from where the last one ended; "+
				"the generator is restarting its phase", frame, step)
		}
		prev = next
	}
}

// Amplitude has to stay inside what µ-law represents well. Clipping at the top
// of the curve is where µ-law is coarsest, so a too-loud tone comes back
// buzzing and gets blamed on the codec.
func TestToneStaysBelowClipping(t *testing.T) {
	tone := NewTone(440, PhoneRate)
	buf := make([]int16, PhoneRate)
	tone.Fill(buf)
	var peak int32
	for _, s := range buf {
		v := int32(s)
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	if peak > 12000 {
		t.Errorf("peak amplitude %d is loud enough to distort through µ-law", peak)
	}
	if peak < 4000 {
		t.Errorf("peak amplitude %d is too quiet to hear on a phone speaker", peak)
	}
}

// The warble is the actual test signal, and its value is entirely in the
// rhythm being right: a dropout shows up as a wrong-length half, audible
// across the room with nothing instrumented.
func TestWarbleAlternatesOnSchedule(t *testing.T) {
	const halfMS = 500
	w := NewWarble(440, 880, halfMS, PhoneRate)

	// Two seconds, gathered 20 ms at a time the way it is really used.
	total := PhoneRate * 2
	all := make([]int16, 0, total)
	for len(all) < total {
		buf := make([]int16, PhoneFrame)
		w.Fill(buf)
		all = append(all, buf...)
	}

	// Count cycles in each half-second window; they should alternate between
	// roughly 220 (440 Hz for half a second) and 440 (880 Hz).
	win := PhoneRate * halfMS / 1000
	var counts []int
	for start := 0; start+win <= len(all); start += win {
		c := 0
		for i := start + 1; i < start+win; i++ {
			if all[i-1] < 0 && all[i] >= 0 {
				c++
			}
		}
		counts = append(counts, c)
	}
	if len(counts) < 4 {
		t.Fatalf("only %d windows to check", len(counts))
	}
	for i, c := range counts {
		want := 220
		if i%2 == 1 {
			want = 440
		}
		if d := c - want; d > 3 || d < -3 {
			t.Errorf("window %d has %d cycles, want about %d — the warble is "+
				"not switching on schedule", i, c, want)
		}
	}
}

// Fill must handle a buffer that spans a switch, because 20 ms frames do not
// divide evenly into every half-length someone might choose.
func TestWarbleHandlesASwitchInsideOneFrame(t *testing.T) {
	// 30 ms halves against 20 ms frames: every frame straddles a boundary.
	w := NewWarble(440, 880, 30, PhoneRate)
	for i := 0; i < 100; i++ {
		buf := make([]int16, PhoneFrame)
		w.Fill(buf) // must not panic or loop forever
	}
}
