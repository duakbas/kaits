package calls

import (
	"io"
	"testing"
	"time"

	"github.com/purpshell/meowcaller"
)

// The two sides of the bridge disagree about size and timing: a WebRTC track
// delivers what it delivers when it arrives, and meowcaller asks for exactly
// 960 samples at a time. Re-cutting is the whole job.
func TestPCMBridgeRecutsIntoFrames(t *testing.T) {
	b := newPCMBridge()

	// Two and a half frames written in awkward pieces.
	total := meowcaller.FrameSamples*2 + meowcaller.FrameSamples/2
	written := make([]float32, total)
	for i := range written {
		written[i] = float32(i%100) / 100
	}
	for off := 0; off < total; off += 137 { // deliberately not a frame multiple
		end := off + 137
		if end > total {
			end = total
		}
		if err := b.Write(written[off:end]); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	var got []float32
	for i := 0; i < 2; i++ {
		frame, err := b.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if len(frame) != meowcaller.FrameSamples {
			t.Fatalf("frame %d is %d samples, want %d", i, len(frame), meowcaller.FrameSamples)
		}
		got = append(got, frame...)
	}
	for i := range got {
		if got[i] != written[i] {
			t.Fatalf("sample %d = %v, want %v — the stream was reordered or dropped",
				i, got[i], written[i])
		}
	}
}

// A gap in what the phone sends must be heard as a gap. Blocking would stall
// the far end's audio entirely, which is a worse failure than a moment of
// silence and much harder to diagnose.
func TestPCMBridgeFillsSilenceOnUnderrun(t *testing.T) {
	b := newPCMBridge()

	start := time.Now()
	frame, err := b.ReadFrame()
	if err != nil {
		t.Fatalf("underrun returned an error: %v", err)
	}
	if len(frame) != meowcaller.FrameSamples {
		t.Errorf("silent frame is %d samples, want %d", len(frame), meowcaller.FrameSamples)
	}
	for i, v := range frame {
		if v != 0 {
			t.Fatalf("sample %d of the silence is %v", i, v)
		}
	}
	// It has to give up quickly. A frame is 60ms of audio; waiting seconds for
	// one would be the stall this exists to avoid.
	if el := time.Since(start); el > time.Second {
		t.Errorf("waited %v for a frame that never came", el)
	}
}

// Audio that is a second late is not worth sending: on a live call the far end
// wants the present. An unbounded buffer turns a briefly slow phone into
// permanently growing delay.
func TestPCMBridgeDropsRatherThanLags(t *testing.T) {
	b := newPCMBridge()
	for i := 0; i < 10; i++ {
		if err := b.Write(make([]float32, meowcaller.SampleRate/2)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	b.mu.Lock()
	buffered := len(b.buf)
	b.mu.Unlock()
	if buffered > meowcaller.SampleRate {
		t.Errorf("buffered %d samples (%.1fs); the cap should have discarded the oldest",
			buffered, float64(buffered)/meowcaller.SampleRate)
	}
}

// After close, a reader drains what is left and then stops, rather than
// producing silence for ever into a call that has ended.
func TestPCMBridgeEndsAfterClose(t *testing.T) {
	b := newPCMBridge()
	b.Write(make([]float32, meowcaller.FrameSamples))
	b.Close()

	if _, err := b.ReadFrame(); err != nil {
		t.Fatalf("the buffered frame was lost on close: %v", err)
	}
	if _, err := b.ReadFrame(); err != io.EOF {
		t.Errorf("after close and drain, err = %v, want EOF", err)
	}
	if err := b.Write(make([]float32, 10)); err == nil {
		t.Error("writing to a closed bridge was accepted")
	}
	b.Close() // must be safe twice
}
