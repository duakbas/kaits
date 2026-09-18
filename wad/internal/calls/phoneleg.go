package calls

// The phone leg: a WebRTC session between the daemon and the KaiOS app.
//
// This is the half of calling that nobody else has solved for us. meowcaller
// handles WhatsApp; this handles a 2016 browser engine, and it is the part most
// likely to go wrong, so it is built to be testable on its own: ring the phone,
// answer, hear a tone. No WhatsApp account involved, nothing to get flagged.
//
// WHAT WAS CHECKED BEFORE WRITING ANY OF THIS
//
// The blocker everyone expects is DTLS. pion/dtls implements DTLS 1.2 and
// nothing older, and the phone is Gecko 48. It turns out Firefox has sent a
// DTLS 1.2 ClientHello since Firefox 37 (bug 1153702 is literally titled
// "Firefox 37 uses DTLS 1.2 client, breaking any WebRTC implementations using
// DTLS 1.0"), and has offered TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 and
// TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 since 34. pion supports both. So the
// handshake should work, and Gecko 48 is comfortably above the floor rather
// than just under it.
//
// DELIBERATELY MINIMAL SDP
//
// Two choices here are about talking to a decade-old stack, not about quality:
//
//   - PCMU and nothing else in the MediaEngine. The daemon offers, so the
//     daemon picks, and a one-codec offer cannot be mis-negotiated. It also
//     means the daemon's entire codec burden is g711.go.
//   - No interceptors. RegisterDefaultInterceptors adds NACK, TWCC and the
//     header extensions they need, which put extra a=extmap lines in the offer
//     for a 2016 parser to have opinions about. For one audio track they buy
//     nothing: NACK does not apply to PCMU here, and we are not doing
//     congestion control on a 64 kbit/s stream. The cost is no RTCP sender
//     reports, so no RTT figure. Worth it.

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"wad/internal/ws"
)

// PhoneRate is the sample rate on the phone leg. G.711 is 8 kHz by definition,
// and everything on this side of the daemon is in these units.
const PhoneRate = 8000

// PhoneFrame is the samples in one 20 ms packet: the RTP cadence everything
// here is cut to. 160 samples, and because µ-law is a byte per sample, 160
// bytes on the wire.
const PhoneFrame = PhoneRate / 50

// PhoneLeg is one WebRTC session with the app.
type PhoneLeg struct {
	callID string
	send   func(ws.Signal)

	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample

	mu      sync.Mutex
	closed  bool
	onPCM   func([]int16)
	onState func(string)
}

// PhoneLegOptions is the little that is worth configuring.
type PhoneLegOptions struct {
	// STUN servers. Usually empty and that is correct: the daemon is on a VPS
	// with a public address, so its host candidate is directly reachable and
	// the phone's own address is learned from the binding request it sends
	// (a peer-reflexive candidate). STUN only matters if the daemon is ever
	// behind NAT too.
	STUN []string
}

// NewPhoneLeg builds the session but does not offer yet; attach the callbacks
// first, then call Start.
func NewPhoneLeg(callID string, send func(ws.Signal), opt PhoneLegOptions) (*PhoneLeg, error) {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: PhoneRate,
			Channels:  1,
		},
		// 0 is PCMU's static payload type, assigned in RFC 3551 and understood
		// by everything that has ever done RTP.
		PayloadType: 0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("register PCMU: %w", err)
	}

	// An EMPTY registry, passed explicitly. Leaving the option off would make
	// pion install its defaults; this is how you say "none" out loud.
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithInterceptorRegistry(&interceptor.Registry{}),
	)

	cfg := webrtc.Configuration{}
	if len(opt.STUN) > 0 {
		cfg.ICEServers = []webrtc.ICEServer{{URLs: opt.STUN}}
	}

	pc, err := api.NewPeerConnection(cfg)
	if err != nil {
		return nil, fmt.Errorf("peer connection: %w", err)
	}

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypePCMU,
		ClockRate: PhoneRate,
		Channels:  1,
	}, "audio", "wad")
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("local track: %w", err)
	}

	// sendrecv explicitly. A phone call is both directions, and letting
	// AddTrack infer the direction has bitten people when the far end is fussy
	// about what it gets back.
	if _, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendrecv,
	}); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("add track: %w", err)
	}

	l := &PhoneLeg{callID: callID, send: send, pc: pc, track: track}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return // gathering finished; trickle needs no end-of-candidates here
		}
		b, err := json.Marshal(c.ToJSON())
		if err != nil {
			log.Printf("calls: marshal candidate: %v", err)
			return
		}
		l.emit(ws.Signal{CallID: callID, Kind: "ice", Candidate: b})
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("calls: phone leg %s: %s", callID, s)
		l.mu.Lock()
		cb := l.onState
		l.mu.Unlock()
		if cb != nil {
			cb(s.String())
		}
	})

	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		l.readTrack(tr)
	})

	return l, nil
}

// OnPCM registers the sink for audio arriving FROM the phone, as 8 kHz mono
// linear samples. One call per 20 ms packet.
func (l *PhoneLeg) OnPCM(fn func([]int16)) {
	l.mu.Lock()
	l.onPCM = fn
	l.mu.Unlock()
}

// OnState registers a callback for the connection state, so the manager can
// tell the app when media is actually flowing rather than when it was asked to
// start.
func (l *PhoneLeg) OnState(fn func(string)) {
	l.mu.Lock()
	l.onState = fn
	l.mu.Unlock()
}

// Start creates the offer and sends it to the app. The daemon always offers:
// it is the side that knows a call exists first, in both directions, and being
// the offerer is what lets us pin the codec list to PCMU alone.
func (l *PhoneLeg) Start() error {
	offer, err := l.pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err := l.pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}
	l.emit(ws.Signal{CallID: l.callID, Kind: "offer", SDP: offer.SDP})
	return nil
}

// HandleSignal takes an answer or an ICE candidate from the app.
func (l *PhoneLeg) HandleSignal(sig ws.Signal) error {
	switch sig.Kind {
	case "answer":
		if sig.SDP == "" {
			return fmt.Errorf("answer with no sdp")
		}
		return l.pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer,
			SDP:  sig.SDP,
		})
	case "ice":
		if len(sig.Candidate) == 0 {
			return nil // end-of-candidates; nothing to add
		}
		var c webrtc.ICECandidateInit
		if err := json.Unmarshal(sig.Candidate, &c); err != nil {
			return fmt.Errorf("parse candidate: %w", err)
		}
		// An empty candidate string is how browsers say "that's all of them".
		if c.Candidate == "" {
			return nil
		}
		return l.pc.AddICECandidate(c)
	case "offer":
		// The app never offers. If it does, something is confused, and
		// answering it would half-establish a session nobody is driving.
		return fmt.Errorf("app sent an offer; the daemon is the offerer")
	}
	return fmt.Errorf("unknown signal kind %q", sig.Kind)
}

// WritePCM sends 8 kHz mono linear audio to the phone. Any length is accepted;
// the duration is derived from it so pion's RTP timestamps stay honest even if
// a caller hands over an odd-sized buffer.
func (l *PhoneLeg) WritePCM(pcm []int16) error {
	if len(pcm) == 0 {
		return nil
	}
	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	if closed {
		return nil
	}
	return l.track.WriteSample(media.Sample{
		Data:     EncodeULaw(pcm),
		Duration: time.Duration(len(pcm)) * time.Second / PhoneRate,
	})
}

func (l *PhoneLeg) readTrack(tr *webrtc.TrackRemote) {
	for {
		pkt, _, err := tr.ReadRTP()
		if err != nil {
			return // track ended, or the connection went away
		}
		l.deliver(pkt)
	}
}

func (l *PhoneLeg) deliver(pkt *rtp.Packet) {
	if len(pkt.Payload) == 0 {
		return
	}
	l.mu.Lock()
	cb := l.onPCM
	l.mu.Unlock()
	if cb == nil {
		return
	}
	cb(DecodeULaw(pkt.Payload))
}

func (l *PhoneLeg) emit(s ws.Signal) {
	l.mu.Lock()
	closed, send := l.closed, l.send
	l.mu.Unlock()
	if closed || send == nil {
		return
	}
	send(s)
}

// Close tears the session down. Safe to call more than once, which matters
// because a call can end from either side at the same moment.
func (l *PhoneLeg) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	return l.pc.Close()
}

// ConnectionState exposes pion's view, for the diagnostics that a call which
// "connected" but carries no audio always ends up needing.
func (l *PhoneLeg) ConnectionState() string {
	return l.pc.ConnectionState().String()
}
