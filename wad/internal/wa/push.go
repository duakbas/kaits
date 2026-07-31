package wa

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Waking the phone.
//
// The app can't hold a socket open while it's closed — on KaiOS there is one
// always-on connection on the whole device, the OS's push channel, and every
// app shares it. So the daemon plays the part every other app's backend plays:
// it stays connected to WhatsApp, and when something arrives it POSTs to the
// phone's push endpoint to wake the service worker.
//
// The push carries NO payload, deliberately. KaiOS allows subscribing without
// an applicationServerKey, and without VAPID only empty pushes are permitted —
// which suits us: the app re-syncs from the daemon on connect anyway, so the
// push is a doorbell, not a delivery. Message content never passes through
// Mozilla's or KaiOS's push infrastructure, and there are no keys to manage.
//
// The service worker fetches a summary from the daemon to fill in the
// notification text; see SummaryForNotification.

// pushSub is one registered endpoint. A phone re-subscribing produces a new
// endpoint, and old ones stop working, so they're pruned when rejected.
type pushSub struct {
	Endpoint string
	Created  int64
}

// pushCoalesce is a per-CHAT window, not a global one. Every chat that gets a
// message wakes the phone straight away; only repeats within the same chat
// inside this window are folded together.
//
// It's short on purpose. A global throttle would silently swallow the second
// and third message of a lively conversation, which is exactly the ping-ping-
// ping you want to keep. Three seconds only merges messages that arrive nearly
// together, and since a chat's notification is one bubble that updates in
// place, the merged ones still show up in its count — you lose a buzz, never a
// message.
const pushCoalesce = 3 * time.Second

type pusher struct {
	mu     sync.Mutex
	last   map[string]time.Time // chat JID -> when we last woke the phone for it
	client *http.Client
}

func newPusher() *pusher {
	return &pusher{
		last:   make(map[string]time.Time),
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// AddPushSubscription records an endpoint the phone gave us.
func (c *Client) AddPushSubscription(endpoint string) error {
	if strings.TrimSpace(endpoint) == "" {
		return nil
	}
	c.hist.putPushSub(endpoint, time.Now().Unix())
	log.Printf("push: registered endpoint %s", shortenEndpoint(endpoint))
	return nil
}

// RemovePushSubscription forgets an endpoint (the app unsubscribed).
func (c *Client) RemovePushSubscription(endpoint string) {
	c.hist.deletePushSub(endpoint)
}

// notifyPush wakes the phone for an incoming message.
//
// Skipped for our own messages, for muted chats (a mute should mean the phone
// stays quiet, not just that the app draws a small icon), and when we've woken
// it recently.
func (c *Client) notifyPush(d msgSummary) {
	if d.FromMe {
		return
	}
	if _, muted, _ := c.hist.chatFlags(d.Chat); muted {
		return
	}

	c.push.mu.Lock()
	if last, ok := c.push.last[d.Chat]; ok && time.Since(last) < pushCoalesce {
		c.push.mu.Unlock()
		return
	}
	c.push.last[d.Chat] = time.Now()
	// Keep the map from growing forever in a long-running daemon.
	if len(c.push.last) > 500 {
		cutoff := time.Now().Add(-time.Hour)
		for k, v := range c.push.last {
			if v.Before(cutoff) {
				delete(c.push.last, k)
			}
		}
	}
	c.push.mu.Unlock()

	subs := c.hist.listPushSubs()
	if len(subs) == 0 {
		return
	}
	for _, s := range subs {
		go c.sendPush(s.Endpoint)
	}
}

// sendPush rings the doorbell. An empty POST is all the Push API needs to wake
// a service worker; TTL asks the push service to hold it briefly if the phone
// is unreachable rather than dropping it immediately.
func (c *Client) sendPush(endpoint string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(nil))
	if err != nil {
		return
	}
	req.Header.Set("TTL", "300")
	req.Header.Set("Content-Length", "0")

	resp, err := c.push.client.Do(req)
	if err != nil {
		log.Printf("push: %s failed: %v", shortenEndpoint(endpoint), err)
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// delivered to the push service; the phone may still be off
	case resp.StatusCode == 404 || resp.StatusCode == 410:
		// The subscription is dead — the app re-subscribed or was removed.
		// Keeping it would mean retrying forever.
		log.Printf("push: endpoint %s is gone, dropping it", shortenEndpoint(endpoint))
		c.hist.deletePushSub(endpoint)
	default:
		log.Printf("push: %s returned %d", shortenEndpoint(endpoint), resp.StatusCode)
	}
}

// msgSummary is the little the push path needs about a message.
type msgSummary struct {
	Chat   string
	FromMe bool
}

// SummaryForNotification is what the woken service worker asks for so it can
// write meaningful notifications.
//
// Returned PER CHAT rather than as one lump, because the phone shows one
// notification per conversation: group A keeps its own bubble, group B another,
// each updating in place as more arrives. A single combined summary couldn't
// express that.
//
// The body is the chat's own preview for a single message ("Alex: on my way")
// and a count once there's more than one — which is what makes a chat collapse
// into one line only after it becomes a conversation.
func (c *Client) SummaryForNotification() map[string]any {
	chats := c.chatListWithFlags()
	out := make([]map[string]any, 0, 8)
	total := 0

	for _, ch := range chats {
		n, _ := ch["unread"].(int64)
		if n <= 0 {
			continue
		}
		if muted, _ := ch["muted"].(bool); muted {
			continue // a mute means the phone stays quiet
		}
		total += int(n)

		name, _ := ch["name"].(string)
		jid, _ := ch["jid"].(string)
		if name == "" {
			name = jid
		}
		body, _ := ch["preview"].(string)
		if n > 1 {
			body = itoa(int(n)) + " new messages"
		}
		ts, _ := ch["ts"].(int64)
		out = append(out, map[string]any{
			"jid": jid, "title": name, "body": body, "count": n, "ts": ts,
		})
	}
	return map[string]any{"chats": out, "count": total}
}

// itoa avoids pulling strconv in for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// shortenEndpoint trims a push URL for logging. These are capability URLs —
// anyone holding one can wake the phone — so the full value stays out of logs.
func shortenEndpoint(e string) string {
	if i := strings.Index(e, "://"); i >= 0 {
		e = e[i+3:]
	}
	if i := strings.IndexByte(e, '/'); i >= 0 {
		host := e[:i]
		rest := e[i:]
		if len(rest) > 12 {
			rest = rest[:12] + "…"
		}
		return host + rest
	}
	return e
}

// ---- storage ----

func (h *histStore) putPushSub(endpoint string, ts int64) {
	if h == nil || h.db == nil || endpoint == "" {
		return
	}
	h.db.Exec(`INSERT INTO push_subs (endpoint, created) VALUES (?,?)
		ON CONFLICT(endpoint) DO UPDATE SET created=excluded.created`, endpoint, ts)
}

func (h *histStore) deletePushSub(endpoint string) {
	if h == nil || h.db == nil {
		return
	}
	h.db.Exec(`DELETE FROM push_subs WHERE endpoint=?`, endpoint)
}

func (h *histStore) listPushSubs() []pushSub {
	var out []pushSub
	if h == nil || h.db == nil {
		return out
	}
	rows, err := h.db.Query(`SELECT endpoint, COALESCE(created,0) FROM push_subs`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s pushSub
		var created sql.NullInt64
		if rows.Scan(&s.Endpoint, &created) != nil {
			continue
		}
		s.Created = created.Int64
		out = append(out, s)
	}
	return out
}
