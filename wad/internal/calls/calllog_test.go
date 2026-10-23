package calls

import (
	"context"
	"testing"
	"time"
)

type logged struct {
	id, peer, dir string
	secs          int
}

func logInto(m *Manager) chan logged {
	ch := make(chan logged, 4)
	m.OnCallLog = func(id, peer, dir string, _ time.Time, secs int) { ch <- logged{id, peer, dir, secs} }
	return ch
}

func next(t *testing.T, ch chan logged) logged {
	t.Helper()
	select {
	case l := <-ch:
		return l
	case <-time.After(time.Second):
		t.Fatal("no call-log entry")
	}
	return logged{}
}

// A call that rang and was never picked up is logged as missed (0 s).
func TestCallLogMissed(t *testing.T) {
	m, _, _ := newM()
	ch := logInto(m)
	m.NotifyIncoming("C1", "41790000001@s.whatsapp.net", "Ann", false, 0)
	m.NotifyEnded("C1", "timeout")
	if l := next(t, ch); l.id != "C1" || l.dir != "in" || l.secs != 0 || l.peer != "41790000001@s.whatsapp.net" {
		t.Errorf("logged %+v", l)
	}
}

// An outgoing call that connected is logged with its direction.
func TestCallLogOutgoingConnected(t *testing.T) {
	m, r, _ := newM()
	ch := logInto(m)
	if err := m.Dial(context.Background(), "41790000002@s.whatsapp.net", "Bob"); err != nil {
		t.Fatal(err)
	}
	if r.accepted != nil {
		r.accepted()
	}
	m.NotifyEnded("OUT1", "hangup")
	if l := next(t, ch); l.dir != "out" || l.peer != "41790000002@s.whatsapp.net" {
		t.Errorf("logged %+v", l)
	}
}
