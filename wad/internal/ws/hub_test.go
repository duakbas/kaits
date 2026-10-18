package ws

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newKey is a distinct map key standing in for a connection. The hub only ever
// uses the pointer for identity, never dereferences it in these paths.
func newKey() *websocket.Conn { return &websocket.Conn{} }

// The reported symptom: a second handset with the same address and token was
// set up correctly and never synced. adopt() closed whatever was already
// connected — "newest wins" — and the first phone, which reconnects on a
// backoff and on every wake, kicked it straight back. Neither finished syncing
// and nothing said why.
func TestPushReachesEveryClient(t *testing.T) {
	h := NewHub()
	a := make(chan Envelope, 4)
	b := make(chan Envelope, 4)
	h.mu.Lock()
	h.clients[newKey()] = a
	h.clients[newKey()] = b
	h.mu.Unlock()

	if got := h.ClientCount(); got != 2 {
		t.Fatalf("ClientCount = %d, want 2", got)
	}
	h.PushT("message", map[string]any{"hello": 1})

	for name, ch := range map[string]chan Envelope{"first": a, "second": b} {
		select {
		case e := <-ch:
			if e.T != "message" {
				t.Errorf("%s client got %q", name, e.T)
			}
		case <-time.After(time.Second):
			t.Errorf("%s client got nothing; a second phone would never sync", name)
		}
	}
}

// One phone out of coverage must not cost the phone in your hand its messages.
// Each client drops from its OWN queue when full.
func TestAFullClientDoesNotStarveTheOthers(t *testing.T) {
	h := NewHub()
	slow := make(chan Envelope, 1)
	fast := make(chan Envelope, 8)
	h.mu.Lock()
	h.clients[newKey()] = slow
	h.clients[newKey()] = fast
	h.mu.Unlock()

	for i := 0; i < 5; i++ {
		h.PushT("message", map[string]any{"n": i})
	}

	if len(fast) != 5 {
		t.Errorf("the healthy client has %d frames, want 5", len(fast))
	}
	if len(slow) != 1 {
		t.Errorf("the full client holds %d frames, want its cap of 1", len(slow))
	}
}

// A disconnect removes only that client.
func TestRemovingOneClientLeavesTheRest(t *testing.T) {
	h := NewHub()
	k1, k2 := newKey(), newKey()
	h.mu.Lock()
	h.clients[k1] = make(chan Envelope, 2)
	h.clients[k2] = make(chan Envelope, 2)
	h.mu.Unlock()

	h.mu.Lock()
	delete(h.clients, k1)
	h.mu.Unlock()

	if h.attached(k1) {
		t.Error("a removed client still reports as attached")
	}
	if !h.attached(k2) {
		t.Error("removing one client detached another")
	}
	if !h.HasClient() {
		t.Error("HasClient says nobody is here while one phone is connected")
	}
}

// While the daemon says "available", WhatsApp mutes the account's other
// devices. So "active" must mean on screen, not merely connected.
func TestActiveFollowsVisibility(t *testing.T) {
	h := NewHub()
	var got []bool
	h.OnActive(func(a bool) { got = append(got, a) })
	a, b := newKey(), newKey()
	add := func(c *websocket.Conn) {
		h.mu.Lock()
		h.clients[c] = make(chan Envelope, 1)
		h.mu.Unlock()
		h.reportActive()
	}
	drop := func(c *websocket.Conn) {
		h.mu.Lock()
		delete(h.clients, c)
		delete(h.visible, c)
		h.mu.Unlock()
		h.reportActive()
	}

	add(a) // an app that hasn't said anything yet counts as on screen
	if !h.Active() {
		t.Fatal("fresh connection should count as active")
	}
	h.setVisible(a, []byte(`{"visible":false}`))
	if h.Active() {
		t.Fatal("hidden app still counted as active")
	}
	add(b)
	h.setVisible(b, []byte(`{"visible":true}`))
	h.setVisible(b, []byte(`{"visible":true}`)) // repeat: no new report
	drop(b)
	h.setVisible(a, []byte(`{"visible":false}`))

	want := []bool{true, false, true, false}
	if len(got) != len(want) {
		t.Fatalf("reports %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reports %v, want %v", got, want)
		}
	}
	h.setVisible(b, []byte(`{"visible":true}`)) // gone connection: ignored
	if h.Active() {
		t.Fatal("a disconnected app's report must not count")
	}
}
