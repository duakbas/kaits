package calls

import (
	"context"
	"log"
	"sync"
	"time"

	"wad/internal/ws"
)

// Step 4: joining the two legs.
//
//	WhatsApp  <--MLow/SRTP-->  meowcaller  <--16k PCM-->  wad  <--G.711-->  phone
//
// With the phone leg built and the resampler tested, this is what the plan
// said it would be: a buffer and a loop. The buffer is the interesting half.
//
// WHY A PACER RATHER THAN A DIRECT FORWARD
//
// meowcaller delivers 60 ms frames; RTP wants a packet every 20 ms. Forwarding
// each arrival straight through would send three packets back to back and then
// nothing for 60 ms, which a jitter buffer can absorb but should not have to,
// and which makes every real network hiccup look three times worse than it is.
// Worse, the two sides are clocked by different things — meowcaller by the
// relay, us by a local ticker — and they drift. Feeding a queue from one and
// draining it on the other's clock is what absorbs that.
//
// Underrun fills with silence rather than blocking. On a live call the far end
// needs a steady stream, and a gap in what arrives should be heard as a gap
// rather than as the call seizing up. Overrun drops the OLDEST audio, because
// a second-old packet is worth nothing to a conversation: better a brief
// glitch now than permanently growing delay.

// jitter is a bounded queue of 8 kHz samples that hands out fixed-size frames.
type jitter struct {
	mu  sync.Mutex
	buf []int16
	max int
}

func newJitter(max int) *jitter { return &jitter{max: max} }

func (j *jitter) push(s []int16) {
	if len(s) == 0 {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.buf = append(j.buf, s...)
	if len(j.buf) > j.max {
		// Keep the newest. See the note above about why old audio is worthless.
		j.buf = j.buf[len(j.buf)-j.max:]
	}
}

// take returns exactly n samples, padded with silence if that is all there is.
// The second return says whether any real audio was in it, which is what the
// "nothing is arriving" diagnostics are built on.
func (j *jitter) take(n int) ([]int16, bool) {
	out := make([]int16, n)
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.buf) == 0 {
		return out, false
	}
	c := copy(out, j.buf)
	j.buf = j.buf[c:]
	return out, true
}

func (j *jitter) depth() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.buf)
}

// answerRealCall answers a WhatsApp call and bridges it to the phone.
//
// UNVERIFIED AGAINST A LIVE CALL. Every piece below is tested on its own — the
// resampler, the codec, the phone leg against a second pion peer, the queue —
// but the three have never run together with a real account at one end,
// because doing that is the step with the risk that cannot be undone. Run the
// tone test first (`/debug/call`); it proves everything here except meowcaller.
func (m *Manager) answerRealCall(ctx context.Context, callID string) error {
	down := NewDown2()
	up := NewUp2()
	// One second. Past that the audio is no longer part of the conversation.
	q := newJitter(PhoneRate)

	// Ask the backend BEFORE building the phone leg. It is the cheaper of the
	// two to fail, and audio that arrives before the leg exists simply lands
	// in the queue — which is what the queue is for.
	src, err := m.be.Answer(ctx, callID, func(pcm []float32) {
		q.push(down.Process(pcm))
	})
	if err != nil {
		return err
	}

	leg, err := m.startPhoneLeg(callID)
	if err != nil {
		_ = src.Close()
		return err
	}

	// The phone's microphone, upsampled and handed to WhatsApp. pcmBridge on
	// the far side re-cuts it into the 960-sample frames meowcaller wants, so
	// nothing here has to care about frame sizes.
	leg.OnPCM(func(pcm []int16) {
		if err := src.Write(up.Process(pcm)); err != nil {
			log.Printf("calls: %s: writing to WhatsApp: %v", callID, err)
		}
	})

	stop := make(chan struct{})
	m.mu.Lock()
	m.stopPump = stop
	m.source = src
	m.mu.Unlock()

	go m.pump(callID, leg, q, stop)

	m.hub.PushT(ws.TCallState, map[string]any{"callid": callID, "state": "accepted"})
	return nil
}

// pump drains the queue into the phone on a steady 20 ms clock.
func (m *Manager) pump(callID string, leg *PhoneLeg, q *jitter, stop chan struct{}) {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	report := time.NewTicker(5 * time.Second)
	defer report.Stop()

	var frames, silent int
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			frame, real := q.take(PhoneFrame)
			frames++
			if !real {
				silent++
			}
			if err := leg.WritePCM(frame); err != nil {
				log.Printf("calls: %s: writing to the phone: %v", callID, err)
				return
			}
		case <-report.C:
			// Depth is the number worth watching: steadily growing means the
			// two clocks disagree and delay is accumulating; permanently zero
			// means nothing is arriving from WhatsApp at all.
			log.Printf("calls: %s: %d frames, %d starved, queue %d samples",
				callID, frames, silent, q.depth())
			frames, silent = 0, 0
		}
	}
}
