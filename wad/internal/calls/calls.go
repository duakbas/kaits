// Package calls bridges WhatsApp calls (via meowcaller) to the phone (via
// WebRTC signalled over the ws hub).
//
// ARCHITECTURE — read this before touching anything:
//
//	WA call  <--MLOW-->  meowcaller  <--PCM-->  pion PeerConnection  <--Opus/WebRTC-->  KaiOS app
//
// meowcaller gives/takes raw PCM float32 (Source to send, Sink to receive).
// pion handles the WebRTC leg to the phone and the Opus <-> PCM transcode.
// This package owns the state machine that ties a WA CallOffer to a pion
// session and forwards SDP/ICE both ways as ws TCallSignal frames.
//
// STATUS: the meowcaller half is stubbed. meowcaller has no tagged release and
// its live-media API (the Source side especially) isn't documented in anything
// I could verify. So we define exactly what the daemon needs as an interface
// (Backend) and ship a Noop stub so the daemon builds and runs message-only
// TODAY. Implement meowcaller.go against the real godoc when you `go get` it.
package calls

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"wad/internal/ws"
)

// Backend is the minimal surface the daemon needs from a WhatsApp-call library.
// meowcaller must be adapted to this; the daemon depends only on this.
type Backend interface {
	// Answer accepts an incoming call by id and starts media. sink receives
	// the peer's audio as PCM; the returned Source lets us push OUR audio back.
	Answer(ctx context.Context, callID string, sink PCMSink) (PCMSource, error)

	// Reject declines an incoming call. fromJID is needed as well as the id:
	// a reject is addressed to the caller, not to the call.
	Reject(ctx context.Context, callID, fromJID string) error

	// Dial places an outgoing call to a JID/number. Same media contract.
	Dial(ctx context.Context, dest string, sink PCMSink) (PCMSource, string, error)

	// Hangup ends the active call.
	Hangup(ctx context.Context, callID, fromJID string) error
}

// PCMSink receives 48kHz mono float32 PCM frames from the peer.
type PCMSink func(pcm []float32)

// PCMSource is pushed OUR audio to send to the peer.
type PCMSource interface {
	Write(pcm []float32) error
	Close() error
}

// Manager coordinates one active call at a time (feature phone: no multi-call).
type Manager struct {
	be  Backend
	hub *ws.Hub

	mu           sync.Mutex
	activeCallID string
	// The caller's JID, kept because a reject is addressed to them rather than
	// to the call — the id alone is not enough to decline.
	activeCallFrom string
	// The WebRTC session with the phone, when one is up.
	leg *PhoneLeg
	// Stops the tone pump on a test call.
	stopTone chan struct{}

	// STUN servers for the phone leg. Normally empty; see PhoneLegOptions.
	STUN []string
}

func NewManager(be Backend, hub *ws.Hub) *Manager {
	return &Manager{be: be, hub: hub}
}

// testCallPrefix marks a call that exists only between the daemon and the
// phone. Nothing about it touches WhatsApp — no account, no relay, nothing to
// get flagged — which is the entire point: it lets the WebRTC leg be tested as
// often as needed while the risky half stays switched off.
const testCallPrefix = "test-"

// IsTestCall reports whether an id belongs to a tone test rather than a real
// WhatsApp call.
func IsTestCall(id string) bool { return strings.HasPrefix(id, testCallPrefix) }

// StartTestCall rings the phone with a synthetic call. Answering it plays a
// warbling tone and logs the level of whatever the phone's microphone sends
// back, which between them prove the phone leg in both directions.
func (m *Manager) StartTestCall() string {
	id := testCallPrefix + strconv.FormatInt(time.Now().UnixNano(), 36)
	m.mu.Lock()
	m.activeCallID = id
	m.activeCallFrom = ""
	m.mu.Unlock()

	m.hub.PushT(ws.TCallOffer, ws.CallData{
		CallID:    id,
		FromJID:   "test@wad.local",
		FromName:  "Tone test",
		Timestamp: time.Now().Unix(),
	})
	log.Printf("calls: test call %s ringing", id)
	return id
}

// OnWACallEvent is wired to wa.Client.SetCallHook. It receives the raw
// whatsmeow call events. We only need type-switching on the concrete types;
// kept as `any` so this package doesn't force a whatsmeow import cycle.
//
// The concrete types are events.CallOffer / events.CallOfferNotice /
// events.CallTerminate. Do the type switch in the file that has the whatsmeow
// import (see wacall.go) and call the methods below.
func (m *Manager) NotifyIncoming(callID, fromJID, fromName string, video bool, ts int64) {
	m.mu.Lock()
	m.activeCallID = callID
	m.activeCallFrom = fromJID
	m.mu.Unlock()
	m.hub.PushT(ws.TCallOffer, ws.CallData{
		CallID:    callID,
		FromJID:   fromJID,
		FromName:  fromName,
		Video:     video,
		Timestamp: ts,
	})
	// The phone now rings. It will reply with TCallAnswer or TCallReject,
	// handled in HandleAppFrame.
}

func (m *Manager) NotifyEnded(callID, reason string) {
	m.mu.Lock()
	ours := callID == m.activeCallID
	m.mu.Unlock()
	if ours {
		// Tears down the phone leg too. The far end going away has to take the
		// WebRTC session with it, or the phone sits in a call with nobody on
		// the other side and no way to tell.
		m.endCall()
	}
	m.hub.PushT(ws.TCallState, map[string]any{"callid": callID, "state": "ended", "reason": reason})
}

// HandleAppFrame processes call-related frames coming FROM the phone.
func (m *Manager) HandleAppFrame(ctx context.Context, e ws.Envelope) {
	switch e.T {
	case ws.TCallAnswer:
		// TODO(pion): create PeerConnection, set up transcode, then:
		//   src, err := m.be.Answer(ctx, m.activeCallID, m.onPeerPCM)
		if err := m.answer(ctx); err != nil {
			m.hub.PushT(ws.TError, map[string]string{"code": "answer", "msg": err.Error()})
		}
	case ws.TCallReject:
		m.mu.Lock()
		id, from := m.activeCallID, m.activeCallFrom
		m.mu.Unlock()
		if id != "" {
			// A test call has no far end to decline to.
			if !IsTestCall(id) {
				if err := m.be.Reject(ctx, id, from); err != nil {
					// Worth saying out loud: a decline that silently failed
					// leaves the caller ringing and the user believing they
					// hung up.
					log.Printf("calls: reject %s: %v", id, err)
					m.hub.PushT(ws.TError, map[string]string{"code": "reject", "msg": err.Error()})
				}
			}
			m.endCall()
		}
	case ws.TCallHangup:
		m.mu.Lock()
		id, from := m.activeCallID, m.activeCallFrom
		m.mu.Unlock()
		if id != "" {
			if !IsTestCall(id) {
				if err := m.be.Hangup(ctx, id, from); err != nil {
					log.Printf("calls: hangup %s: %v", id, err)
				}
			}
			m.endCall()
		}
	case ws.TCallSignalA:
		m.mu.Lock()
		leg := m.leg
		m.mu.Unlock()
		if leg == nil {
			// Signalling for a call we are not in. Normal during teardown —
			// the app's last candidates can outrun our hangup — so this is a
			// log line and not an error pushed at the user.
			log.Printf("calls: signalling arrived with no active leg, ignored")
			return
		}
		var sig ws.Signal
		if err := json.Unmarshal(e.Data, &sig); err != nil {
			log.Printf("calls: bad signalling frame: %v", err)
			return
		}
		if err := leg.HandleSignal(sig); err != nil {
			log.Printf("calls: signalling (%s): %v", sig.Kind, err)
		}
	}
}

// startPhoneLeg brings up the WebRTC session and offers it to the app.
// The caller must not hold m.mu.
func (m *Manager) startPhoneLeg(callID string) (*PhoneLeg, error) {
	leg, err := NewPhoneLeg(callID, func(s ws.Signal) {
		m.hub.PushT(ws.TCallSignal, s)
	}, PhoneLegOptions{STUN: m.STUN})
	if err != nil {
		return nil, err
	}

	leg.OnState(func(state string) {
		switch state {
		case "connected":
			m.hub.PushT(ws.TCallState, map[string]any{
				"callid": callID, "state": "connected"})
		case "failed", "closed", "disconnected":
			m.hub.PushT(ws.TCallState, map[string]any{
				"callid": callID, "state": "ended", "reason": state})
		}
	})

	m.mu.Lock()
	m.leg = leg
	m.mu.Unlock()

	if err := leg.Start(); err != nil {
		_ = leg.Close()
		m.mu.Lock()
		m.leg = nil
		m.mu.Unlock()
		return nil, err
	}
	return leg, nil
}

// answerTestCall runs the tone test: play a warble at the phone, and report
// what comes back from its microphone.
//
// The inbound half matters as much as the outbound one. A tone you can hear
// proves DTLS, SRTP and the codec in one direction only, and the direction
// that is harder to get right on this hardware is the microphone —
// getUserMedia in a packaged app, a permission that can be silently refused,
// an echo canceller that mutes everything. Logging the level turns "I think it
// works" into a number.
func (m *Manager) answerTestCall(callID string) error {
	leg, err := m.startPhoneLeg(callID)
	if err != nil {
		return err
	}

	var (
		heardMu   sync.Mutex
		heardPeak int16
		heardN    int
	)
	leg.OnPCM(func(pcm []int16) {
		var peak int16
		for _, s := range pcm {
			v := s
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		heardMu.Lock()
		if peak > heardPeak {
			heardPeak = peak
		}
		heardN++
		heardMu.Unlock()
	})

	stop := make(chan struct{})
	m.mu.Lock()
	m.stopTone = stop
	m.mu.Unlock()

	go func() {
		warble := NewWarble(440, 880, 400, PhoneRate)
		buf := make([]int16, PhoneFrame)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		report := time.NewTicker(2 * time.Second)
		defer report.Stop()

		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				warble.Fill(buf)
				if err := leg.WritePCM(buf); err != nil {
					log.Printf("calls: test tone write: %v", err)
					return
				}
			case <-report.C:
				heardMu.Lock()
				peak, n := heardPeak, heardN
				heardPeak, heardN = 0, 0
				heardMu.Unlock()
				if n == 0 {
					log.Printf("calls: test %s — nothing from the phone's mic yet", callID)
					continue
				}
				log.Printf("calls: test %s — %d packets from the phone, peak %d (%.0f dBFS)",
					callID, n, peak, dbfs(peak))
			}
		}
	}()

	m.hub.PushT(ws.TCallState, map[string]any{"callid": callID, "state": "accepted"})
	return nil
}

func dbfs(peak int16) float64 {
	if peak <= 0 {
		return -99
	}
	return 20 * math.Log10(float64(peak)/32768)
}

// endCall tears down whatever is up. Safe with nothing running.
func (m *Manager) endCall() {
	m.mu.Lock()
	leg, stop := m.leg, m.stopTone
	m.leg, m.stopTone = nil, nil
	m.activeCallID, m.activeCallFrom = "", ""
	m.mu.Unlock()

	if stop != nil {
		close(stop)
	}
	if leg != nil {
		_ = leg.Close()
	}
}

func (m *Manager) answer(ctx context.Context) error {
	m.mu.Lock()
	id := m.activeCallID
	m.mu.Unlock()
	if id == "" {
		return nil
	}
	// A test call is the phone leg alone — no WhatsApp backend to ask.
	if IsTestCall(id) {
		if err := m.answerTestCall(id); err != nil {
			m.hub.PushT(ws.TCallState, map[string]any{
				"callid": id, "state": "ended", "reason": "phone-leg"})
			return err
		}
		return nil
	}
	// Ask the backend before telling the app it worked. The old code announced
	// "accepted" unconditionally, so a phone with no media backend showed a
	// call in progress and silence — which is a worse failure than being told
	// the call cannot be answered here.
	if _, err := m.be.Answer(ctx, m.activeCallID, nil); err != nil {
		m.hub.PushT(ws.TCallState, map[string]any{
			"callid": m.activeCallID, "state": "ended", "reason": "no-media"})
		return err
	}
	m.hub.PushT(ws.TCallState, map[string]any{"callid": m.activeCallID, "state": "accepted"})
	return nil
}

// ---- Noop backend so the daemon compiles & runs message-only today ----

type Noop struct{}

func (Noop) Answer(context.Context, string, PCMSink) (PCMSource, error) {
	log.Printf("calls: Noop backend — Answer ignored (wire meowcaller)")
	return noopSource{}, nil
}
func (Noop) Reject(context.Context, string, string) error { return nil }
func (Noop) Dial(context.Context, string, PCMSink) (PCMSource, string, error) {
	return noopSource{}, "", nil
}
func (Noop) Hangup(context.Context, string, string) error { return nil }

type noopSource struct{}

func (noopSource) Write([]float32) error { return nil }
func (noopSource) Close() error          { return nil }
