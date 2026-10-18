package calls

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"wad/internal/ws"
)

type rec struct {
	answered, hungup, rejected []string
	dialID                     string
	accepted                   func()
	ended                      func(string)
}

type src struct{}

func (src) Write([]float32) error { return nil }
func (src) Close() error          { return nil }

func (r *rec) Answer(_ context.Context, id string, _ PCMSink) (PCMSource, error) {
	r.answered = append(r.answered, id)
	return src{}, nil
}
func (r *rec) Dial(_ context.Context, dest string, _ PCMSink) (PCMSource, string, error) {
	return src{}, r.dialID, nil
}
func (r *rec) Reject(_ context.Context, id, from string) error {
	r.rejected = append(r.rejected, id)
	return nil
}
func (r *rec) Hangup(_ context.Context, id, from string) error {
	r.hungup = append(r.hungup, id)
	return nil
}
func (r *rec) Watch(id string, a func(), e func(string)) { r.accepted, r.ended = a, e }

func env(t string, v any) ws.Envelope {
	b, _ := json.Marshal(v)
	return ws.Envelope{T: t, Data: b}
}

func newM() (*Manager, *rec, chan ws.Envelope) {
	h := ws.NewHub()
	ch := ws.Tap(h)
	r := &rec{dialID: "OUT1"}
	return NewManager(r, h), r, ch
}

func drain(ch chan ws.Envelope) []ws.Envelope {
	var out []ws.Envelope
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		case <-time.After(50 * time.Millisecond):
			return out
		}
	}
}

func has(es []ws.Envelope, t string) bool {
	for _, e := range es {
		if e.T == t {
			return true
		}
	}
	return false
}

func TestSecondCallWaitsThenSwitches(t *testing.T) {
	m, r, ch := newM()
	ctx := context.Background()
	m.NotifyIncoming("A", "111@s.whatsapp.net", "Ann", false, 0)
	m.HandleAppFrame(ctx, env(ws.TCallAnswer, map[string]string{"callid": "A"}))
	drain(ch)
	m.NotifyIncoming("B", "222@s.whatsapp.net", "Bob", false, 0)
	es := drain(ch)
	if !has(es, ws.TCallWaiting) || has(es, ws.TCallOffer) {
		t.Fatalf("second call should be call-waiting, got %v", es)
	}
	if m.activeCallID != "A" {
		t.Fatalf("active call must stay A, is %q", m.activeCallID)
	}
	m.HandleAppFrame(ctx, env(ws.TCallSwitch, map[string]string{"callid": "B"}))
	if len(r.hungup) != 1 || r.hungup[0] != "A" {
		t.Fatalf("switch must hang up A, hung up %v", r.hungup)
	}
	if r.answered[len(r.answered)-1] != "B" || m.activeCallID != "B" {
		t.Fatalf("switch must answer B; answered %v active %q", r.answered, m.activeCallID)
	}
}

func TestWaitingCallRejectedKeepsCurrent(t *testing.T) {
	m, r, ch := newM()
	ctx := context.Background()
	m.NotifyIncoming("A", "111@s.whatsapp.net", "Ann", false, 0)
	m.HandleAppFrame(ctx, env(ws.TCallAnswer, nil))
	m.NotifyIncoming("B", "222@s.whatsapp.net", "Bob", false, 0)
	drain(ch)
	m.HandleAppFrame(ctx, env(ws.TCallReject, map[string]string{"callid": "B"}))
	if len(r.rejected) != 1 || r.rejected[0] != "B" || m.activeCallID != "A" || m.waiting != nil {
		t.Fatalf("reject waiting: rejected %v active %q waiting %v", r.rejected, m.activeCallID, m.waiting)
	}
}

func TestWaitingCallerGivesUp(t *testing.T) {
	m, _, ch := newM()
	m.NotifyIncoming("A", "111@s.whatsapp.net", "Ann", false, 0)
	m.HandleAppFrame(context.Background(), env(ws.TCallAnswer, nil))
	m.NotifyIncoming("B", "222@s.whatsapp.net", "Bob", false, 0)
	drain(ch)
	m.NotifyEnded("B", "timeout")
	if m.activeCallID != "A" || m.waiting != nil {
		t.Fatalf("A must survive B timing out: active %q waiting %v", m.activeCallID, m.waiting)
	}
}

func TestGlareSwitchesToTheirCall(t *testing.T) {
	m, r, ch := newM()
	if err := m.Dial(context.Background(), "333@s.whatsapp.net", "Cem"); err != nil {
		t.Fatal(err)
	}
	drain(ch)
	m.NotifyIncoming("C", "333@s.whatsapp.net", "Cem", false, 0)
	es := drain(ch)
	var offer ws.CallData
	for _, e := range es {
		if e.T == ws.TCallOffer {
			json.Unmarshal(e.Data, &offer)
		}
	}
	if offer.CallID != "C" || !offer.AutoAnswer {
		t.Fatalf("expected auto-answer offer for C, got %+v (%v)", offer, es)
	}
	if len(r.hungup) != 1 || r.hungup[0] != "OUT1" {
		t.Fatalf("our outgoing call must be cancelled, hung up %v", r.hungup)
	}
}

func TestCallFromSomeoneElseWhileDialingWaits(t *testing.T) {
	m, r, ch := newM()
	m.Dial(context.Background(), "333@s.whatsapp.net", "Cem")
	drain(ch)
	m.NotifyIncoming("D", "444@s.whatsapp.net", "Dila", false, 0)
	es := drain(ch)
	if !has(es, ws.TCallWaiting) || len(r.hungup) != 0 || m.activeCallID != "OUT1" {
		t.Fatalf("other caller while dialing must wait: %v hung %v active %q", es, r.hungup, m.activeCallID)
	}
}

func TestDialAcceptedAndEnded(t *testing.T) {
	m, r, ch := newM()
	m.Dial(context.Background(), "333@s.whatsapp.net", "Cem")
	es := drain(ch)
	if !has(es, ws.TCallState) {
		t.Fatalf("dial should announce dialing: %v", es)
	}
	r.accepted()
	if !m.activeConnected {
		t.Fatal("accept should mark connected")
	}
	r.ended("hangup")
	if m.activeCallID != "" {
		t.Fatalf("end must clear the call, active %q", m.activeCallID)
	}
	if err := m.Dial(context.Background(), "333@s.whatsapp.net", "Cem"); err != nil {
		t.Fatalf("a new call after the end must be allowed: %v", err)
	}
}

func TestBusyRefusesSecondDial(t *testing.T) {
	m, _, _ := newM()
	m.Dial(context.Background(), "333@s.whatsapp.net", "Cem")
	if err := m.Dial(context.Background(), "444@s.whatsapp.net", "Dila"); err == nil {
		t.Fatal("dialing during a call must fail")
	}
}

func TestEndingCurrentCallRingsTheWaitingOne(t *testing.T) {
	m, _, ch := newM()
	ctx := context.Background()
	m.NotifyIncoming("A", "111@s.whatsapp.net", "Ann", false, 0)
	m.HandleAppFrame(ctx, env(ws.TCallAnswer, nil))
	m.NotifyIncoming("B", "222@s.whatsapp.net", "Bob", false, 0)
	drain(ch)
	m.HandleAppFrame(ctx, env(ws.TCallHangup, nil))
	es := drain(ch)
	if !has(es, ws.TCallOffer) || m.activeCallID != "B" {
		t.Fatalf("hanging up A must ring B: %v active %q", es, m.activeCallID)
	}
	if d, ok := m.Ringing(); !ok || d.CallID != "B" {
		t.Fatalf("B must be ringing, got %+v %v", d, ok)
	}
}

func TestRemoteEndRingsTheWaitingOne(t *testing.T) {
	m, _, ch := newM()
	m.NotifyIncoming("A", "111@s.whatsapp.net", "Ann", false, 0)
	m.HandleAppFrame(context.Background(), env(ws.TCallAnswer, nil))
	m.NotifyIncoming("B", "222@s.whatsapp.net", "Bob", false, 0)
	drain(ch)
	m.NotifyEnded("A", "hangup")
	if m.activeCallID != "B" {
		t.Fatalf("A ending must ring B, active %q", m.activeCallID)
	}
}
