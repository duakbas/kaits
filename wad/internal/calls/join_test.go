package calls

import "testing"

// The queue hands out exactly the frame size asked for, whatever it was fed.
// meowcaller pushes 60 ms at a time and RTP pulls 20 ms at a time; if the
// sizes did not decouple here they would have to match, and they cannot.
func TestJitterRecutsIntoFrames(t *testing.T) {
	q := newJitter(PhoneRate)
	// One 60 ms block, as it arrives from the far end after downsampling.
	block := make([]int16, 3*PhoneFrame)
	for i := range block {
		block[i] = int16(i + 1)
	}
	q.push(block)

	for f := 0; f < 3; f++ {
		frame, real := q.take(PhoneFrame)
		if !real {
			t.Fatalf("frame %d reported as starved with audio queued", f)
		}
		if len(frame) != PhoneFrame {
			t.Fatalf("frame %d is %d samples, want %d", f, len(frame), PhoneFrame)
		}
		for i, s := range frame {
			want := int16(f*PhoneFrame + i + 1)
			if s != want {
				t.Fatalf("frame %d sample %d = %d, want %d", f, i, s, want)
			}
		}
	}
	if q.depth() != 0 {
		t.Errorf("%d samples left after taking all three frames", q.depth())
	}
}

// Underrun fills with silence rather than blocking or returning short. The far
// end needs a steady stream; a gap in what arrives should be heard as a gap,
// not as the call seizing up, and a short frame would desynchronise the RTP
// timestamps for the rest of the call.
func TestJitterFillsSilenceWhenStarved(t *testing.T) {
	q := newJitter(PhoneRate)

	frame, real := q.take(PhoneFrame)
	if len(frame) != PhoneFrame {
		t.Fatalf("starved frame is %d samples, want %d", len(frame), PhoneFrame)
	}
	if real {
		t.Error("an empty queue reported that it returned real audio")
	}
	for i, s := range frame {
		if s != 0 {
			t.Fatalf("starved frame sample %d = %d, want silence", i, s)
		}
	}

	// A partial frame is padded, not truncated.
	q.push(make([]int16, 40))
	frame, real = q.take(PhoneFrame)
	if len(frame) != PhoneFrame {
		t.Fatalf("partial frame is %d samples, want %d", len(frame), PhoneFrame)
	}
	if !real {
		t.Error("a partly-filled frame reported as fully starved")
	}
}

// Overrun drops the OLDEST audio. A phone or a network that falls behind must
// cost a glitch, not permanently growing delay: audio a second old is worth
// nothing to a conversation, and an unbounded queue turns a brief stall into a
// call that is minutes behind by the end.
func TestJitterDropsTheOldestWhenItOverflows(t *testing.T) {
	const max = 400
	q := newJitter(max)

	// Push well past the limit, with values that say where each sample came
	// from.
	for block := 0; block < 10; block++ {
		s := make([]int16, 100)
		for i := range s {
			s[i] = int16(block*100 + i)
		}
		q.push(s)
	}

	if d := q.depth(); d > max {
		t.Fatalf("queue grew to %d samples, past its %d limit", d, max)
	}

	// What survives must be the END of the stream, not the beginning.
	frame, _ := q.take(1)
	oldest := frame[0]
	firstKept := int16(1000 - max) // ten blocks of 100, keeping the last max
	if oldest != firstKept {
		t.Errorf("oldest surviving sample is %d, want %d — the queue is "+
			"discarding new audio instead of stale audio", oldest, firstKept)
	}
}

// Pushing nothing is a no-op rather than a way to corrupt the buffer.
func TestJitterIgnoresEmptyPushes(t *testing.T) {
	q := newJitter(PhoneRate)
	q.push(nil)
	q.push([]int16{})
	if q.depth() != 0 {
		t.Errorf("empty pushes left %d samples", q.depth())
	}
	q.push([]int16{1, 2, 3})
	if q.depth() != 3 {
		t.Errorf("depth = %d after pushing 3", q.depth())
	}
}

// The arithmetic the pump depends on: one 60 ms frame from WhatsApp becomes
// exactly three 20 ms frames at the phone, so a steady stream in is a steady
// stream out with no residue accumulating.
func TestOneWhatsAppFrameIsExactlyThreePhoneFrames(t *testing.T) {
	down := NewDown2()
	q := newJitter(PhoneRate)

	// Ten 960-sample frames in, thirty 160-sample frames out, nothing left.
	for i := 0; i < 10; i++ {
		q.push(down.Process(make([]float32, 960)))
	}
	for i := 0; i < 30; i++ {
		if _, real := q.take(PhoneFrame); !real {
			t.Fatalf("starved at phone frame %d; ten WhatsApp frames should "+
				"fill exactly thirty", i)
		}
	}
	if q.depth() != 0 {
		t.Errorf("%d samples left over — the frame sizes do not divide evenly", q.depth())
	}
}
