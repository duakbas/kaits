package calls

import (
	"context"
	"errors"
	"testing"

	"wad/internal/ws"
)

type fakeBackend struct {
	rejectedID   string
	rejectedFrom string
	answerErr    error
	answered     bool
}

func (f *fakeBackend) Answer(context.Context, string, PCMSink) (PCMSource, error) {
	f.answered = true
	if f.answerErr != nil {
		return nil, f.answerErr
	}
	return noopSource{}, nil
}
func (f *fakeBackend) Dial(context.Context, string, PCMSink) (PCMSource, string, error) {
	return noopSource{}, "", nil
}
func (f *fakeBackend) Reject(_ context.Context, callID, fromJID string) error {
	f.rejectedID, f.rejectedFrom = callID, fromJID
	return nil
}
func (f *fakeBackend) Hangup(ctx context.Context, callID, fromJID string) error {
	return f.Reject(ctx, callID, fromJID)
}

// A reject is addressed to the caller, not to the call: whatsmeow's RejectCall
// takes both, so the manager has to have kept the JID from the offer. Losing it
// means a decline that cannot be sent, and a caller left ringing.
func TestRejectCarriesTheCallersJID(t *testing.T) {
	be := &fakeBackend{}
	m := NewManager(be, ws.NewHub())

	m.NotifyIncoming("CALL1", "41791234567@s.whatsapp.net", "Someone", false, 100)
	m.HandleAppFrame(context.Background(), ws.Envelope{T: ws.TCallReject})

	if be.rejectedID != "CALL1" {
		t.Errorf("rejected call id = %q, want CALL1", be.rejectedID)
	}
	if be.rejectedFrom != "41791234567@s.whatsapp.net" {
		t.Errorf("rejected from = %q, want the caller's JID", be.rejectedFrom)
	}

	// And the call is forgotten, or the next reject declines a call that has
	// already ended.
	be.rejectedID, be.rejectedFrom = "", ""
	m.HandleAppFrame(context.Background(), ws.Envelope{T: ws.TCallReject})
	if be.rejectedID != "" {
		t.Errorf("rejected %q after the call was already declined", be.rejectedID)
	}
}

// Answering without a media backend must tell the app the call ended, not that
// it was accepted. The old code announced "accepted" unconditionally, which on
// the phone is a call in progress and total silence — worse than being told it
// cannot be answered here.
func TestAnswerWithoutMediaReportsFailure(t *testing.T) {
	be := &fakeBackend{answerErr: errors.New("no media backend")}
	m := NewManager(be, ws.NewHub())

	m.NotifyIncoming("CALL1", "41791234567@s.whatsapp.net", "Someone", false, 100)
	if err := m.answer(context.Background()); err == nil {
		t.Error("answering with no media backend reported success")
	}
	if !be.answered {
		t.Error("the backend was never asked; the manager decided on its own")
	}
}

// NotifyEnded has to clear the caller as well as the id. A stale JID paired
// with the next call's id would address a decline to the wrong person.
func TestEndedClearsTheCaller(t *testing.T) {
	be := &fakeBackend{}
	m := NewManager(be, ws.NewHub())

	m.NotifyIncoming("CALL1", "111@s.whatsapp.net", "A", false, 100)
	m.NotifyEnded("CALL1", "timeout")
	m.HandleAppFrame(context.Background(), ws.Envelope{T: ws.TCallReject})
	if be.rejectedFrom != "" {
		t.Errorf("declined to %q after the call ended", be.rejectedFrom)
	}
}
