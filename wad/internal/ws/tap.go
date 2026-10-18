package ws

import "github.com/gorilla/websocket"

// Tap attaches a pretend client and returns the channel its frames go to.
// For tests: it lets them see exactly what the app would be sent.
func Tap(h *Hub) chan Envelope {
	ch := make(chan Envelope, 256)
	h.mu.Lock()
	h.clients[&websocket.Conn{}] = ch
	h.mu.Unlock()
	return ch
}
