package wa

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"google.golang.org/protobuf/proto"
)

// Sending a location.
//
// Two shapes, and they are not the same thing. A pin is one message. A live
// share is a session: an opening message that declares how long it will run,
// then a stream of updates carrying an increasing sequence number and the time
// elapsed since that opening message.
//
// The session lives here rather than in the app for three reasons: this is
// where the WhatsApp connection is, this is where the sequence numbering has
// to be consistent, and this clock keeps running when the phone's screen goes
// off and the app's timers get throttled to minutes.

// SendLocation sends a one-shot location pin. name and address are optional
// labels; WhatsApp renders the coordinates either way.
func (c *Client) SendLocation(ctx context.Context, chatJID string, lat, lon float64,
	acc uint32, name, address, quotedID string) (string, error) {

	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return "", err
	}
	if err := checkCoords(lat, lon); err != nil {
		return "", err
	}

	loc := &waE2E.LocationMessage{
		DegreesLatitude:  proto.Float64(lat),
		DegreesLongitude: proto.Float64(lon),
	}
	if acc > 0 {
		loc.AccuracyInMeters = proto.Uint32(acc)
	}
	if name != "" {
		loc.Name = proto.String(name)
	}
	if address != "" {
		loc.Address = proto.String(address)
	}
	// No JPEGThumbnail: WhatsApp ships one with locations it sends, but we have
	// no map to render and no key to fetch one with. Receiving clients draw the
	// card from the coordinates, so the pin arrives either way — it just has no
	// preview image in clients that only display the shipped thumbnail.

	msg := &waE2E.Message{LocationMessage: loc}
	if quotedID != "" {
		if ci := c.quotedContext(quotedID); ci != nil {
			loc.ContextInfo = ci
		}
	}

	resp, err := c.WA.SendMessage(ctx, jid, msg)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// quotedContext builds the reply context for a stored message id, or nil if
// that message isn't in the reply cache — in which case the caller sends
// unquoted rather than failing.
func (c *Client) quotedContext(quotedID string) *waE2E.ContextInfo {
	c.replyMu.Lock()
	rc, ok := c.replyCtx[quotedID]
	c.replyMu.Unlock()
	if !ok {
		return nil
	}
	return &waE2E.ContextInfo{
		StanzaID:      proto.String(quotedID),
		Participant:   proto.String(rc.sender.String()),
		QuotedMessage: rc.msg,
	}
}

// ---- live location ----

type liveShare struct {
	chat     types.JID
	startMsg string
	startAt  time.Time
	endsAt   time.Time
	seq      int64
}

type liveShares struct {
	mu sync.Mutex
	m  map[string]*liveShare // chat JID -> session
}

func newLiveShares() *liveShares { return &liveShares{m: map[string]*liveShare{}} }

// maxLiveSecs caps a share at eight hours, which is WhatsApp's own longest
// option. A share that outlives the user's intent is a privacy problem, not a
// feature, so an unbounded or absurd duration is clamped rather than honoured.
const maxLiveSecs = 8 * 60 * 60

// StartLiveLocation opens a live share in a chat, replacing any share already
// running there. Returns the id of the opening message.
func (c *Client) StartLiveLocation(ctx context.Context, chatJID string,
	lat, lon float64, acc uint32, secs int64) (string, time.Time, error) {

	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := checkCoords(lat, lon); err != nil {
		return "", time.Time{}, err
	}
	if secs <= 0 || secs > maxLiveSecs {
		secs = maxLiveSecs
	}

	now := time.Now()
	msg := &waE2E.Message{
		LiveLocationMessage: &waE2E.LiveLocationMessage{
			DegreesLatitude:  proto.Float64(lat),
			DegreesLongitude: proto.Float64(lon),
			SequenceNumber:   proto.Int64(now.UnixMilli()),
			// TimeOffset on the opening message is how long the share will run.
			TimeOffset: proto.Uint32(uint32(secs)),
		},
	}
	if acc > 0 {
		msg.LiveLocationMessage.AccuracyInMeters = proto.Uint32(acc)
	}

	resp, err := c.WA.SendMessage(ctx, jid, msg)
	if err != nil {
		return "", time.Time{}, err
	}

	endsAt := now.Add(time.Duration(secs) * time.Second)
	c.live.mu.Lock()
	c.live.m[jid.String()] = &liveShare{
		chat:     jid,
		startMsg: resp.ID,
		startAt:  now,
		endsAt:   endsAt,
		seq:      now.UnixMilli(),
	}
	c.live.mu.Unlock()

	return resp.ID, endsAt, nil
}

// UpdateLiveLocation sends one position update for a running share. It reports
// whether the share is still running: once it has expired the caller should
// stop producing fixes, which on the phone means switching the GPS back off.
func (c *Client) UpdateLiveLocation(ctx context.Context, chatJID string,
	lat, lon float64, acc uint32) (bool, error) {

	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return false, err
	}
	if err := checkCoords(lat, lon); err != nil {
		return false, err
	}

	c.live.mu.Lock()
	s, ok := c.live.m[jid.String()]
	if !ok {
		c.live.mu.Unlock()
		return false, fmt.Errorf("no live location share running in %s", chatJID)
	}
	if time.Now().After(s.endsAt) {
		delete(c.live.m, jid.String())
		c.live.mu.Unlock()
		return false, nil
	}
	s.seq++
	seq := s.seq
	offset := uint32(time.Since(s.startAt).Seconds())
	startMsg := s.startMsg
	c.live.mu.Unlock()

	msg := &waE2E.Message{
		LiveLocationMessage: &waE2E.LiveLocationMessage{
			DegreesLatitude:  proto.Float64(lat),
			DegreesLongitude: proto.Float64(lon),
			SequenceNumber:   proto.Int64(seq),
			TimeOffset:       proto.Uint32(offset),
			// Point every update back at the message that opened the share, so
			// a client that groups them has something to group them by.
			ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String(startMsg)},
		},
	}
	if acc > 0 {
		msg.LiveLocationMessage.AccuracyInMeters = proto.Uint32(acc)
	}

	if _, err := c.WA.SendMessage(ctx, jid, msg); err != nil {
		return true, err
	}
	return true, nil
}

// StopLiveLocation ends a share. Safe to call when nothing is running, because
// the app calls it on cleanup paths where it may not know.
func (c *Client) StopLiveLocation(chatJID string) bool {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return false
	}
	c.live.mu.Lock()
	defer c.live.mu.Unlock()
	if _, ok := c.live.m[jid.String()]; !ok {
		return false
	}
	delete(c.live.m, jid.String())
	return true
}

// LiveLocationEndsAt reports when a running share expires, so a reconnecting
// app can restore its own indicator instead of losing track of a share that is
// still sending.
func (c *Client) LiveLocationEndsAt(chatJID string) (time.Time, bool) {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return time.Time{}, false
	}
	c.live.mu.Lock()
	defer c.live.mu.Unlock()
	s, ok := c.live.m[jid.String()]
	if !ok || time.Now().After(s.endsAt) {
		return time.Time{}, false
	}
	return s.endsAt, true
}

// checkCoords rejects coordinates that can't be real. 0,0 is a valid point in
// the Atlantic but is overwhelmingly a failed fix reported as a success, and
// sending it silently puts a pin off the coast of Africa in someone's chat.
func checkCoords(lat, lon float64) error {
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return fmt.Errorf("coordinates out of range: %f, %f", lat, lon)
	}
	if lat == 0 && lon == 0 {
		return fmt.Errorf("refusing to send 0,0 — that's a failed fix, not a place")
	}
	return nil
}
