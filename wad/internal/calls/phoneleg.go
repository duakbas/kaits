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
//   - ONE codec in the MediaEngine: Opus (wideband, opus.go) by default,
//     PCMU (g711.go) as the fallback. The daemon offers, so the daemon picks,
//     and a one-codec offer cannot be mis-negotiated.
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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"wad/internal/ws"
)

// PhoneRate is G.711's wire rate (the fallback codec), 8 kHz by definition.
const PhoneRate = 8000

// PhoneFrame is 20 ms of G.711.
const PhoneFrame = PhoneRate / 50

// LegRate is what the phone leg takes and gives, whatever goes on the wire:
// 16 kHz, the same as meowcaller, so with Opus nothing is resampled at all.
const LegRate = 16000

// LegFrame is 20 ms at LegRate: the cadence the pump writes in.
const LegFrame = LegRate / 50

// opusBitrate for the phone leg. Wideband speech is transparent well below
// this; the headroom is for FEC, which spends bits to survive lost packets.
const opusBitrate = 32000

// phoneCodec picks the wire codec: Opus (wideband) unless built without
// libopus or WAD_CALL_CODEC=pcmu asks for the old narrowband G.711.
func phoneCodec(want string) string {
	if want == "" {
		want = strings.ToLower(os.Getenv("WAD_CALL_CODEC"))
	}
	if want == "pcmu" || !opusAvailable {
		return "pcmu"
	}
	return "opus"
}

// PhoneLeg is one WebRTC session with the app.
type PhoneLeg struct {
	callID string
	send   func(ws.Signal)

	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample

	codec string // "opus" or "pcmu"

	// Opus
	enc     *opusEncoder
	dec     *opusDecoder
	pending []int16 // 16 kHz samples not yet a whole frame
	lastSeq uint16
	haveSeq bool

	// G.711: 16 kHz <-> 8 kHz around the wire
	down *Down2
	up   *Up2

	mu      sync.Mutex
	wmu     sync.Mutex // serialises WritePCM (encoder state)
	dmu     sync.Mutex // serialises decoding against Close
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

	// Codec: "opus", "pcmu", or "" for the default (see phoneCodec).
	Codec string
}

// NewPhoneLeg builds the session but does not offer yet; attach the callbacks
// first, then call Start.
func NewPhoneLeg(callID string, send func(ws.Signal), opt PhoneLegOptions) (*PhoneLeg, error) {
	codec := phoneCodec(opt.Codec)
	capab := webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypePCMU,
		ClockRate: PhoneRate,
		Channels:  1,
	}
	// 0 is PCMU's static payload type, assigned in RFC 3551.
	var pt webrtc.PayloadType = 0
	if codec == "opus" {
		// RFC 7587: Opus is always signalled as 48000/2, whatever it carries.
		// maxplaybackrate tells the phone's encoder 16 kHz is all we want —
		// the same band as WhatsApp's leg — so it doesn't spend bits or CPU
		// on audio that would be thrown away. useinbandfec asks it to send
		// FEC so a lost packet on mobile data is rebuilt instead of clicking.
		capab = webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=0;sprop-stereo=0;maxplaybackrate=16000;sprop-maxcapturerate=16000;maxaveragebitrate=32000",
		}
		pt = 111
	}

	// One codec, never two: a single-codec offer cannot be mis-negotiated,
	// and the track below is bound to exactly this one.
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: capab,
		PayloadType:        pt,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("register %s: %w", codec, err)
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

	track, err := webrtc.NewTrackLocalStaticSample(capab, "audio", "wad")
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

	l := &PhoneLeg{callID: callID, send: send, pc: pc, track: track, codec: codec}
	if codec == "opus" {
		if l.enc, err = newOpusEncoder(LegRate, opusBitrate); err == nil {
			l.dec, err = newOpusDecoder(LegRate)
		}
		if err != nil {
			l.enc.Close()
			_ = pc.Close()
			return nil, err
		}
	} else {
		l.down, l.up = NewDown2(), NewUp2()
	}
	log.Printf("calls: phone leg %s: codec %s", callID, codec)

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

// OnPCM registers the sink for audio arriving FROM the phone, as LegRate
// (16 kHz) mono linear samples. One call per packet.
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

// Codec reports the wire codec, for the logs.
func (l *PhoneLeg) Codec() string { return l.codec }

// WritePCM sends LegRate (16 kHz) mono audio to the phone. Any length is
// accepted: Opus needs whole 20 ms frames, so a remainder waits for the next
// call; G.711 is resampled to 8 kHz and sent as it is.
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
	l.wmu.Lock()
	defer l.wmu.Unlock()

	if l.codec != "opus" {
		f := make([]float32, len(pcm))
		for i, s := range pcm {
			f[i] = float32(s) / 32768
		}
		nb := l.down.Process(f)
		if len(nb) == 0 {
			return nil
		}
		return l.track.WriteSample(media.Sample{
			Data:     EncodeULaw(nb),
			Duration: time.Duration(len(nb)) * time.Second / PhoneRate,
		})
	}

	l.pending = append(l.pending, pcm...)
	for len(l.pending) >= LegFrame {
		pkt, err := l.enc.Encode(l.pending[:LegFrame])
		if err != nil {
			return err
		}
		l.pending = l.pending[LegFrame:]
		if err := l.track.WriteSample(media.Sample{
			Data:     pkt,
			Duration: 20 * time.Millisecond,
		}); err != nil {
			return err
		}
	}
	if len(l.pending) == 0 {
		l.pending = l.pending[:0:0] // let the backing array go
	}
	return nil
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
	l.dmu.Lock()
	defer l.dmu.Unlock()
	l.mu.Lock()
	cb, closed := l.onPCM, l.closed
	l.mu.Unlock()
	if cb == nil || closed {
		return
	}

	if l.codec != "opus" {
		f := l.up.Process(DecodeULaw(pkt.Payload))
		out := make([]int16, len(f))
		for i, s := range f {
			out[i] = clamp16(float64(s) * 32767)
		}
		cb(out)
		return
	}

	// Lost packets: rebuild the one just before this from this packet's FEC,
	// conceal any earlier ones. Capped, so a long outage does not dump
	// seconds of synthesised audio on WhatsApp at once.
	if l.haveSeq {
		gap := int(pkt.SequenceNumber - l.lastSeq) // uint16 wraparound
		if gap <= 0 || gap > 0x8000 {
			return // duplicate or reordered-late: already played past it
		}
		lost := gap - 1
		if lost > 5 {
			lost = 5
		}
		for i := 0; i < lost; i++ {
			var pcm []int16
			var err error
			if i == lost-1 {
				pcm, err = l.dec.Decode(pkt.Payload, LegFrame, true)
			} else {
				pcm, err = l.dec.Decode(nil, LegFrame, false)
			}
			if err == nil && len(pcm) > 0 {
				cb(pcm)
			}
		}
	}
	l.lastSeq, l.haveSeq = pkt.SequenceNumber, true

	pcm, err := l.dec.Decode(pkt.Payload, 0, false)
	if err != nil {
		log.Printf("calls: %s: %v", l.callID, err)
		return
	}
	cb(pcm)
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
	err := l.pc.Close()
	l.wmu.Lock()
	l.enc.Close()
	l.wmu.Unlock()
	l.dmu.Lock()
	l.dec.Close()
	l.dmu.Unlock()
	return err
}

// ConnectionState exposes pion's view, for the diagnostics that a call which
// "connected" but carries no audio always ends up needing.
func (l *PhoneLeg) ConnectionState() string {
	return l.pc.ConnectionState().String()
}
