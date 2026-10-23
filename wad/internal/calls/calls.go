// Package calls bridges WhatsApp calls (via meowcaller) to the phone (via
// WebRTC signalled over the ws hub).
//
// ARCHITECTURE — read this before touching anything:
//
//	WA call  <--MLow/SRTP-->  meowcaller  <--16k PCM-->  wad  <--Opus 16k-->  KaiOS app
//
// meowcaller gives and takes 16 kHz mono float32 in 960-sample frames.
// phoneleg.go owns the WebRTC session with the app and takes/gives 16 kHz;
// on the wire that is Opus (opus.go, wideband, no resampling) or, with
// WAD_CALL_CODEC=pcmu or a build without libopus, G.711 at 8 kHz. This
// file owns the state machine tying a WA CallOffer to both of those and
// forwarding SDP/ICE as ws TCallSignal frames.
//
// STATUS, by step of CALLS.md:
//
//	0-1  declining and missed calls — done, first-party whatsmeow
//	2    meowcaller server-side — built, behind WAD_CALLS=1
//	3    the phone leg — built, and testable with no account at all:
//	       curl "https://your.host/debug/call?token=$WAD_TOKEN"
//	4    joining the two — built (join.go), UNVERIFIED against a live call
//	5    outgoing — not started
//
// Every piece has tests; what nothing here can test is meowcaller against a
// real account, and Gecko 48 against pion. Step 3's tone test covers the
// second without touching the first, which is the order to do them in: the
// account risk is the only part of this that cannot be undone.
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

// PCMSink receives the peer's audio as 16 kHz mono float32 — meowcaller's
// format, and the rate everything on the WhatsApp side of the daemon is in.
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
	// Stops the 20ms pump feeding the phone — the tone on a test call, the
	// far end on a real one.
	stopPump chan struct{}
	// The WhatsApp side of a real call, closed with everything else.
	source PCMSource

	// STUN servers for the phone leg. Normally empty; see PhoneLegOptions.
	STUN []string

	// ringing holds the offer while the phone hasn't answered yet, so an app
	// that only opens because of the push notification still gets the call.
	ringing *ws.CallData

	// The active call's direction ("in"/"out"), who's on the other end, and
	// whether it has been picked up. An outgoing call that's still ringing is
	// what makes "they call me while I call them" detectable.
	activeDir       string
	activePeer      string
	activeConnected bool
	// When the active call started ringing / was picked up, for the call log.
	activeSince time.Time
	connectedAt time.Time

	// OnCallLog, if set, is told about every finished call (test calls
	// excluded): who, which way, when it started, and how long it was
	// connected (0 = missed / declined / not picked up).
	OnCallLog func(id, peer, dir string, started time.Time, secs int)

	// waiting is a second incoming call that arrived while one was going on.
	// The app shows it as call waiting; it's answered with callswitch.
	waiting *ws.CallData

	// SamePerson reports whether two JIDs are the same contact (phone vs LID
	// forms). Defaults to comparing the user part.
	SamePerson func(a, b string) bool

	// NameFor turns a caller's JID into a display name (contacts, push name).
	NameFor func(jid string) string
	// OnRing is called for every incoming WhatsApp call — used to wake a
	// closed app through push, since the offer frame only reaches a running one.
	OnRing func()
}

// Ringing reports the call currently waiting for an answer, if any.
func (m *Manager) Ringing() (ws.CallData, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ringing == nil {
		return ws.CallData{}, false
	}
	return *m.ringing, true
}

// ResendOffer re-rings an app that connected (or came on screen) while a call
// is still waiting. Harmless if the app already shows it.
func (m *Manager) ResendOffer() {
	if d, ok := m.Ringing(); ok {
		m.hub.PushT(ws.TCallOffer, d)
	}
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
	d := ws.CallData{
		CallID:    id,
		FromJID:   "test@wad.local",
		FromName:  "Tone test",
		Timestamp: time.Now().Unix(),
	}
	m.mu.Lock()
	m.activeCallID = id
	m.activeCallFrom = ""
	m.ringing = &d
	m.mu.Unlock()

	m.hub.PushT(ws.TCallOffer, d)
	log.Printf("calls: test call %s ringing", id)
	// Rings a closed app through push too, exactly like a real call — so the
	// wake-up path can be tested without anyone calling.
	if m.OnRing != nil {
		go m.OnRing()
	}
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
	if m.NameFor != nil {
		if n := m.NameFor(fromJID); n != "" {
			fromName = n
		}
	}
	d := ws.CallData{
		CallID:    callID,
		FromJID:   fromJID,
		FromName:  fromName,
		Video:     video,
		Timestamp: ts,
	}
	m.mu.Lock()
	if (m.ringing != nil && m.ringing.CallID == callID) ||
		(m.waiting != nil && m.waiting.CallID == callID) || m.activeCallID == callID {
		// whatsmeow can report the same call as both an offer and an offer
		// notice; ring once.
		m.mu.Unlock()
		return
	}
	if m.activeCallID != "" && !IsTestCall(m.activeCallID) {
		// Already in (or placing) a call.
		if m.activeDir == "out" && !m.activeConnected && m.same(m.activePeer, fromJID) {
			// We're calling them and they're calling us: both want the same
			// call. Drop ours and take theirs without making anyone press
			// anything.
			outID, outPeer := m.activeCallID, m.activePeer
			m.mu.Unlock()
			log.Printf("calls: %s called back while we were calling them — switching to their call", fromName)
			if err := m.be.Hangup(context.Background(), outID, outPeer); err != nil {
				log.Printf("calls: cancel %s: %v", outID, err)
			}
			m.endCall()
			d.AutoAnswer = true
			m.mu.Lock()
			m.activeCallID, m.activeCallFrom = callID, fromJID
			m.activeSince, m.connectedAt = time.Now(), time.Time{}
			m.activeDir, m.activePeer, m.activeConnected = "in", fromJID, false
			m.ringing = &d
			m.mu.Unlock()
			m.hub.PushT(ws.TCallOffer, d)
			return
		}
		// Anything else is call waiting: tell the app, let the user choose.
		m.waiting = &d
		m.mu.Unlock()
		log.Printf("calls: call waiting from %s", fromName)
		m.hub.PushT(ws.TCallWaiting, d)
		return
	}
	m.activeDir, m.activePeer, m.activeConnected = "in", fromJID, false
	m.activeCallID = callID
	m.activeSince, m.connectedAt = time.Now(), time.Time{}
	m.activeCallFrom = fromJID
	m.ringing = &d
	m.mu.Unlock()
	m.hub.PushT(ws.TCallOffer, d)
	log.Printf("calls: incoming call from %s", fromName)
	if m.OnRing != nil {
		go m.OnRing()
	}
	// The phone now rings. It will reply with TCallAnswer or TCallReject,
	// handled in HandleAppFrame.
}

func (m *Manager) NotifyEnded(callID, reason string) {
	m.mu.Lock()
	if m.waiting != nil && m.waiting.CallID == callID {
		// The waiting caller gave up; the current call carries on.
		m.waiting = nil
		m.mu.Unlock()
		m.hub.PushT(ws.TCallState, map[string]any{"callid": callID, "state": "ended", "reason": reason})
		return
	}
	ours := callID == m.activeCallID
	wasRinging := m.ringing != nil && m.ringing.CallID == callID
	m.mu.Unlock()
	if wasRinging && m.OnRing != nil {
		// Wake the phone again so its "incoming call" notification is replaced
		// by the current state (the missed call) instead of ringing on.
		defer func() { go m.OnRing() }()
	}
	if ours {
		// Tears down the phone leg too. The far end going away has to take the
		// WebRTC session with it, or the phone sits in a call with nobody on
		// the other side and no way to tell.
		m.endCall()
	}
	m.hub.PushT(ws.TCallState, map[string]any{"callid": callID, "state": "ended", "reason": reason})
	if ours {
		m.promoteWaiting()
	}
}

// HandleAppFrame processes call-related frames coming FROM the phone.
func (m *Manager) HandleAppFrame(ctx context.Context, e ws.Envelope) {
	switch e.T {
	case ws.TCallAnswer:
		if err := m.answer(ctx); err != nil {
			m.hub.PushT(ws.TError, map[string]string{"code": "answer", "msg": err.Error()})
		}
	case ws.TCallReject:
		var want struct {
			CallID string `json:"callid"`
		}
		_ = json.Unmarshal(e.Data, &want)
		m.mu.Lock()
		if w := m.waiting; w != nil && want.CallID == w.CallID {
			m.waiting = nil
			m.mu.Unlock()
			if err := m.be.Reject(ctx, w.CallID, w.FromJID); err != nil {
				log.Printf("calls: reject waiting %s: %v", w.CallID, err)
			}
			m.hub.PushT(ws.TCallState, map[string]any{"callid": w.CallID, "state": "ended", "reason": "rejected"})
			return
		}
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
			m.promoteWaiting()
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
			m.promoteWaiting()
		}
	case ws.TCallSwitch:
		// End what's going on and answer the waiting call. The app has
		// already armed its media side for the new call id.
		m.mu.Lock()
		w := m.waiting
		id, peer := m.activeCallID, m.activePeer
		m.waiting = nil
		m.mu.Unlock()
		if w == nil {
			return
		}
		if id != "" && !IsTestCall(id) {
			if err := m.be.Hangup(ctx, id, peer); err != nil {
				log.Printf("calls: hangup %s for switch: %v", id, err)
			}
		}
		m.endCall()
		m.mu.Lock()
		m.activeCallID, m.activeCallFrom = w.CallID, w.FromJID
		m.activeSince, m.connectedAt = time.Now(), time.Time{}
		m.activeDir, m.activePeer, m.activeConnected = "in", w.FromJID, false
		m.mu.Unlock()
		if err := m.answer(ctx); err != nil {
			m.hub.PushT(ws.TError, map[string]string{"code": "answer", "msg": err.Error()})
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
	m.stopPump = stop
	m.mu.Unlock()

	go func() {
		warble := NewWarble(440, 880, 400, LegRate)
		buf := make([]int16, LegFrame)
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
	logID, logPeer, logDir := m.activeCallID, m.activePeer, m.activeDir
	logSince, logConn := m.activeSince, m.connectedAt
	m.activeSince, m.connectedAt = time.Time{}, time.Time{}
	leg, stop, src := m.leg, m.stopPump, m.source
	m.leg, m.stopPump, m.source = nil, nil, nil
	m.activeCallID, m.activeCallFrom = "", ""
	m.activeDir, m.activePeer, m.activeConnected = "", "", false
	m.ringing = nil
	m.mu.Unlock()

	if m.OnCallLog != nil && logID != "" && !IsTestCall(logID) && logPeer != "" {
		if logSince.IsZero() {
			logSince = time.Now()
		}
		secs := 0
		if !logConn.IsZero() {
			secs = int(time.Since(logConn).Seconds())
		}
		go m.OnCallLog(logID, logPeer, logDir, logSince, secs)
	}

	// Order matters: stop producing before tearing down what consumes, or the
	// pump's next tick writes to a closed track and logs an error for
	// something that is simply a call ending normally.
	if stop != nil {
		close(stop)
	}
	if leg != nil {
		_ = leg.Close()
	}
	if src != nil {
		_ = src.Close()
	}
}

func (m *Manager) answer(ctx context.Context) error {
	m.mu.Lock()
	id := m.activeCallID
	m.ringing = nil
	m.activeConnected = true
	m.connectedAt = time.Now()
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
	// A real call: both legs, joined. answerRealCall asks the backend before
	// building anything, so a daemon with no media backend still fails here
	// rather than announcing success. The old code announced "accepted"
	// unconditionally, which on the phone is a call in progress and total
	// silence — a worse failure than being told it cannot be answered.
	if err := m.answerRealCall(ctx, id); err != nil {
		m.hub.PushT(ws.TCallState, map[string]any{
			"callid": id, "state": "ended", "reason": "no-media"})
		return err
	}
	return nil
}

// promoteWaiting turns a waiting call into a normal ringing one once the call
// it was waiting behind has ended — the phone simply rings for it now.
func (m *Manager) promoteWaiting() {
	m.mu.Lock()
	w := m.waiting
	if w == nil || m.activeCallID != "" {
		m.mu.Unlock()
		return
	}
	m.waiting = nil
	m.activeCallID, m.activeCallFrom = w.CallID, w.FromJID
	m.activeSince, m.connectedAt = time.Now(), time.Time{}
	m.activeDir, m.activePeer, m.activeConnected = "in", w.FromJID, false
	m.ringing = w
	m.mu.Unlock()
	m.hub.PushT(ws.TCallOffer, *w)
	if m.OnRing != nil {
		go m.OnRing()
	}
}

func (m *Manager) same(a, b string) bool {
	if m.SamePerson != nil {
		return m.SamePerson(a, b)
	}
	user := func(j string) string {
		if i := strings.IndexAny(j, "@:"); i >= 0 {
			return j[:i]
		}
		return j
	}
	return user(a) != "" && user(a) == user(b)
}

// CallEventer is implemented by backends that can report an outgoing call's
// progress: the far end picking up, and the call ending for any reason.
type CallEventer interface {
	Watch(callID string, accepted func(), ended func(reason string))
}

// Dial places an outgoing WhatsApp call and brings up the phone leg for it.
// The app has already shown its "calling…" screen and armed its media side.
func (m *Manager) Dial(ctx context.Context, dest, name string) error {
	m.mu.Lock()
	if m.activeCallID != "" {
		m.mu.Unlock()
		return errBusy
	}
	m.mu.Unlock()

	q := newJitter(LegRate)
	src, id, err := m.be.Dial(ctx, dest, func(pcm []float32) { q.push(f32to16(pcm)) })
	if err != nil {
		return err
	}
	if id == "" {
		_ = src.Close()
		return errNoCallID
	}
	m.mu.Lock()
	m.activeCallID, m.activeCallFrom = id, dest
	m.activeSince, m.connectedAt = time.Now(), time.Time{}
	m.activeDir, m.activePeer, m.activeConnected = "out", dest, false
	m.source = src
	m.mu.Unlock()
	if w, ok := m.be.(CallEventer); ok {
		w.Watch(id, func() {
			m.mu.Lock()
			ours := m.activeCallID == id
			if ours {
				m.activeConnected = true
				m.connectedAt = time.Now()
			}
			m.mu.Unlock()
			if ours {
				m.hub.PushT(ws.TCallState, map[string]any{"callid": id, "state": "accepted"})
			}
		}, func(reason string) { m.NotifyEnded(id, reason) })
	}
	m.hub.PushT(ws.TCallState, map[string]any{"callid": id, "state": "dialing", "name": name, "peer": dest})

	leg, err := m.startPhoneLeg(id)
	if err != nil {
		_ = m.be.Hangup(ctx, id, dest)
		m.endCall()
		return err
	}
	leg.OnPCM(func(pcm []int16) {
		if err := src.Write(i16tof32(pcm)); err != nil {
			log.Printf("calls: %s: writing to WhatsApp: %v", id, err)
		}
	})
	stop := make(chan struct{})
	m.mu.Lock()
	m.stopPump = stop
	m.mu.Unlock()
	go m.pump(id, leg, q, stop)
	log.Printf("calls: calling %s (%s)", name, id)
	return nil
}

var (
	errBusy     = errorString("already in a call")
	errNoCallID = errorString("the call backend returned no call id (is WAD_CALLS=1 set?)")
)

type errorString string

func (e errorString) Error() string { return string(e) }

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
