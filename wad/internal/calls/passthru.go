package calls

// Passthrough: WhatsApp's audio goes to the phone as it arrives, stamped with
// WhatsApp's own RTP timestamps, instead of being re-clocked through a queue.
//
// WHY
//
// The queue-and-pump design (join.go) turns every late or missing WhatsApp
// frame into 20 ms of hard zeros, and stamps those zeros with perfectly
// continuous RTP timestamps. The phone's WebRTC stack has a proper adaptive
// jitter buffer — reordering, time-stretching, Opus concealment — but it is
// handed a flawless-looking stream that happens to contain silence, so it
// never gets to do any of that. The zero edges are the pops.
//
// With the timestamps passed through, a late frame arrives late with the
// right timestamp and the phone's buffer stretches to cover it; a lost frame
// is a gap in timestamps and sequence numbers, which the phone conceals.
// There is no queue in wad, so there is also no startup backlog to trim.
//
// It needs meowcaller to hand over the timestamp, which upstream does not:
// see the meowcaller section of deploy/patch-vendor.sh. Without the patch the
// timestamps are synthesised from the frame count (logged once), which is
// still better than the queue — no zero-fill — but loses gap information.
//
// REDUNDANCY
//
// The hop to the phone is mobile data, and it loses packets too. Each packet
// is sent twice, the copy dupDelay later: same sequence number, so the phone's
// SRTP replay window drops whichever copy arrives second. Unlike Opus FEC it
// costs no audio quality — FEC spends part of the bitrate on a low-quality
// copy of the previous frame — only bandwidth, and it works for every codec.
//
// Measured with tools/call_sim.py (same network traces, 5 calls averaged):
// glitches/min on a "typical" mobile network went from 50 (queue) to 15
// (passthrough + duplication), and Opus at 64 kbit/s without FEC scored
// PESQ 4.33 against 2.83 for the old 32 kbit/s with FEC.

import (
	"context"
	"log"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"wad/internal/ws"
)

// callAudioConfig is read once from the environment. Every switch here exists
// so a misbehaving phone can be put back on the old behaviour with a service
// override and a restart, no rebuild.
type callAudioConfig struct {
	passthru bool          // WAD_CALL_PASSTHRU   (default 1) — 0 = old queue + pump
	opusKbps int           // WAD_CALL_OPUS_KBPS  (default 64)
	fec      bool          // WAD_CALL_OPUS_FEC   (default 0)
	dup      time.Duration // WAD_CALL_DUP_MS     (default 40; 0 = off)
}

var callAudio = loadCallAudio()

func loadCallAudio() callAudioConfig {
	c := callAudioConfig{passthru: true, opusKbps: 64, fec: false, dup: 40 * time.Millisecond}
	if v, ok := envInt("WAD_CALL_PASSTHRU"); ok {
		c.passthru = v != 0
	}
	if v, ok := envInt("WAD_CALL_OPUS_KBPS"); ok && v >= 6 && v <= 510 {
		c.opusKbps = v
	}
	if v, ok := envInt("WAD_CALL_OPUS_FEC"); ok {
		c.fec = v != 0
	}
	if v, ok := envInt("WAD_CALL_DUP_MS"); ok && v >= 0 && v <= 200 {
		c.dup = time.Duration(v) * time.Millisecond
	}
	return c
}

func envInt(k string) (int, bool) {
	s := os.Getenv(k)
	if s == "" {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		log.Printf("calls: ignoring %s=%q: not a number", k, s)
		return 0, false
	}
	return v, true
}

func (c callAudioConfig) String() string {
	mode := "passthrough"
	if !c.passthru {
		mode = "queue"
	}
	return mode + ", opus " + strconv.Itoa(c.opusKbps) + "k, fec " + strconv.FormatBool(c.fec) +
		", duplicate " + c.dup.String()
}

// TimedPCMSink receives the peer's audio as it arrives, with the RTP
// timestamp it was sent with (16 kHz units). Frames can be late, out of
// order, duplicated or missing.
type TimedPCMSink func(rtpTS uint32, pcm []float32)

// TimedBackend is implemented by backends that can deliver timestamped audio.
type TimedBackend interface {
	AnswerTimed(ctx context.Context, callID string, sink TimedPCMSink) (PCMSource, error)
	DialTimed(ctx context.Context, dest string, sink TimedPCMSink) (PCMSource, string, error)
}

// downlink is the WhatsApp -> phone direction for one call. Audio arrives
// before the phone leg is up (ICE + DTLS take most of a second); it is
// dropped rather than queued, so the call does not open with a backlog.
type downlink struct {
	leg atomic.Pointer[PhoneLeg]
}

func (d *downlink) frame(ts uint32, pcm []float32) {
	if l := d.leg.Load(); l != nil {
		if err := l.WriteTimed(ts, f32to16(pcm)); err != nil {
			log.Printf("calls: %s: writing to the phone: %v", l.callID, err)
		}
	}
}

// timedStats is what WriteTimed counts, reported every few seconds.
type timedStats struct {
	frames  int // WhatsApp frames forwarded
	late    int // arrived after a newer one had been sent: dropped
	gapMs   int // timestamp gaps — audio WhatsApp never delivered
	rebases int // timestamp discontinuities (stream restarted)
	dupSent int // duplicate packets sent
}

// answerPassthru answers an incoming WhatsApp call with timestamped audio and
// bridges it to the phone — answerRealCall without the queue.
func (m *Manager) answerPassthru(ctx context.Context, callID string, tb TimedBackend) error {
	dl := &downlink{}
	src, err := tb.AnswerTimed(ctx, callID, dl.frame)
	if err != nil {
		return err
	}
	leg, err := m.startPhoneLeg(callID)
	if err != nil {
		_ = src.Close()
		return err
	}
	m.wirePassthru(callID, leg, src, dl)
	m.hub.PushT(ws.TCallState, map[string]any{"callid": callID, "state": "accepted"})
	return nil
}

// wirePassthru connects both directions once the phone leg exists. The
// uplink is unchanged: the phone's audio goes to WhatsApp as before.
func (m *Manager) wirePassthru(callID string, leg *PhoneLeg, src PCMSource, dl *downlink) {
	leg.OnPCM(func(pcm []int16) {
		if err := src.Write(i16tof32(pcm)); err != nil {
			log.Printf("calls: %s: writing to WhatsApp: %v", callID, err)
		}
	})
	dl.leg.Store(leg)
	stop := make(chan struct{})
	m.mu.Lock()
	m.stopPump = stop
	m.source = src
	m.mu.Unlock()
	go m.reportPassthru(callID, leg, stop)
	log.Printf("calls: %s: audio %s", callID, callAudio)
}

// reportPassthru logs the downlink counters until the call ends.
func (m *Manager) reportPassthru(callID string, leg *PhoneLeg, stop chan struct{}) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s := leg.takeTimedStats()
			log.Printf("calls: %s: %d frames from WhatsApp, %d ms missing, %d late (dropped), %d restarts, %d duplicates sent",
				callID, s.frames, s.gapMs, s.late, s.rebases, s.dupSent)
		}
	}
}
