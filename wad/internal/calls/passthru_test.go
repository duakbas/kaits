package calls

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"wad/internal/ws"
)

// captureWriter records what the leg's track would put on the wire.
type captureWriter struct {
	mu   sync.Mutex
	hdrs []rtp.Header
}

func (c *captureWriter) WriteRTP(h *rtp.Header, _ []byte) (int, error) {
	c.mu.Lock()
	c.hdrs = append(c.hdrs, *h)
	c.mu.Unlock()
	return 0, nil
}
func (c *captureWriter) Write(b []byte) (int, error) { return len(b), nil }

func (c *captureWriter) seen() []rtp.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]rtp.Header(nil), c.hdrs...)
}

func timedLeg(codec string, w webrtc.TrackLocalWriter) *PhoneLeg {
	l := &PhoneLeg{callID: "t"}
	l.track = &legTrack{leg: l, name: codec, w: w, seq: 1000, ts: 50000, ssrc: 7}
	return l
}

func waFrame(v int16) []int16 {
	f := make([]int16, 960)
	for i := range f {
		f[i] = v
	}
	return f
}

// The slot arithmetic: a lost WhatsApp frame leaves a gap in BOTH sequence
// numbers and timestamps (so the phone sees loss and conceals), a late frame
// or a duplicate is dropped, and a stream restart continues seamlessly.
func TestWriteTimedSlots(t *testing.T) {
	old := callAudio
	callAudio.dup = 0
	defer func() { callAudio = old }()

	w := &captureWriter{}
	l := timedLeg("pcmu", w)
	for _, ts := range []uint32{
		0, 960, 1920, // slots 0-8
		// 2880 lost  -> slots 9-11 never sent
		3840,              // slots 12-14
		2880,              // late: dropped
		3840,              // duplicate: dropped
		4800,              // slots 15-17
		4800 + 10*LegRate, // restart: carries on at slot 18
	} {
		if err := l.WriteTimed(ts, waFrame(1000)); err != nil {
			t.Fatal(err)
		}
	}

	var slots []int
	for _, h := range w.seen() {
		s := int(uint16(h.SequenceNumber - 1000))
		slots = append(slots, s)
		if want := uint32(50000 + s*160); h.Timestamp != want { // PCMU: 160 ticks per 20 ms
			t.Errorf("slot %d: timestamp %d, want %d", s, h.Timestamp, want)
		}
	}
	want := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	if len(slots) != len(want) {
		t.Fatalf("slots %v, want %v", slots, want)
	}
	for i := range want {
		if slots[i] != want[i] {
			t.Fatalf("slots %v, want %v", slots, want)
		}
	}
	st := l.takeTimedStats()
	if st.frames != 6 || st.late != 2 || st.gapMs != 60 || st.rebases != 1 {
		t.Errorf("stats %+v, want 6 frames, 2 late, 60 ms gap, 1 restart", st)
	}
}

// Timestamps wrap at 2^32 (about 3 days at 16 kHz, but WhatsApp's start
// at a random value). The wrap must look like any other 60 ms step.
func TestWriteTimedWrap(t *testing.T) {
	old := callAudio
	callAudio.dup = 0
	defer func() { callAudio = old }()

	w := &captureWriter{}
	l := timedLeg("pcmu", w)
	start := uint32(1<<32 - 960)
	for i := uint32(0); i < 3; i++ {
		_ = l.WriteTimed(start+i*960, waFrame(1000))
	}
	h := w.seen()
	if len(h) != 9 || uint16(h[8].SequenceNumber-1000) != 8 {
		t.Fatalf("got %d packets, last seq offset %d; want 9 contiguous", len(h), uint16(h[len(h)-1].SequenceNumber-1000))
	}
	if st := l.takeTimedStats(); st.rebases != 0 || st.gapMs != 0 {
		t.Errorf("wrap misread as a discontinuity: %+v", st)
	}
}

// Every packet goes out twice, the copy dup later, identical.
func TestWriteTimedDuplicates(t *testing.T) {
	old := callAudio
	callAudio.dup = 15 * time.Millisecond
	defer func() { callAudio = old }()

	w := &captureWriter{}
	l := timedLeg("pcmu", w)
	_ = l.WriteTimed(0, waFrame(1000))
	if n := len(w.seen()); n != 3 {
		t.Fatalf("%d packets immediately, want 3 (copies come later)", n)
	}
	time.Sleep(60 * time.Millisecond)
	h := w.seen()
	if len(h) != 6 {
		t.Fatalf("%d packets after the delay, want 6", len(h))
	}
	// The three copies fire on separate timers, in any order; the receiver
	// places packets by sequence number, so only the set matters.
	orig := map[uint16]uint32{}
	for _, x := range h[:3] {
		orig[x.SequenceNumber] = x.Timestamp
	}
	for _, x := range h[3:] {
		if ts, ok := orig[x.SequenceNumber]; !ok || ts != x.Timestamp {
			t.Errorf("copy seq %d ts %d matches no original", x.SequenceNumber, x.Timestamp)
		}
		delete(orig, x.SequenceNumber)
	}
	if st := l.takeTimedStats(); st.dupSent != 3 {
		t.Errorf("dupSent %d, want 3", st.dupSent)
	}
}

// Audio before the phone leg has negotiated is dropped, not queued — and the
// slot count starts with the first frame that actually goes out.
func TestWriteTimedDropsBeforeNegotiation(t *testing.T) {
	old := callAudio
	callAudio.dup = 0
	defer func() { callAudio = old }()

	w := &captureWriter{}
	l := timedLeg("pcmu", w)
	l.track.w = nil // not bound yet
	for i := uint32(0); i < 20; i++ {
		_ = l.WriteTimed(i*960, waFrame(1000))
	}
	l.track.w = w
	_ = l.WriteTimed(20*960, waFrame(1000))
	h := w.seen()
	if len(h) != 3 || h[0].SequenceNumber != 1000 || h[0].Timestamp != 50000 {
		t.Fatalf("after negotiation got %d packets starting seq %d ts %d; want 3 from slot 0",
			len(h), h[0].SequenceNumber, h[0].Timestamp)
	}
}

// End to end over real WebRTC (DTLS-SRTP) against a pion peer standing in for
// the phone: lost WhatsApp frames arrive as sequence gaps, and duplicates are
// sent but each packet is delivered to the phone exactly once — the SRTP
// replay window drops the second copy, which is what Gecko's libsrtp does.
func TestPassthroughEndToEnd(t *testing.T) {
	old := callAudio
	callAudio.dup = 20 * time.Millisecond
	defer func() { callAudio = old }()

	leg, got := connectedLeg(t, "pcmu")
	defer leg.Close()

	// 1 s of WhatsApp audio with frames 5 and 6 lost, frame 9 late (after 10).
	order := []uint32{0, 1, 2, 3, 4, 7, 8, 10, 9, 11, 12, 13, 14, 15, 16}
	for _, f := range order {
		if err := leg.WriteTimed(f*960, waFrame(4000)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	got.mu.Lock()
	defer got.mu.Unlock()
	seqs := map[uint16]int{}
	var base uint16
	for i, h := range got.hdrs {
		if i == 0 {
			base = h.SequenceNumber
		}
		seqs[h.SequenceNumber-base]++
	}
	for s, n := range seqs {
		if n != 1 {
			t.Errorf("seq +%d delivered %d times, want once", s, n)
		}
	}
	// frames 5, 6 lost and 9 late -> slots 15-20 and 27-29 never sent
	for _, missing := range []uint16{15, 16, 17, 18, 19, 20, 27, 28, 29} {
		if seqs[missing] != 0 {
			t.Errorf("seq +%d delivered, but its frame was lost or late", missing)
		}
	}
	if want := (len(order) - 1) * 3; len(seqs) != want { // -1: the late frame
		t.Errorf("%d distinct packets delivered, want %d", len(seqs), want)
	}
	if st := leg.takeTimedStats(); st.dupSent != (len(order)-1)*3 || st.late != 1 || st.gapMs != 180 {
		t.Errorf("stats %+v, want %d duplicates, 1 late, 180 ms missing (5, 6 lost; 9 counted missing when 10 came first)", st, (len(order)-1)*3)
	}
}

type rxLog struct {
	mu   sync.Mutex
	hdrs []rtp.Header
}

// connectedLeg brings up a PhoneLeg against a pion peer (same signalling as
// loopback) and records every RTP header the peer receives.
func connectedLeg(t *testing.T, codec string) (*PhoneLeg, *rxLog) {
	t.Helper()
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1},
		PayloadType:        0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(&interceptor.Registry{}))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}

	log := &rxLog{}
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			log.mu.Lock()
			log.hdrs = append(log.hdrs, pkt.Header)
			log.mu.Unlock()
		}
	})

	var mu sync.Mutex
	var legReady, phoneReady bool
	var legQ, phoneQ []webrtc.ICECandidateInit
	var leg *PhoneLeg
	send := func(s ws.Signal) {
		switch s.Kind {
		case "offer":
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: s.SDP}); err != nil {
				t.Errorf("phone setRemote: %v", err)
				return
			}
			mu.Lock()
			phoneReady = true
			q := phoneQ
			phoneQ = nil
			mu.Unlock()
			for _, c := range q {
				_ = pc.AddICECandidate(c)
			}
			ans, err := pc.CreateAnswer(nil)
			if err != nil {
				t.Errorf("answer: %v", err)
				return
			}
			_ = pc.SetLocalDescription(ans)
			if err := leg.HandleSignal(ws.Signal{Kind: "answer", SDP: ans.SDP}); err != nil {
				t.Errorf("leg answer: %v", err)
				return
			}
			mu.Lock()
			legReady = true
			lq := legQ
			legQ = nil
			mu.Unlock()
			for _, c := range lq {
				b, _ := json.Marshal(c)
				_ = leg.HandleSignal(ws.Signal{Kind: "ice", Candidate: b})
			}
		case "ice":
			var c webrtc.ICECandidateInit
			_ = json.Unmarshal(s.Candidate, &c)
			mu.Lock()
			if !phoneReady {
				phoneQ = append(phoneQ, c)
				mu.Unlock()
				return
			}
			mu.Unlock()
			_ = pc.AddICECandidate(c)
		}
	}
	leg, err = NewPhoneLeg("e2e", send, PhoneLegOptions{Codec: codec})
	if err != nil {
		t.Fatal(err)
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		mu.Lock()
		if !legReady {
			legQ = append(legQ, c.ToJSON())
			mu.Unlock()
			return
		}
		mu.Unlock()
		b, _ := json.Marshal(c.ToJSON())
		_ = leg.HandleSignal(ws.Signal{Kind: "ice", Candidate: b})
	})
	connected := make(chan struct{})
	var once sync.Once
	leg.OnState(func(s string) {
		if s == "connected" {
			once.Do(func() { close(connected) })
		}
	})
	if err := leg.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("leg never connected")
	}
	// Wait for the track to be bound to a codec.
	for i := 0; i < 100 && leg.Codec() == ""; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if leg.Codec() == "" {
		t.Fatal("no codec negotiated")
	}
	return leg, log
}
