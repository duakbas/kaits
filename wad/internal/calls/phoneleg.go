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
//   - A short, fixed codec list: Opus, G.722, PCMU (phoneCodecs). The phone
//     answers with what it can do and legTrack binds to the best of those.
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

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

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

// legCodec is one codec the daemon can offer the phone.
type legCodec struct {
	name string
	p    webrtc.RTPCodecParameters
}

var (
	codecOpus = legCodec{"opus", webrtc.RTPCodecParameters{
		// RFC 7587: Opus is always signalled as 48000/2, whatever it carries.
		// maxplaybackrate=16000: we only want wideband, the same band as
		// WhatsApp's leg. Only parameters Firefox and Chrome themselves emit,
		// because Gecko 48's SDP parser is strict.
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=0;maxplaybackrate=16000",
		},
		PayloadType: 111,
	}}
	// KaiOS's Gecko registers Opus as opus/48000/1 (mono) instead of the
	// standard /2, and only accepts an offer whose channel count matches
	// exactly — webrtc-probe.py showed it. Offered as well as the standard
	// one, so Chrome-style peers and the phone each find theirs. The packets
	// are the same mono Opus either way.
	codecOpusMono = legCodec{"opus", webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 1,
			SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=0;maxplaybackrate=16000",
		},
		PayloadType: 110,
	}}
	// G.722: 16 kHz audio, but signalled (and timestamped) at 8000 — RFC 3551.
	codecG722 = legCodec{"g722", webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeG722, ClockRate: 8000, Channels: 1},
		PayloadType:        9,
	}}
	codecPCMU = legCodec{"pcmu", webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: PhoneRate, Channels: 1},
		PayloadType:        0,
	}}
)

// phoneCodecs is the offer, best first. The phone answers with the ones it
// can do and the leg uses the best of those — so a phone that can't do Opus
// (the 2720 Flip rejected an Opus-only offer) still gets wideband G.722, and
// G.711 is always there as the floor.
//
// WAD_CALL_CODEC=pcmu|g722|opus narrows it: that codec first, G.711 behind it.
func phoneCodecs(want string) []legCodec {
	if want == "" {
		want = strings.ToLower(os.Getenv("WAD_CALL_CODEC"))
	}
	var out []legCodec
	add := func(c legCodec, ok bool) {
		if ok {
			out = append(out, c)
		}
	}
	switch want {
	case "pcmu":
	case "g722":
		add(codecG722, g722Available)
	case "opus":
		add(codecOpus, opusAvailable)
		add(codecOpusMono, opusAvailable)
	default:
		add(codecOpus, opusAvailable)
		add(codecOpusMono, opusAvailable)
		add(codecG722, g722Available)
	}
	return append(out, codecPCMU)
}

// PhoneLeg is one WebRTC session with the app.
type PhoneLeg struct {
	callID string
	send   func(ws.Signal)

	pc    *webrtc.PeerConnection
	track *legTrack

	// Encoders run on the pump goroutine (under wmu), decoders on pion's
	// read goroutine (under dmu). Each is created when its direction's codec
	// is known: sending at Bind, receiving at OnTrack.
	opusEnc *opusEncoder
	opusDec *opusDecoder
	g722Enc *g722Encoder
	g722Dec *g722Decoder
	down    *Down2
	up      *Up2
	pending []int16 // 16 kHz samples not yet a whole frame
	lastSeq uint16
	haveSeq bool
	rxCodec string

	mu      sync.Mutex
	wmu     sync.Mutex
	dmu     sync.Mutex
	closed  bool
	nCands  int
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

	// Codec: "opus", "g722", "pcmu", or "" for all of them (see phoneCodecs).
	Codec string
}

// NewPhoneLeg builds the session but does not offer yet; attach the callbacks
// first, then call Start.
func NewPhoneLeg(callID string, send func(ws.Signal), opt PhoneLegOptions) (*PhoneLeg, error) {
	offer := phoneCodecs(opt.Codec)
	me := &webrtc.MediaEngine{}
	var names []string
	for _, c := range offer {
		if err := me.RegisterCodec(c.p, webrtc.RTPCodecTypeAudio); err != nil {
			return nil, fmt.Errorf("register %s: %w", c.name, err)
		}
		names = append(names, c.name)
	}

	// An EMPTY registry, passed explicitly. Leaving the option off would make
	// pion install its defaults (NACK, TWCC and their extmap lines, which a
	// 2016 SDP parser has opinions about).
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

	l := &PhoneLeg{callID: callID, send: send, pc: pc}
	l.track = &legTrack{leg: l, prefer: offer}

	// sendrecv explicitly. A phone call is both directions, and letting
	// AddTrack infer the direction has bitten people when the far end is fussy
	// about what it gets back.
	if _, err := pc.AddTransceiverFromTrack(l.track, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendrecv,
	}); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("add track: %w", err)
	}
	log.Printf("calls: phone leg %s: offering %s", callID, strings.Join(names, ", "))

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

	// Transport detail, for a call stuck at "connecting": ICE (did the
	// candidates reach each other?) and DTLS (did encryption come up?) are
	// separate stages and fail for different reasons.
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Printf("calls: phone leg %s: ice %s (%d candidates from the phone)", callID, s, l.remoteCands())
		if s == webrtc.ICEConnectionStateConnected {
			if st := pc.GetStats(); st != nil {
				for _, v := range st {
					if p, ok := v.(webrtc.ICECandidatePairStats); ok && p.Nominated {
						log.Printf("calls: phone leg %s: ice pair %s", callID, p.ID)
					}
				}
			}
		}
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
// the offerer is what lets it choose which codecs are on the table.
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
		err := l.pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer,
			SDP:  sig.SDP,
		})
		if err == nil {
			if trs := l.pc.GetTransceivers(); len(trs) > 0 && trs[0].Sender() != nil {
				if dt := trs[0].Sender().Transport(); dt != nil {
					dt.OnStateChange(func(st webrtc.DTLSTransportState) {
						log.Printf("calls: phone leg %s: dtls %s", l.callID, st)
					})
				}
			}
		}
		if err != nil {
			// The phone's answer is the only record of what it would accept;
			// without it a refusal ("no ice-ufrag" = it rejected every codec)
			// can't be diagnosed.
			log.Printf("calls: %s: phone's answer was refused (%v):\n%s", l.callID, err, answerSummary(sig.SDP))
		}
		return err
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
		l.mu.Lock()
		l.nCands++
		n := l.nCands
		l.mu.Unlock()
		if n <= 4 {
			log.Printf("calls: phone leg %s: phone candidate %s", l.callID, candSummary(c.Candidate))
		}
		err := l.pc.AddICECandidate(c)
		if err != nil {
			log.Printf("calls: phone leg %s: candidate refused: %v", l.callID, err)
		}
		return err
	case "offer":
		// The app never offers. If it does, something is confused, and
		// answering it would half-establish a session nobody is driving.
		return fmt.Errorf("app sent an offer; the daemon is the offerer")
	}
	return fmt.Errorf("unknown signal kind %q", sig.Kind)
}

// Codec reports the codec the daemon sends to the phone ("" before the answer).
func (l *PhoneLeg) Codec() string { return l.track.codecName() }

// WritePCM sends LegRate (16 kHz) mono audio to the phone in whatever codec
// was negotiated. Any length is accepted: it is re-cut into 20 ms frames, and
// a remainder waits for the next call.
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
	codec := l.track.codecName()
	if codec == "" {
		return nil // not negotiated yet; audio before the answer is dropped
	}
	l.wmu.Lock()
	defer l.wmu.Unlock()

	l.pending = append(l.pending, pcm...)
	for len(l.pending) >= LegFrame {
		frame := l.pending[:LegFrame]
		var payload []byte
		var ticks uint32
		switch codec {
		case "opus":
			if l.opusEnc == nil {
				enc, err := newOpusEncoder(LegRate, opusBitrate)
				if err != nil {
					return err
				}
				l.opusEnc = enc
			}
			b, err := l.opusEnc.Encode(frame)
			if err != nil {
				return err
			}
			payload, ticks = b, 960 // 20 ms at Opus's 48 kHz RTP clock
		case "g722":
			if l.g722Enc == nil {
				enc, err := newG722Encoder()
				if err != nil {
					return err
				}
				l.g722Enc = enc
			}
			payload, ticks = l.g722Enc.Encode(frame), 160 // 8000 Hz clock, see codecG722
		default:
			if l.down == nil {
				l.down = NewDown2()
			}
			nb := l.down.Process(i16tof32(frame))
			payload, ticks = EncodeULaw(nb), uint32(len(nb))
		}
		l.pending = l.pending[LegFrame:]
		if err := l.track.write(payload, ticks); err != nil {
			return err
		}
	}
	if len(l.pending) == 0 {
		l.pending = l.pending[:0:0] // let the backing array go
	}
	return nil
}

func (l *PhoneLeg) readTrack(tr *webrtc.TrackRemote) {
	name := mimeName(tr.Codec().MimeType)
	l.dmu.Lock()
	l.rxCodec = name
	l.dmu.Unlock()
	log.Printf("calls: phone leg %s: receiving %s from the phone", l.callID, name)
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

	switch l.rxCodec {
	case "opus":
		l.deliverOpus(pkt, cb)
	case "g722":
		if l.g722Dec == nil {
			d, err := newG722Decoder()
			if err != nil {
				log.Printf("calls: %s: %v", l.callID, err)
				return
			}
			l.g722Dec = d
		}
		cb(l.g722Dec.Decode(pkt.Payload))
	default:
		if l.up == nil {
			l.up = NewUp2()
		}
		cb(f32to16(l.up.Process(DecodeULaw(pkt.Payload))))
	}
}

func (l *PhoneLeg) deliverOpus(pkt *rtp.Packet, cb func([]int16)) {
	if l.opusDec == nil {
		d, err := newOpusDecoder(LegRate)
		if err != nil {
			log.Printf("calls: %s: %v", l.callID, err)
			return
		}
		l.opusDec = d
	}
	// Lost packets: rebuild the one just before this from this packet's FEC,
	// conceal any earlier ones. Capped, so a long outage does not dump
	// seconds of synthesised audio on WhatsApp at once.
	if l.haveSeq {
		gap := int(pkt.SequenceNumber - l.lastSeq) // uint16 wraparound
		if gap <= 0 || gap > 0x8000 {
			return // duplicate or late: already played past it
		}
		lost := gap - 1
		if lost > 5 {
			lost = 5
		}
		for i := 0; i < lost; i++ {
			var pcm []int16
			var err error
			if i == lost-1 {
				pcm, err = l.opusDec.Decode(pkt.Payload, LegFrame, true)
			} else {
				pcm, err = l.opusDec.Decode(nil, LegFrame, false)
			}
			if err == nil && len(pcm) > 0 {
				cb(pcm)
			}
		}
	}
	l.lastSeq, l.haveSeq = pkt.SequenceNumber, true

	pcm, err := l.opusDec.Decode(pkt.Payload, 0, false)
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
	l.opusEnc.Close()
	l.g722Enc.Close()
	l.wmu.Unlock()
	l.dmu.Lock()
	l.opusDec.Close()
	l.g722Dec.Close()
	l.dmu.Unlock()
	return err
}

// ConnectionState exposes pion's view, for the diagnostics that a call which
// "connected" but carries no audio always ends up needing.
func (l *PhoneLeg) ConnectionState() string {
	return l.pc.ConnectionState().String()
}

func (l *PhoneLeg) remoteCands() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nCands
}

// candSummary is "udp host 10.x.x.x" style: the type and transport, with the
// address shortened so the log shows the shape, not a full IP.
func candSummary(c string) string {
	f := strings.Fields(c)
	// candidate:<foundation> <component> <transport> <priority> <ip> <port> typ <type> ...
	if len(f) < 8 {
		return c
	}
	ip := f[4]
	if i := strings.LastIndexAny(ip, ".:"); i > 0 {
		ip = ip[:i] + ".x"
	}
	return strings.Join([]string{f[2], f[7], ip}, " ")
}
