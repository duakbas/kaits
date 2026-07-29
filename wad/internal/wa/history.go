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
	-- Nicknames the user saves from the app. Deliberately OUR table, not
	-- whatsmeow_contacts: WhatsApp has no contact-write API (the address book
	-- syncs one way, phone -> account), and anything we wrote into whatsmeow's
	-- table would be wiped by its next contact sync.
	CREATE TABLE IF NOT EXISTS local_contacts (
		jid  TEXT PRIMARY KEY,
		name TEXT,
		ts   INTEGER
	);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// Columns added after the first release. sqlite has no ADD COLUMN IF NOT
	// EXISTS, and re-adding errors harmlessly, so existing dbs upgrade in place.
	for _, col := range []string{"muted", "archived"} {
		db.Exec(`ALTER TABLE chats ADD COLUMN ` + col + ` INTEGER DEFAULT 0`)
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
	rows, err := h.db.Query(`SELECT jid,name,is_group,pinned,last_ts,preview,
		COALESCE(muted,0),COALESCE(archived,0) FROM chats ORDER BY last_ts DESC`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var jid string
		var name, preview sql.NullString
		var isGroup, pinned, ts, muted, archived sql.NullInt64
		if err := rows.Scan(&jid, &name, &isGroup, &pinned, &ts, &preview, &muted, &archived); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"jid": jid, "name": name.String, "group": isGroup.Int64 == 1,
			"pinned": pinned.Int64 == 1, "ts": ts.Int64, "preview": preview.String,
			"muted": muted.Int64 == 1, "archived": archived.Int64 == 1,
		})
	}
	return out
}

// setChatFlag updates one boolean column on a chat row. The column name is
// never user input — callers pass a literal — so it's safe to interpolate,
// which sqlite requires since identifiers can't be bound as parameters.
func (h *histStore) setChatFlag(jid, col string, on bool) {
	if h == nil || h.db == nil {
		return
	}
	switch col {
	case "pinned", "muted", "archived": // allow-list, belt and braces
	default:
		return
	}
	h.db.Exec(`UPDATE chats SET `+col+`=? WHERE jid=?`, boolToInt(on), jid)
}

// chatFlags reads back the stored pin/mute/archive state for one chat.
func (h *histStore) chatFlags(jid string) (pinned, muted, archived bool) {
	if h == nil || h.db == nil {
		return
	}
	var p, m, a sql.NullInt64
	h.db.QueryRow(`SELECT COALESCE(pinned,0),COALESCE(muted,0),COALESCE(archived,0)
		FROM chats WHERE jid=?`, jid).Scan(&p, &m, &a)
	return p.Int64 == 1, m.Int64 == 1, a.Int64 == 1
}

// lastMessage returns the newest stored message for a chat. WhatsApp's archive
// and delete-chat app-state patches carry a "message range" anchored on it.
func (h *histStore) lastMessage(chat string) (msgid string, ts int64, fromMe bool, sender string) {
	if h == nil || h.db == nil {
		return
	}
	var id, snd sql.NullString
	var t, fm sql.NullInt64
	h.db.QueryRow(`SELECT msgid,ts,fromme,sender FROM messages WHERE chat=?
		ORDER BY ts DESC LIMIT 1`, chat).Scan(&id, &t, &fm, &snd)
	return id.String, t.Int64, fm.Int64 == 1, snd.String
}

// messageByID finds a stored message by id, across chats.
//
// This is the durable counterpart to the in-memory replyCtx cache: that only
// holds messages seen live this session, so anything scrolled to out of stored
// history — or anything at all after a restart — isn't in it. The columns here
// are enough to address the sender and quote the message.
func (h *histStore) messageByID(msgid string) (chat, sender, kind, text string, fromMe, ok bool) {
	if h == nil || h.db == nil || msgid == "" {
		return
	}
	var c, s, k, t sql.NullString
	var fm sql.NullInt64
	err := h.db.QueryRow(`SELECT chat, sender, kind, text, fromme FROM messages
		WHERE msgid=? LIMIT 1`, msgid).Scan(&c, &s, &k, &t, &fm)
	if err != nil {
		return
	}
	return c.String, s.String, k.String, t.String, fm.Int64 == 1, true
}

// dropChat removes a chat and its messages from our own store. Used after a
// delete is accepted by WhatsApp, so the app's list matches the account.
func (h *histStore) dropChat(jid string) {
	if h == nil || h.db == nil {
		return
	}
	h.db.Exec(`DELETE FROM messages WHERE chat=?`, jid)
	h.db.Exec(`DELETE FROM chats WHERE jid=?`, jid)
}

// localContactName returns the nickname the user saved for a JID, or "".
func (h *histStore) localContactName(jid string) string {
	if h == nil || h.db == nil {
		return ""
	}
	var name string
	h.db.QueryRow(`SELECT name FROM local_contacts WHERE jid=? AND name<>''`, jid).Scan(&name)
	return name
}

// setLocalContact saves (or, with an empty name, clears) a nickname.
func (h *histStore) setLocalContact(jid, name string, ts int64) {
	if h == nil || h.db == nil || jid == "" {
		return
	}
	if name == "" {
		h.db.Exec(`DELETE FROM local_contacts WHERE jid=?`, jid)
		return
	}
	h.db.Exec(`INSERT INTO local_contacts (jid,name,ts) VALUES (?,?,?)
		ON CONFLICT(jid) DO UPDATE SET name=excluded.name, ts=excluded.ts`, jid, name, ts)
}

// renameChatAndMessages applies a newly saved nickname to already-stored rows,
// so saving a contact updates the chat list and past messages immediately
// instead of only affecting new traffic.
func (h *histStore) renameChatAndMessages(jid, name string) {
	if h == nil || h.db == nil || jid == "" || name == "" {
		return
	}
	h.db.Exec(`UPDATE chats SET name=? WHERE jid=?`, name, jid)
	h.db.Exec(`UPDATE messages SET sendername=? WHERE sender=? AND fromme=0`, name, jid)
	h.db.Exec(`UPDATE messages SET quoted_name=? WHERE quoted_name<>'' AND sender=?`, name, jid)
}

// BackfillQuotedNames re-resolves quoted-message authors that were stored as
// raw numbers. The reply quote bar keeps its own copy of the author name, so
// it needs its own backfill — the sender-name pass doesn't reach it.
func (h *histStore) BackfillQuotedNames(resolve func(string) string) int {
	if h == nil || h.db == nil {
		return 0
	}
	rows, err := h.db.Query(`SELECT DISTINCT quoted_name FROM messages
		WHERE quoted_name<>'' AND quoted_name GLOB '[0-9]*'`)
	if err != nil {
		return 0
	}
	var raw []string
	for rows.Next() {
		var q string
		if rows.Scan(&q) == nil {
			raw = append(raw, q)
		}
	}
	rows.Close()
	fixed := 0
	for _, num := range raw {
		name := resolve(num)
		if name == "" || isNumeric(name) {
			continue
		}
		res, _ := h.db.Exec(`UPDATE messages SET quoted_name=? WHERE quoted_name=?`, name, num)
		if res != nil {
			n, _ := res.RowsAffected()
			fixed += int(n)
		}
	}
	return fixed
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
		d.SenderJID = sender.String
		d.SenderName = sname.String
		d.Kind = kind.String
		d.Text = text.String
		d.MediaURL = media.String
		d.Mime = mime.String
		d.QuotedID = qid.String
		d.QuotedText = qtext.String
		d.QuotedName = qname.String
		d.Timestamp = ts.Int64
		d.FromMe = fromme.Int64 == 1
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
	if h == nil || h.db == nil || lid == "" {
		return
	}
	if name == "" && pn == "" {
		return
	}
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
	if h == nil || h.db == nil {
		return ""
	}
	var name string
	h.db.QueryRow(`SELECT name FROM lid_identity WHERE lid=? AND name<>''`, lid).Scan(&name)
	return name
}

// resolvePN returns the phone JID we learned for a LID, "" if we never saw one.
// This is the last-resort arm of canonicalJID, behind whatsmeow's own tables.
func (h *histStore) resolvePN(lid string) string {
	if h == nil || h.db == nil {
		return ""
	}
	var pn string
	h.db.QueryRow(`SELECT pn FROM lid_identity WHERE lid=? AND pn<>''`, lid).Scan(&pn)
	return pn
}

// BackfillSenderNames rewrites messages whose sendername is a raw number, using
// the resolver (which consults the learned table). Returns rows fixed.
func (h *histStore) BackfillSenderNames(resolve func(sender string) string) int {
	if h == nil || h.db == nil {
		return 0
	}
	rows, err := h.db.Query(`SELECT DISTINCT sender FROM messages WHERE sendername GLOB '[0-9]*' OR sendername=''`)
	if err != nil {
		return 0
	}
	var senders []string
	for rows.Next() {
		var sn string
		if rows.Scan(&sn) == nil {
			senders = append(senders, sn)
		}
	}
	rows.Close()
	fixed := 0
	for _, sn := range senders {
		name := resolve(sn)
		if name == "" {
			continue
		}
		res, _ := h.db.Exec(`UPDATE messages SET sendername=? WHERE sender=? AND (sendername GLOB '[0-9]*' OR sendername='')`, name, sn)
		if res != nil {
			n, _ := res.RowsAffected()
			fixed += int(n)
		}
	}
	return fixed
}

// BackfillChatNames rewrites chats whose stored name is missing or is just a
// raw number, using the resolver. Returns the number of chats renamed.
func (h *histStore) BackfillChatNames(resolve func(jid string) string) int {
	if h == nil || h.db == nil {
		return 0
	}
	// GLOB '[0-9]*' catches names that start with a digit — i.e. bare numbers,
	// which is exactly what an unresolved LID/phone JID leaves behind.
	rows, err := h.db.Query(`SELECT jid FROM chats WHERE name IS NULL OR name='' OR name GLOB '[0-9]*'`)
	if err != nil {
		return 0
	}
	var jids []string
	for rows.Next() {
		var j string
		if rows.Scan(&j) == nil {
			jids = append(jids, j)
		}
	}
	rows.Close()
	fixed := 0
	for _, j := range jids {
		name := resolve(j)
		if name == "" || isNumeric(name) {
			continue
		}
		res, _ := h.db.Exec(`UPDATE chats SET name=? WHERE jid=?`, name, j)
		if res != nil {
			n, _ := res.RowsAffected()
			fixed += int(n)
		}
	}
	return fixed
}

// RefreshNames rewrites stored names for anyone the resolver can now name
// authoritatively — i.e. where the user has a saved nickname or an address-book
// entry — regardless of what the row currently says.
//
// This is deliberately different from the Backfill* passes, which only touch
// names that look like bare numbers. That test misses the case that actually
// hurts: a contact stored under the name THEY chose ("Sarp Doruk Gerenli")
// when the user has them saved as something else ("bulgayrian"). Nothing about
// that stored value looks broken, so the numeric passes skip it forever.
//
// resolve must return only authoritative names (saved nickname / address book)
// and "" otherwise — never a push name — or this would happily overwrite good
// data with whatever the contact currently calls themselves.
//
// Returns (chats, messages, quotes) rows updated.
func (h *histStore) RefreshNames(resolve func(jid string) string) (int, int, int) {
	if h == nil || h.db == nil {
		return 0, 0, 0
	}
	chatsFixed, msgsFixed, quotesFixed := 0, 0, 0

	// --- message senders ---
	rows, err := h.db.Query(`SELECT DISTINCT sender, sendername FROM messages
		WHERE fromme=0 AND sender<>''`)
	if err != nil {
		return 0, 0, 0
	}
	type pair struct{ jid, name string }
	var senders []pair
	for rows.Next() {
		var j, n sql.NullString
		if rows.Scan(&j, &n) == nil {
			senders = append(senders, pair{j.String, n.String})
		}
	}
	rows.Close()

	// Old name -> new name, so quoted-reply authors can be corrected too. A
	// quote stores the author's NAME, not their JID, so a string swap is the
	// only handle we have on it.
	renames := map[string]string{}
	for _, s := range senders {
		want := resolve(s.jid)
		if want == "" || want == s.name {
			continue
		}
		res, _ := h.db.Exec(`UPDATE messages SET sendername=? WHERE sender=? AND fromme=0`,
			want, s.jid)
		if res != nil {
			n, _ := res.RowsAffected()
			msgsFixed += int(n)
		}
		if s.name != "" {
			renames[s.name] = want
		}
	}

	for old, want := range renames {
		res, _ := h.db.Exec(`UPDATE messages SET quoted_name=? WHERE quoted_name=?`, want, old)
		if res != nil {
			n, _ := res.RowsAffected()
			quotesFixed += int(n)
		}
	}

	// --- 1:1 chat titles ---
	crows, err := h.db.Query(`SELECT jid, name FROM chats WHERE COALESCE(is_group,0)=0`)
	if err != nil {
		return chatsFixed, msgsFixed, quotesFixed
	}
	var chatRows []pair
	for crows.Next() {
		var j string
		var n sql.NullString
		if crows.Scan(&j, &n) == nil {
			chatRows = append(chatRows, pair{j, n.String})
		}
	}
	crows.Close()
	for _, c := range chatRows {
		want := resolve(c.jid)
		if want == "" || want == c.name {
			continue
		}
		res, _ := h.db.Exec(`UPDATE chats SET name=? WHERE jid=?`, want, c.jid)
		if res != nil {
			n, _ := res.RowsAffected()
			chatsFixed += int(n)
		}
	}
	return chatsFixed, msgsFixed, quotesFixed
}

// MigrateLIDs rewrites @lid chats/messages to their phone JID (via resolver)
// and merges duplicate chats. Returns (seen, merged, unmapped).
func (h *histStore) MigrateLIDs(resolve func(string) (string, bool)) (int, int, int) {
	if h == nil || h.db == nil {
		return 0, 0, 0
	}
	rows, err := h.db.Query(`SELECT jid FROM chats WHERE jid LIKE '%@lid'`)
	if err != nil {
		return 0, 0, 0
	}
	var lids []string
	for rows.Next() {
		var j string
		if rows.Scan(&j) == nil {
			lids = append(lids, j)
		}
	}
	rows.Close()
	seen, merged, unmapped := 0, 0, 0
	for _, lid := range lids {
		seen++
		pn, ok := resolve(lid)
		if !ok || pn == "" || pn == lid {
			unmapped++
			continue
		}
		tx, err := h.db.Begin()
		if err != nil {
			continue
		}
		tx.Exec(`UPDATE OR IGNORE messages SET chat=? WHERE chat=?`, pn, lid)
		tx.Exec(`DELETE FROM messages WHERE chat=?`, lid)
		var exists int
		tx.QueryRow(`SELECT COUNT(*) FROM chats WHERE jid=?`, pn).Scan(&exists)
		if exists > 0 {
			tx.Exec(`UPDATE chats SET name=COALESCE(NULLIF((SELECT name FROM chats WHERE jid=?),''),name) WHERE jid=?`, lid, pn)
			tx.Exec(`UPDATE chats SET last_ts=MAX(last_ts,(SELECT last_ts FROM chats WHERE jid=?)) WHERE jid=?`, lid, pn)
			tx.Exec(`DELETE FROM chats WHERE jid=?`, lid)
		} else {
			tx.Exec(`UPDATE chats SET jid=? WHERE jid=?`, pn, lid)
		}
		if tx.Commit() == nil {
			merged++
		}
	}
	return seen, merged, unmapped
}
