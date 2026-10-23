package wa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"wad/internal/ws"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Polls: shown with live counts, and votable from the phone.
//
// A vote is not a plain message: it is encrypted with the poll's own message
// secret and names the options only by their SHA-256. whatsmeow keeps the
// secrets and does the crypto (DecryptPollVote / BuildPollVote); this file
// keeps the polls and everyone's current choice, and turns hashes back into
// option names by hashing the options we stored.

// pollOf returns whichever version of poll-creation message this is.
func pollOf(m *waE2E.Message) *waE2E.PollCreationMessage {
	for _, p := range []*waE2E.PollCreationMessage{m.GetPollCreationMessageV3(), m.GetPollCreationMessage(),
		m.GetPollCreationMessageV2(), m.GetPollCreationMessageV5(), m.GetPollCreationMessageV6()} {
		if p != nil {
			return p
		}
	}
	return nil
}

func (h *histStore) ensurePolls() {
	if h == nil || h.db == nil {
		return
	}
	h.db.Exec(`CREATE TABLE IF NOT EXISTS polls (
		msgid      TEXT PRIMARY KEY,
		chat       TEXT,  -- the chat as WhatsApp addresses it (may be a LID)
		appchat    TEXT,  -- the chat as the app knows it
		sender     TEXT,
		fromme     INTEGER,
		isgroup    INTEGER,
		name       TEXT,
		options    TEXT,  -- JSON array of option names
		selectable INTEGER
	)`)
	h.db.Exec(`CREATE TABLE IF NOT EXISTS poll_votes (
		msgid  TEXT,
		voter  TEXT,      -- "me" for our own votes
		hashes TEXT,      -- JSON array of hex SHA-256 of the chosen options
		ts     INTEGER,
		PRIMARY KEY (msgid, voter)
	)`)
}

type pollRow struct {
	chat, appChat, sender string
	fromMe, isGroup       bool
	name                  string
	options               []string
	selectable            int
}

func (h *histStore) getPoll(msgid string) (pollRow, bool) {
	var r pollRow
	if h == nil || h.db == nil {
		return r, false
	}
	h.ensurePolls()
	var opts string
	var fromMe, isGroup int
	err := h.db.QueryRow(`SELECT chat,appchat,sender,fromme,isgroup,name,options,selectable FROM polls WHERE msgid=?`, msgid).
		Scan(&r.chat, &r.appChat, &r.sender, &fromMe, &isGroup, &r.name, &opts, &r.selectable)
	if err != nil {
		return r, false
	}
	r.fromMe, r.isGroup = fromMe == 1, isGroup == 1
	_ = json.Unmarshal([]byte(opts), &r.options)
	return r, true
}

// storePoll remembers a poll as it arrives (live or from history sync).
func (c *Client) storePoll(v *events.Message, appChat string, p *waE2E.PollCreationMessage) {
	if c.hist == nil || c.hist.db == nil {
		return
	}
	c.hist.ensurePolls()
	names := make([]string, 0, len(p.GetOptions()))
	for _, o := range p.GetOptions() {
		names = append(names, o.GetOptionName())
	}
	opts, _ := json.Marshal(names)
	b := func(x bool) int {
		if x {
			return 1
		}
		return 0
	}
	c.hist.db.Exec(`INSERT OR REPLACE INTO polls (msgid,chat,appchat,sender,fromme,isgroup,name,options,selectable)
		VALUES (?,?,?,?,?,?,?,?,?)`, v.Info.ID, v.Info.Chat.String(), appChat, v.Info.Sender.ToNonAD().String(),
		b(v.Info.IsFromMe), b(v.Info.IsGroup), p.GetName(), string(opts), int(p.GetSelectableOptionsCount()))
}

func (h *histStore) putVote(msgid, voter string, hashes [][]byte, ts int64) {
	hx := make([]string, len(hashes))
	for i, x := range hashes {
		hx[i] = hex.EncodeToString(x)
	}
	j, _ := json.Marshal(hx)
	h.db.Exec(`INSERT INTO poll_votes (msgid,voter,hashes,ts) VALUES (?,?,?,?)
		ON CONFLICT(msgid,voter) DO UPDATE SET hashes=excluded.hashes, ts=excluded.ts
		WHERE excluded.ts >= poll_votes.ts`, msgid, voter, string(j), ts)
}

// PollData tallies a stored poll for the app. nil if the poll isn't stored.
func (c *Client) PollData(msgid string) *ws.PollData {
	p, ok := c.hist.getPoll(msgid)
	if !ok {
		return nil
	}
	out := &ws.PollData{Name: p.name, Multi: p.selectable != 1}
	idx := map[string]int{}
	for i, n := range p.options {
		sum := sha256.Sum256([]byte(n))
		idx[hex.EncodeToString(sum[:])] = i
		out.Options = append(out.Options, ws.PollOption{Name: n})
	}
	rows, err := c.hist.db.Query(`SELECT voter, hashes FROM poll_votes WHERE msgid=?`, msgid)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var voter, hj string
		if rows.Scan(&voter, &hj) != nil {
			continue
		}
		var hs []string
		_ = json.Unmarshal([]byte(hj), &hs)
		if len(hs) > 0 {
			out.Voters++
		}
		for _, h := range hs {
			if i, ok := idx[h]; ok {
				out.Options[i].Votes++
				if voter == "me" {
					out.Options[i].Mine = true
				}
			}
		}
	}
	return out
}

// handlePollVote decrypts someone's vote (or our own, from another device),
// records it and tells the app.
func (c *Client) handlePollVote(v *events.Message) {
	pu := v.Message.GetPollUpdateMessage()
	if pu == nil {
		return
	}
	id := pu.GetPollCreationMessageKey().GetID()
	p, ok := c.hist.getPoll(id)
	if !ok {
		return // a vote in a poll we never saw; nothing to show it on
	}
	vote, err := c.WA.DecryptPollVote(context.Background(), v)
	if err != nil {
		log.Printf("poll %s: can't decrypt a vote: %v", id, err)
		return
	}
	voter := "me"
	if !v.Info.IsFromMe {
		voter = c.canonicalJID(v.Info.Sender).String()
	}
	ts := pu.GetSenderTimestampMS()
	if ts == 0 {
		ts = v.Info.Timestamp.UnixMilli()
	}
	c.hist.putVote(id, voter, vote.GetSelectedOptions(), ts)
	c.hub.PushT(ws.TPollUpdate, map[string]any{"chat": p.appChat, "msgid": id, "poll": c.PollData(id)})
}

// VotePoll sends our choice. An empty list withdraws the vote.
func (c *Client) VotePoll(ctx context.Context, msgid string, names []string) error {
	p, ok := c.hist.getPoll(msgid)
	if !ok {
		return fmt.Errorf("poll %s is not stored", msgid)
	}
	chat, err := types.ParseJID(p.chat)
	if err != nil {
		return err
	}
	sender, err := types.ParseJID(p.sender)
	if err != nil {
		return err
	}
	if p.selectable == 1 && len(names) > 1 {
		names = names[:1]
	}
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: p.fromMe, IsGroup: p.isGroup},
		ID:            types.MessageID(msgid),
	}
	msg, err := c.WA.BuildPollVote(ctx, info, names)
	if err != nil {
		return err
	}
	if _, err := c.WA.SendMessage(ctx, chat, msg); err != nil {
		return err
	}
	hashes := make([][]byte, len(names))
	for i, n := range names {
		s := sha256.Sum256([]byte(n))
		hashes[i] = s[:]
	}
	c.hist.putVote(msgid, "me", hashes, time.Now().UnixMilli())
	c.hub.PushT(ws.TPollUpdate, map[string]any{"chat": p.appChat, "msgid": msgid, "poll": c.PollData(msgid)})
	return nil
}

// attachPolls fills in the tally for poll messages loaded from history.
func (c *Client) attachPolls(msgs []ws.MsgData) []ws.MsgData {
	for i := range msgs {
		if msgs[i].Kind == "poll" {
			msgs[i].Poll = c.PollData(msgs[i].MsgID)
		}
	}
	return msgs
}
