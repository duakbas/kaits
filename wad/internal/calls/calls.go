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
	"log"

	"wad/internal/ws"
)

// Backend is the minimal surface the daemon needs from a WhatsApp-call library.
// meowcaller must be adapted to this; the daemon depends only on this.
type Backend interface {
	// Answer accepts an incoming call by id and starts media. sink receives
	// the peer's audio as PCM; the returned Source lets us push OUR audio back.
	Answer(ctx context.Context, callID string, sink PCMSink) (PCMSource, error)

	// Reject declines an incoming call.
	Reject(ctx context.Context, callID string) error

	// Dial places an outgoing call to a JID/number. Same media contract.
	Dial(ctx context.Context, dest string, sink PCMSink) (PCMSource, string, error)

	// Hangup ends the active call.
	Hangup(ctx context.Context, callID string) error
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

	activeCallID string
	// pion PeerConnection would live here; omitted until backend is real.
}

func NewManager(be Backend, hub *ws.Hub) *Manager {
	return &Manager{be: be, hub: hub}
}

// OnWACallEvent is wired to wa.Client.SetCallHook. It receives the raw
// whatsmeow call events. We only need type-switching on the concrete types;
// kept as `any` so this package doesn't force a whatsmeow import cycle.
//
// The concrete types are events.CallOffer / events.CallOfferNotice /
// events.CallTerminate. Do the type switch in the file that has the whatsmeow
// import (see wacall.go) and call the methods below.
func (m *Manager) NotifyIncoming(callID, fromJID, fromName string, video bool, ts int64) {
	m.activeCallID = callID
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
	if callID == m.activeCallID {
		m.activeCallID = ""
	}
	m.hub.PushT(ws.TCallState, map[string]any{"callid": callID, "state": "ended", "reason": reason})
}

// HandleAppFrame processes call-related frames coming FROM the phone.
func (m *Manager) HandleAppFrame(ctx context.Context, e ws.Envelope) {
	switch e.T {
	case ws.TCallAnswer:
		// TODO(pion): create PeerConnection, set up transcode, then:
		//   src, err := m.be.Answer(ctx, m.activeCallID, m.onPeerPCM)
		// For now just acknowledge so the app flow is testable.
		if err := m.answer(ctx); err != nil {
			m.hub.PushT(ws.TError, map[string]string{"code": "answer", "msg": err.Error()})
		}
	case ws.TCallReject:
		if m.activeCallID != "" {
			_ = m.be.Reject(ctx, m.activeCallID)
			m.activeCallID = ""
		}
	case ws.TCallHangup:
		if m.activeCallID != "" {
			_ = m.be.Hangup(ctx, m.activeCallID)
			m.activeCallID = ""
		}
	case ws.TCallSignalA:
		// Forward app's SDP/ICE into pion. TODO when pion is wired.
		log.Printf("calls: got signalling from app (%d bytes) — pion not wired yet", len(e.Data))
	}
}

func (m *Manager) answer(ctx context.Context) error {
	if m.activeCallID == "" {
		return nil
	}
	m.hub.PushT(ws.TCallState, map[string]any{"callid": m.activeCallID, "state": "accepted"})
	// Real impl: m.be.Answer + pion offer -> push TCallSignal to app.
	return nil
}

// ---- Noop backend so the daemon compiles & runs message-only today ----

type Noop struct{}

func (Noop) Answer(context.Context, string, PCMSink) (PCMSource, error) {
	log.Printf("calls: Noop backend — Answer ignored (wire meowcaller)")
	return noopSource{}, nil
}
func (Noop) Reject(context.Context, string) error { return nil }
func (Noop) Dial(context.Context, string, PCMSink) (PCMSource, string, error) {
	return noopSource{}, "", nil
}
func (Noop) Hangup(context.Context, string) error { return nil }

type noopSource struct{}

func (noopSource) Write([]float32) error { return nil }
func (noopSource) Close() error          { return nil }
