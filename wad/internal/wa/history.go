package wa

import (
	"database/sql"
	"fmt"

	"wad/internal/ws"

	_ "github.com/mattn/go-sqlite3"
)

// histStore is a persistence layer the daemon owns — separate from whatsmeow's
// session db. It stores normalized messages + chats so the app has real history
// on launch and across restarts, instead of only seeing live messages.
type histStore struct {
	db *sql.DB
}

func openHistStore(path string) (*histStore, error) {
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000", path))
	if err != nil {
		return nil, err
	}
	// A single shared connection so reads always see the writer's WAL pages.
	// Without this, Go's connection pool can hand ListChats a stale snapshot
	// that misses everything sitting in the -wal file (the "no chats on refresh"
	// bug: capture writes on one pooled conn, getchats reads an empty one).
	db.SetMaxOpenConns(1)
	schema := `
	CREATE TABLE IF NOT EXISTS chats (
		jid        TEXT PRIMARY KEY,
		name       TEXT,
		is_group   INTEGER,
		pinned     INTEGER,
		last_ts    INTEGER,
		preview    TEXT
	);
	CREATE TABLE IF NOT EXISTS messages (
		msgid       TEXT,
		chat        TEXT,
		sender      TEXT,
		sendername  TEXT,
		fromme      INTEGER,
		ts          INTEGER,
		kind        TEXT,
		text        TEXT,
		media       TEXT,
		mime        TEXT,
		quoted_id   TEXT,
		quoted_text TEXT,
		quoted_name TEXT,
		PRIMARY KEY (chat, msgid)
	);
	CREATE INDEX IF NOT EXISTS idx_messages_chat_ts ON messages(chat, ts);
	CREATE TABLE IF NOT EXISTS lid_identity (
		lid  TEXT PRIMARY KEY,
		name TEXT,
		pn   TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_lid_name ON lid_identity(name);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &histStore{db: db}, nil
}

func (h *histStore) close() {
	if h.db != nil {
		h.db.Close()
	}
}

// putMessage upserts one normalized message and updates its chat's summary.
func (h *histStore) putMessage(d ws.MsgData) {
	if h == nil || h.db == nil {
		return
	}
	_, err := h.db.Exec(`
		INSERT OR REPLACE INTO messages
		(msgid, chat, sender, sendername, fromme, ts, kind, text, media, mime, quoted_id, quoted_text, quoted_name)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.MsgID, d.ChatJID, d.SenderJID, d.SenderName, boolToInt(d.FromMe), d.Timestamp,
		d.Kind, d.Text, d.MediaURL, d.Mime, d.QuotedID, d.QuotedText, d.QuotedName)
	if err != nil {
		return
	}
	preview := d.Text
	if d.Kind != "text" {
		preview = "[" + d.Kind + "]"
	}
	if d.IsGroup && !d.FromMe && d.SenderName != "" {
		preview = d.SenderName + ": " + preview
	}
	h.db.Exec(`
		INSERT INTO chats (jid, name, is_group, pinned, last_ts, preview)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(jid) DO UPDATE SET
			name=excluded.name,
			is_group=excluded.is_group,
			pinned=excluded.pinned,
			last_ts=MAX(chats.last_ts, excluded.last_ts),
			preview=CASE WHEN excluded.last_ts >= chats.last_ts THEN excluded.preview ELSE chats.preview END`,
		d.ChatJID, d.ChatName, boolToInt(d.IsGroup), boolToInt(d.Pinned), d.Timestamp, preview)
}

// listChats returns all known chats, newest first (pinned handled app-side).
func (h *histStore) listChats() []map[string]any {
	out := []map[string]any{}
	if h == nil || h.db == nil {
		return out
	}
	rows, err := h.db.Query(`SELECT jid,name,is_group,pinned,last_ts,preview FROM chats ORDER BY last_ts DESC`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var jid string
		var name, preview sql.NullString
		var isGroup, pinned, ts sql.NullInt64
		if err := rows.Scan(&jid, &name, &isGroup, &pinned, &ts, &preview); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"jid": jid, "name": name.String, "group": isGroup.Int64 == 1,
			"pinned": pinned.Int64 == 1, "ts": ts.Int64, "preview": preview.String,
		})
	}
	return out
}

// history returns messages for a chat, oldest->newest, up to limit, with an
// optional beforeTS cursor for loading older pages (0 = most recent page).
func (h *histStore) history(chat string, beforeTS int64, limit int) []ws.MsgData {
	out := []ws.MsgData{}
	if h == nil || h.db == nil {
		return out
	}
	if limit <= 0 {
		limit = 40
	}
	q := `SELECT msgid,chat,sender,sendername,fromme,ts,kind,text,media,mime,quoted_id,quoted_text,quoted_name
	      FROM messages WHERE chat=?`
	args := []any{chat}
	if beforeTS > 0 {
		q += ` AND ts < ?`
		args = append(args, beforeTS)
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)

	rows, err := h.db.Query(q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	// collected newest-first; reverse to oldest-first for display
	var tmp []ws.MsgData
	for rows.Next() {
		var d ws.MsgData
		var fromme, ts sql.NullInt64
		var sender, sname, text, media, mime, qid, qtext, qname, kind sql.NullString
		if err := rows.Scan(&d.MsgID, &d.ChatJID, &sender, &sname, &fromme,
			&ts, &kind, &text, &media, &mime,
			&qid, &qtext, &qname); err != nil {
			continue
		}
		d.SenderJID=sender.String; d.SenderName=sname.String; d.Kind=kind.String
		d.Text=text.String; d.MediaURL=media.String; d.Mime=mime.String
		d.QuotedID=qid.String; d.QuotedText=qtext.String; d.QuotedName=qname.String
		d.Timestamp=ts.Int64; d.FromMe = fromme.Int64 == 1
		tmp = append(tmp, d)
	}
	for i := len(tmp) - 1; i >= 0; i-- {
		out = append(out, tmp[i])
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// learnLID records what we know about a LID: a display name and/or its phone
// JID. Accumulates identity knowledge beyond whatsmeow's own map so the same
// person's many per-group LIDs can be unified by name.
func (h *histStore) learnLID(lid, name, pn string) {
	if h == nil || h.db == nil || lid == "" { return }
	if name == "" && pn == "" { return }
	h.db.Exec(`INSERT INTO lid_identity (lid,name,pn) VALUES (?,?,?)
		ON CONFLICT(lid) DO UPDATE SET
			name=CASE WHEN excluded.name<>'' THEN excluded.name ELSE lid_identity.name END,
			pn=CASE WHEN excluded.pn<>'' THEN excluded.pn ELSE lid_identity.pn END`,
		lid, name, pn)
}

// resolveLIDName returns a known name for a LID: first a direct hit, else any
// other LID we've seen carrying a phone JID under the same name (aggressive
// unification by display name).
func (h *histStore) resolveLIDName(lid string) string {
	if h == nil || h.db == nil { return "" }
	var name string
	h.db.QueryRow(`SELECT name FROM lid_identity WHERE lid=? AND name<>''`, lid).Scan(&name)
	return name
}

// BackfillSenderNames rewrites messages whose sendername is a raw number, using
// the resolver (which consults the learned table). Returns rows fixed.
func (h *histStore) BackfillSenderNames(resolve func(sender string) string) int {
	if h == nil || h.db == nil { return 0 }
	rows, err := h.db.Query(`SELECT DISTINCT sender FROM messages WHERE sendername GLOB '[0-9]*' OR sendername=''`)
	if err != nil { return 0 }
	var senders []string
	for rows.Next() { var sn string; if rows.Scan(&sn)==nil { senders=append(senders,sn) } }
	rows.Close()
	fixed:=0
	for _, sn := range senders {
		name := resolve(sn)
		if name=="" { continue }
		res,_ := h.db.Exec(`UPDATE messages SET sendername=? WHERE sender=? AND (sendername GLOB '[0-9]*' OR sendername='')`, name, sn)
		if res!=nil { n,_:=res.RowsAffected(); fixed+=int(n) }
	}
	return fixed
}

// MigrateLIDs rewrites @lid chats/messages to their phone JID (via resolver)
// and merges duplicate chats. Returns (seen, merged, unmapped).
func (h *histStore) MigrateLIDs(resolve func(string) (string, bool)) (int, int, int) {
	if h == nil || h.db == nil { return 0,0,0 }
	rows, err := h.db.Query(`SELECT jid FROM chats WHERE jid LIKE '%@lid'`)
	if err != nil { return 0,0,0 }
	var lids []string
	for rows.Next() { var j string; if rows.Scan(&j)==nil { lids=append(lids,j) } }
	rows.Close()
	seen,merged,unmapped := 0,0,0
	for _, lid := range lids {
		seen++
		pn, ok := resolve(lid)
		if !ok || pn=="" || pn==lid { unmapped++; continue }
		tx, err := h.db.Begin(); if err!=nil { continue }
		tx.Exec(`UPDATE OR IGNORE messages SET chat=? WHERE chat=?`, pn, lid)
		tx.Exec(`DELETE FROM messages WHERE chat=?`, lid)
		var exists int
		tx.QueryRow(`SELECT COUNT(*) FROM chats WHERE jid=?`, pn).Scan(&exists)
		if exists>0 {
			tx.Exec(`UPDATE chats SET name=COALESCE(NULLIF((SELECT name FROM chats WHERE jid=?),''),name) WHERE jid=?`, lid, pn)
			tx.Exec(`UPDATE chats SET last_ts=MAX(last_ts,(SELECT last_ts FROM chats WHERE jid=?)) WHERE jid=?`, lid, pn)
			tx.Exec(`DELETE FROM chats WHERE jid=?`, lid)
		} else {
			tx.Exec(`UPDATE chats SET jid=? WHERE jid=?`, pn, lid)
		}
		if tx.Commit()==nil { merged++ }
	}
	return seen,merged,unmapped
}
