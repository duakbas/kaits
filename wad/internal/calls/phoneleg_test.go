package calls

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"wad/internal/ws"
)

// The offer is the one artefact the phone has to understand, and the phone is
// a browser engine from 2016 that nothing here can run. So assert its shape.

func offerFrom(t *testing.T, codec ...string) string {
	t.Helper()
	var got string
	var mu sync.Mutex
	leg, err := NewPhoneLeg("c1", func(s ws.Signal) {
		if s.Kind == "offer" {
			mu.Lock()
			got = s.SDP
			mu.Unlock()
		}
	}, PhoneLegOptions{Codec: strings.Join(codec, "")})
	if err != nil {
		t.Fatalf("new leg: %v", err)
	}
	defer leg.Close()
	if err := leg.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got == "" {
		t.Fatal("Start produced no offer")
	}
	return got
}

// One codec, and it is PCMU. The daemon offers precisely so that it gets to
// decide this: a single-codec offer cannot be mis-negotiated, and it keeps the
// daemon's whole codec burden at g711.go. If this ever picks up a second
// payload type, the transcode path has a case it does not handle.
func TestOfferAdvertisesPCMUAndNothingElse(t *testing.T) {
	sdp := offerFrom(t, "pcmu")

	var audio string
	for _, line := range strings.Split(sdp, "\n") {
		if strings.HasPrefix(line, "m=audio") {
			audio = strings.TrimSpace(line)
		}
	}
	if audio == "" {
		t.Fatalf("no m=audio line in offer:\n%s", sdp)
	}
	// m=audio <port> <proto> <payload types...>
	fields := strings.Fields(audio)
	if len(fields) < 4 {
		t.Fatalf("m=audio line has no payload types: %q", audio)
	}
	if pts := fields[3:]; len(pts) != 1 || pts[0] != "0" {
		t.Errorf("payload types = %v, want exactly [0] (PCMU)", pts)
	}
	if !strings.Contains(sdp, "PCMU/8000") {
		t.Errorf("offer does not map payload 0 to PCMU/8000:\n%s", sdp)
	}
	for _, unwanted := range []string{"opus", "G722", "PCMA", "telephone-event"} {
		if strings.Contains(sdp, unwanted) {
			t.Errorf("offer mentions %s; the media engine should carry PCMU alone", unwanted)
		}
	}
}

// The default offer: Opus, then G.722, then PCMU, best first.
func TestOfferDefaultIsOpusG722PCMU(t *testing.T) {
	if !opusAvailable || !g722Available {
		t.Skip("built without libopus or spandsp")
	}
	sdp := offerFrom(t)
	var audio string
	for _, line := range strings.Split(sdp, "\n") {
		if strings.HasPrefix(line, "m=audio") {
			audio = strings.TrimSpace(line)
		}
	}
	if f := strings.Fields(audio); len(f) != 7 || strings.Join(f[3:], " ") != "111 110 9 0" {
		t.Errorf("m=audio = %q, want payload types 111 110 9 0", audio)
	}
	for _, want := range []string{"a=rtpmap:111 opus/48000/2", "a=rtpmap:110 opus/48000/1", "a=rtpmap:9 G722/8000", "a=rtpmap:0 PCMU/8000", "maxplaybackrate=16000"} {
		if !strings.Contains(sdp, want) {
			t.Errorf("offer is missing %s:\n%s", want, sdp)
		}
	}
}

// No header extensions, which is the other half of "minimal SDP for an old
// parser". Registering the default interceptors would add TWCC and the extmap
// lines it needs, and they buy nothing on a single 64 kbit/s audio stream.
func TestOfferCarriesNoHeaderExtensions(t *testing.T) {
	sdp := offerFrom(t)
	// "a=extmap:N uri" is a header extension. "a=extmap-allow-mixed" is not —
	// it is a session-level note (RFC 8285) that pion always emits, saying it
	// would accept two-byte extension headers if any existed. No extension is
	// negotiated by it, and an unknown a= line is skipped by any conformant
	// parser, so it costs nothing. Match on the colon or this assertion
	// catches the wrong thing, which it did.
	if strings.Contains(sdp, "a=extmap:") {
		t.Errorf("offer contains header extensions:\n%s", sdp)
	}
	// The things a 2016 engine does need, on the other hand, must be present.
	for _, want := range []string{"a=rtcp-mux", "a=fingerprint:sha-256", "a=setup:actpass"} {
		if !strings.Contains(sdp, want) {
			t.Errorf("offer is missing %s:\n%s", want, sdp)
		}
	}
}

// Both directions, which is what a call is. sendrecv in the offer, or the
// phone has no reason to send its microphone back.
func TestOfferIsSendrecv(t *testing.T) {
	sdp := offerFrom(t)
	if !strings.Contains(sdp, "a=sendrecv") {
		t.Errorf("offer is not sendrecv:\n%s", sdp)
	}
}

// An app that sends an offer is confused; answering it would half-establish a
// session with nobody driving it.
func TestAnAppOfferIsRefused(t *testing.T) {
	leg, err := NewPhoneLeg("c1", func(ws.Signal) {}, PhoneLegOptions{})
	if err != nil {
		t.Fatalf("new leg: %v", err)
	}
	defer leg.Close()
	if err := leg.HandleSignal(ws.Signal{Kind: "offer", SDP: "v=0"}); err == nil {
		t.Error("an offer from the app was accepted")
	}
	if err := leg.HandleSignal(ws.Signal{Kind: "nonsense"}); err == nil {
		t.Error("an unknown signal kind was accepted")
	}
	// End-of-candidates arrives as an empty candidate and is not an error.
	if err := leg.HandleSignal(ws.Signal{Kind: "ice"}); err != nil {
		t.Errorf("end-of-candidates treated as an error: %v", err)
	}
	if err := leg.HandleSignal(ws.Signal{Kind: "ice", Candidate: json.RawMessage(`{"candidate":""}`)}); err != nil {
		t.Errorf("empty candidate treated as an error: %v", err)
	}
}

// Closing twice happens: a call can end from both ends at the same moment.
func TestCloseIsIdempotent(t *testing.T) {
	leg, err := NewPhoneLeg("c1", func(ws.Signal) {}, PhoneLegOptions{})
	if err != nil {
		t.Fatalf("new leg: %v", err)
	}
	if err := leg.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := leg.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
	// And writing to a closed leg is a no-op rather than a panic.
	if err := leg.WritePCM(make([]int16, LegFrame)); err != nil {
		t.Errorf("write after close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The real one: a whole session, negotiated and carrying audio.
//
// This cannot prove Gecko 48 interoperates — nothing here can run a 2016
// browser. What it does prove is that everything on OUR side is right: the
// offer is answerable, ICE completes, DTLS-SRTP comes up, µ-law survives the
// round trip, and audio moves in both directions. When the phone fails, this
// test passing is what says the fault is at the far end.

// fakePhone is a plain pion peer configured the way the app will be: PCMU,
// answers rather than offers, sends its microphone back.
type fakePhone struct {
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample
	codec string
	enc   *opusEncoder
	dec   *opusDecoder
	g7e   *g722Encoder
	g7d   *g722Decoder
}

func newFakePhone(t *testing.T, codec string) *fakePhone {
	t.Helper()
	capab := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: PhoneRate, Channels: 1}
	var pt webrtc.PayloadType = 0
	switch codec {
	case "opus":
		// What Firefox registers.
		capab = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "maxplaybackrate=48000;stereo=1;useinbandfec=1"}
		pt = 109
	case "g722":
		capab = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeG722, ClockRate: 8000, Channels: 1}
		pt = 9
	case "opusmono":
		// What KaiOS's Gecko registers (webrtc-probe.py on a 2720 Flip).
		capab = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 1,
			SDPFmtpLine: "maxplaybackrate=48000;stereo=0"}
		pt = 109
	}
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: capab,
		PayloadType:        pt,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("phone codec: %v", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me),
		webrtc.WithInterceptorRegistry(&interceptor.Registry{}))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("phone pc: %v", err)
	}
	track, err := webrtc.NewTrackLocalStaticSample(capab, "audio", "phone")
	if err != nil {
		t.Fatalf("phone track: %v", err)
	}
	if _, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendrecv,
	}); err != nil {
		t.Fatalf("phone add track: %v", err)
	}
	if codec == "opusmono" {
		codec = "opus"
	}
	fp := &fakePhone{pc: pc, track: track, codec: codec}
	if codec == "g722" {
		if fp.g7e, err = newG722Encoder(); err != nil {
			t.Fatal(err)
		}
		if fp.g7d, err = newG722Decoder(); err != nil {
			t.Fatal(err)
		}
	}
	if codec == "opus" {
		if fp.enc, err = newOpusEncoder(LegRate, 32000); err != nil {
			t.Fatalf("phone encoder: %v", err)
		}
		if fp.dec, err = newOpusDecoder(LegRate); err != nil {
			t.Fatalf("phone decoder: %v", err)
		}
	}
	return fp
}

// decode / encode what the fake phone receives and sends, in its codec.
func (p *fakePhone) decode(b []byte) []int16 {
	if p.codec == "g722" {
		return p.g7d.Decode(b)
	}
	if p.codec == "opus" {
		pcm, _ := p.dec.Decode(b, 0, false)
		return pcm
	}
	return DecodeULaw(b)
}

func (p *fakePhone) frame(tone *Tone) []byte {
	if p.codec == "g722" {
		b := make([]int16, LegFrame)
		tone.Fill(b)
		return p.g7e.Encode(b)
	}
	if p.codec == "opus" {
		b := make([]int16, LegFrame)
		tone.Fill(b)
		pkt, _ := p.enc.Encode(b)
		return pkt
	}
	b := make([]int16, PhoneFrame)
	tone.Fill(b)
	return EncodeULaw(b)
}

func TestLoopbackPCMU(t *testing.T) { loopback(t, "pcmu", "pcmu") }

func TestLoopbackOpus(t *testing.T) {
	if !opusAvailable {
		t.Skip("built without libopus")
	}
	loopback(t, "opus", "")
}

// The 2720 Flip case: the full offer, a phone that can't do Opus.
func TestLoopbackFallsBackToG722(t *testing.T) {
	if !g722Available {
		t.Skip("built without spandsp")
	}
	loopback(t, "g722", "")
}

func TestLoopbackFallsBackToPCMU(t *testing.T) { loopback(t, "pcmu", "") }

// The 2720 Flip itself: Opus registered as opus/48000/1.
func TestLoopbackKaiOSMonoOpus(t *testing.T) {
	if !opusAvailable {
		t.Skip("built without libopus")
	}
	loopback(t, "opusmono", "")
}

// loopback runs a whole session: phoneCodec is the only codec the fake phone
// has; legOffer is the leg's Codec option ("" = the full default offer).
func loopback(t *testing.T, codec, legOffer string) {
	phone := newFakePhone(t, codec)
	defer phone.pc.Close()

	// Candidates can arrive before the description they belong to, and pion
	// refuses them then. Queue until each side is ready — the same dance the
	// app has to do.
	var mu sync.Mutex
	var legReady, phoneReady bool
	var legQueue, phoneQueue []webrtc.ICECandidateInit

	fromPhone := make(chan []int16, 64)
	toPhone := make(chan []int16, 64)

	var leg *PhoneLeg

	addToPhone := func(c webrtc.ICECandidateInit) {
		mu.Lock()
		defer mu.Unlock()
		if !phoneReady {
			phoneQueue = append(phoneQueue, c)
			return
		}
		_ = phone.pc.AddICECandidate(c)
	}
	addToLeg := func(c webrtc.ICECandidateInit) {
		mu.Lock()
		defer mu.Unlock()
		if !legReady {
			legQueue = append(legQueue, c)
			return
		}
		b, _ := json.Marshal(c)
		_ = leg.HandleSignal(ws.Signal{Kind: "ice", Candidate: b})
	}

	send := func(s ws.Signal) {
		switch s.Kind {
		case "offer":
			if err := phone.pc.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer, SDP: s.SDP,
			}); err != nil {
				t.Errorf("phone setRemoteDescription: %v", err)
				return
			}
			mu.Lock()
			phoneReady = true
			queued := phoneQueue
			phoneQueue = nil
			mu.Unlock()
			for _, c := range queued {
				_ = phone.pc.AddICECandidate(c)
			}

			answer, err := phone.pc.CreateAnswer(nil)
			if err != nil {
				t.Errorf("phone createAnswer: %v", err)
				return
			}
			if err := phone.pc.SetLocalDescription(answer); err != nil {
				t.Errorf("phone setLocalDescription: %v", err)
				return
			}
			if err := leg.HandleSignal(ws.Signal{Kind: "answer", SDP: answer.SDP}); err != nil {
				t.Errorf("leg handle answer: %v", err)
				return
			}
			mu.Lock()
			legReady = true
			lq := legQueue
			legQueue = nil
			mu.Unlock()
			for _, c := range lq {
				b, _ := json.Marshal(c)
				_ = leg.HandleSignal(ws.Signal{Kind: "ice", Candidate: b})
			}

		case "ice":
			var c webrtc.ICECandidateInit
			if err := json.Unmarshal(s.Candidate, &c); err != nil {
				t.Errorf("unmarshal candidate: %v", err)
				return
			}
			addToPhone(c)
		}
	}

	var err error
	leg, err = NewPhoneLeg("c1", send, PhoneLegOptions{Codec: legOffer})
	if err != nil {
		t.Fatalf("new leg: %v", err)
	}
	defer leg.Close()

	leg.OnPCM(func(pcm []int16) {
		select {
		case fromPhone <- pcm:
		default:
		}
	})

	phone.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		addToLeg(c.ToJSON())
	})
	phone.pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			select {
			case toPhone <- phone.decode(pkt.Payload):
			default:
			}
		}
	})

	connected := make(chan struct{})
	var once sync.Once
	leg.OnState(func(s string) {
		if s == "connected" {
			once.Do(func() { close(connected) })
		}
	})

	if err := leg.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	select {
	case <-connected:
	case <-time.After(20 * time.Second):
		t.Fatalf("never connected; leg state = %s", leg.ConnectionState())
	}

	// Both sides talk. A recognisable, non-silent signal each way, so that
	// "audio arrived" cannot be satisfied by comfort noise or zeros.
	stop := make(chan struct{})
	defer close(stop)

	legTone := NewTone(440, LegRate)
	want := codec
	if want == "opusmono" {
		want = "opus"
	}
	phoneRate := PhoneRate
	if codec != "pcmu" {
		phoneRate = LegRate
	}
	phoneTone := NewTone(1000, phoneRate)
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				a := make([]int16, LegFrame)
				legTone.Fill(a)
				_ = leg.WritePCM(a)

				_ = phone.track.WriteSample(media.Sample{
					Data:     phone.frame(phoneTone),
					Duration: 20 * time.Millisecond,
				})
			}
		}
	}()

	if got := leg.Codec(); got != want {
		t.Errorf("leg sends %q, want %q", got, want)
	}
	if !waitForLoudFrame(t, toPhone, "daemon -> phone") {
		return
	}
	if !waitForLoudFrame(t, fromPhone, "phone -> daemon") {
		return
	}
}

// waitForLoudFrame drains until a frame arrives that actually has energy in
// it. The first packets after connection can be partial or silent, and
// asserting on the first one makes the test flaky for a reason that has
// nothing to do with the code.
func waitForLoudFrame(t *testing.T, ch <-chan []int16, dir string) bool {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case pcm := <-ch:
			if len(pcm) == 0 {
				continue
			}
			var peak int32
			for _, s := range pcm {
				v := int32(s)
				if v < 0 {
					v = -v
				}
				if v > peak {
					peak = v
				}
			}
			// A tone at a quarter of full scale should come through far above
			// this; the threshold only excludes silence and dither.
			if peak > 1000 {
				return true
			}
		case <-deadline:
			t.Errorf("%s: no audible frame within the deadline", dir)
			return false
		}
	}
}
