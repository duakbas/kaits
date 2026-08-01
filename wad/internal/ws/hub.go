package ws

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Hub manages the (single) phone connection. Serves one user's one phone, but
// must tolerate the phone reconnecting constantly (refresh, backgrounding).
//
// KEY DESIGN: each connection gets its OWN send channel. Earlier a single
// shared channel meant a superseded connection's writeLoop could grab a frame
// (e.g. the chatlist reply to getchats) and discard it on "cur != c" — so on
// refresh the reply vanished. Per-connection channels make that impossible:
// Push always targets the current connection's channel, and a dead loop only
// ever drained its own (now-abandoned) channel.
type Hub struct {
	mu       sync.Mutex
	conn     *websocket.Conn
	sendCh   chan Envelope // the CURRENT connection's send channel
	handler  func(Envelope)
	upgrader websocket.Upgrader
}

func NewHub() *Hub {
	return &Hub{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

func (h *Hub) OnFrame(fn func(Envelope)) { h.handler = fn }

// HasClient reports whether the app is currently attached.
//
// Used to decide whether a message needs a push wake-up: if the app is here,
// the frame it already received is the notification, and waking the phone on
// top of that would be noise.
func (h *Hub) HasClient() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sendCh != nil
}

// Push queues a frame to the current connection. If none is attached, the
// frame is dropped (the app re-requests on reconnect anyway).
func (h *Hub) Push(e Envelope) {
	h.mu.Lock()
	ch := h.sendCh
	h.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- e:
	default:
		// channel full — drop oldest, then enqueue
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- e:
		default:
		}
	}
}

func (h *Hub) PushT(t string, v any) {
	e, err := Frame(t, v)
	if err != nil {
		log.Printf("ws: marshal %s: %v", t, err)
		return
	}
	h.Push(e)
}

func (h *Hub) ServeHTTP(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != secret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := h.upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("ws: upgrade: %v", err)
			return
		}
		h.adopt(c)
	}
}

func (h *Hub) adopt(c *websocket.Conn) {
	// Give THIS connection its own channel.
	ch := make(chan Envelope, 512)

	h.mu.Lock()
	if h.conn != nil {
		_ = h.conn.Close() // newest wins
	}
	h.conn = c
	h.sendCh = ch
	h.mu.Unlock()

	// How long the phone stays connected is the number that decides whether
	// notifications are possible at all on this platform. KaiOS's push service
	// accepts pushes and discards them, so the only working route is the app
	// raising notifications itself over this socket — which lasts exactly as
	// long as KaiOS lets the app keep running with the phone shut. Logging the
	// session length measures that without any code on the phone.
	connectedAt := time.Now()
	log.Printf("ws: phone connected")
	go h.writeLoop(c, ch)
	reason := h.readLoop(c)
	log.Printf("ws: phone disconnected after %s", time.Since(connectedAt).Round(time.Second))
	explain(reason, time.Since(connectedAt))
}

// explain turns the close reason into the one sentence that matters. These
// three failures look identical from the phone — messages simply stop — and
// have nothing in common but the symptom, so the log should not make anyone
// squint at a errno to tell them apart.
func explain(reason string, ran time.Duration) {
	if msg := classify(reason); msg != "" {
		log.Printf("ws: ^ %s", msg)
	}
}

// classify is separate from the logging so the decision can be tested without
// capturing output.
func classify(reason string) string {
	r := strings.ToLower(reason)
	switch {
	case strings.Contains(r, "child was killed"):
		return "that was the SYSTEM KILLING THE APP — the phone needed memory " +
			"for something else and the app was the cheapest thing to take. " +
			"Not a network fault and not a crash."
	case strings.Contains(r, "no route to host"),
		strings.Contains(r, "network is unreachable"):
		return "the phone left the network — Wi-Fi asleep or out of range. The " +
			"app is probably still running; it reconnects when the radio returns."
	case strings.Contains(r, "operation timed out"),
		strings.Contains(r, "i/o timeout"):
		return "the phone stopped answering. Usually the radio going to sleep " +
			"rather than the app dying."
	}
	return ""
}

func (h *Hub) readLoop(c *websocket.Conn) string {
	c.SetReadLimit(1 << 20)
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			log.Printf("ws: read closed: %v", err)
			h.mu.Lock()
			if h.conn == c {
				h.conn = nil
				h.sendCh = nil
			}
			h.mu.Unlock()
			return err.Error()
		}
		var e Envelope
		if err := json.Unmarshal(data, &e); err != nil {
			log.Printf("ws: bad frame: %v", err)
			continue
		}
		if h.handler != nil {
			h.handler(e)
		}
	}
}

// writeLoop drains THIS connection's own channel. It never touches another
// connection's frames, so a superseded loop can't eat the live one's replies.
func (h *Hub) writeLoop(c *websocket.Conn, ch chan Envelope) {
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case e := <-ch:
			h.mu.Lock()
			cur := h.conn
			h.mu.Unlock()
			if cur != c {
				return // superseded — stop, but we only drained our OWN channel
			}
			_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.WriteJSON(e); err != nil {
				log.Printf("ws: write: %v", err)
				return
			}
		case <-ping.C:
			h.mu.Lock()
			cur := h.conn
			h.mu.Unlock()
			if cur != c {
				return
			}
			_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (h *Hub) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conn != nil
}
