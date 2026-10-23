package wa

import (
	"time"

	"go.mau.fi/whatsmeow/types"
)

// The call log behind the app's Calls tab: every finished WhatsApp call that
// went through wad, newest first. Kept here rather than read from WhatsApp,
// which has no call-history API a linked device can use.

func (h *histStore) ensureCallLog() {
	if h == nil || h.db == nil {
		return
	}
	h.db.Exec(`CREATE TABLE IF NOT EXISTS call_log (
		id       TEXT PRIMARY KEY,
		peer     TEXT,
		dir      TEXT,
		ts       INTEGER,
		secs     INTEGER
	)`)
}

// LogCall records one finished call. secs is how long it was connected; 0 for
// a call nobody picked up.
func (c *Client) LogCall(id, peer, dir string, started time.Time, secs int) {
	if c.hist == nil || c.hist.db == nil || id == "" {
		return
	}
	c.hist.ensureCallLog()
	// Stored under the phone address, so the app can open the chat from it.
	if j, err := types.ParseJID(peer); err == nil {
		peer = c.canonicalJID(j).String()
	}
	c.hist.db.Exec(`INSERT OR REPLACE INTO call_log (id,peer,dir,ts,secs) VALUES (?,?,?,?,?)`,
		id, peer, dir, started.Unix(), secs)
}

// CallEntry is one row of the Calls tab.
type CallEntry struct {
	ID   string `json:"id"`
	Peer string `json:"peer"`
	Name string `json:"name"`
	Dir  string `json:"dir"` // "in" | "out"
	TS   int64  `json:"ts"`
	Secs int    `json:"secs"` // 0: missed (in) / not answered (out)
}

// RecentCalls returns the newest calls, names resolved now (a contact saved
// after the call shows under their saved name).
func (c *Client) RecentCalls(limit int) []CallEntry {
	out := []CallEntry{}
	if c.hist == nil || c.hist.db == nil {
		return out
	}
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	c.hist.ensureCallLog()
	rows, err := c.hist.db.Query(`SELECT id,peer,dir,ts,secs FROM call_log ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var e CallEntry
		if rows.Scan(&e.ID, &e.Peer, &e.Dir, &e.TS, &e.Secs) == nil {
			out = append(out, e)
		}
	}
	for i := range out {
		out[i].Name = c.NameForJID(out[i].Peer)
	}
	return out
}
