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

// pushMinInterval throttles wake-ups. A burst in a busy group shouldn't mean a
// POST per message — the app pulls everything once it's awake, so one wake
// covers them all.
const pushMinInterval = 20 * time.Second

type pusher struct {
	mu       sync.Mutex
	lastSent time.Time
	client   *http.Client
}

func newPusher() *pusher {
	return &pusher{client: &http.Client{Timeout: 15 * time.Second}}
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
	if time.Since(c.push.lastSent) < pushMinInterval {
		c.push.mu.Unlock()
		return
	}
	c.push.lastSent = time.Now()
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
// write a meaningful notification. Kept to counts and a name — enough to be
// useful, nothing the OS needs to store.
func (c *Client) SummaryForNotification() map[string]any {
	chats := c.hist.listChats()
	total, chatsWithUnread := 0, 0
	newest := ""
	var newestTS int64
	for _, ch := range chats {
		n, _ := ch["unread"].(int64)
		if n <= 0 {
			continue
		}
		if muted, _ := ch["muted"].(bool); muted {
			continue
		}
		total += int(n)
		chatsWithUnread++
		if ts, _ := ch["ts"].(int64); ts >= newestTS {
			newestTS = ts
			if name, _ := ch["name"].(string); name != "" {
				newest = name
			}
		}
	}
	title := "WhatsApp"
	body := "New message"
	switch {
	case total == 0:
		body = ""
	case chatsWithUnread == 1 && newest != "":
		title = newest
		if total > 1 {
			body = itoa(total) + " new messages"
		}
	case chatsWithUnread > 1:
		body = itoa(total) + " new messages in " + itoa(chatsWithUnread) + " chats"
	}
	return map[string]any{"title": title, "body": body, "count": total}
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
