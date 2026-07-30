package wa

import (
	"database/sql"
	"fmt"
	"regexp"

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
	-- Everything needed to re-download one attachment from WhatsApp's CDN.
	-- Without this, media only worked while the message that carried it was
	-- still in the in-memory cache, so every photo broke on daemon restart.
	-- These are decryption keys for blobs already on the CDN, not the account
	-- session, but they still belong to the user's messages — this file is
	-- gitignored for the same reason the rest of it is.
	CREATE TABLE IF NOT EXISTS media_keys (
		msgid       TEXT PRIMARY KEY,
		direct_path TEXT,
		enc_sha256  BLOB,
		sha256      BLOB,
		media_key   BLOB,
		media_type  TEXT,
		mime        TEXT
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
	db.Exec(`ALTER TABLE messages ADD COLUMN forwarded INTEGER DEFAULT 0`)
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
		(msgid, chat, sender, sendername, fromme, ts, kind, text, media, mime, quoted_id, quoted_text, quoted_name, forwarded)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.MsgID, d.ChatJID, d.SenderJID, d.SenderName, boolToInt(d.FromMe), d.Timestamp,
		d.Kind, d.Text, d.MediaURL, d.Mime, d.QuotedID, d.QuotedText, d.QuotedName,
		boolToInt(d.Forwarded))
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

// mediaRef is the persisted form of a downloadable attachment.
type mediaRef struct {
	directPath string
	encSHA256  []byte
	sha256     []byte
	mediaKey   []byte
	mediaType  string
	mime       string
}

// putMediaRef stores the keys for one attachment so it stays fetchable across
// restarts.
func (h *histStore) putMediaRef(msgid string, r mediaRef) {
	if h == nil || h.db == nil || msgid == "" || r.directPath == "" {
		return
	}
	h.db.Exec(`INSERT INTO media_keys
		(msgid, direct_path, enc_sha256, sha256, media_key, media_type, mime)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(msgid) DO UPDATE SET
			direct_path=excluded.direct_path, enc_sha256=excluded.enc_sha256,
			sha256=excluded.sha256, media_key=excluded.media_key,
			media_type=excluded.media_type, mime=excluded.mime`,
		msgid, r.directPath, r.encSHA256, r.sha256, r.mediaKey, r.mediaType, r.mime)
}

// mediaRefFor loads an attachment's stored keys.
func (h *histStore) mediaRefFor(msgid string) (mediaRef, bool) {
	if h == nil || h.db == nil || msgid == "" {
		return mediaRef{}, false
	}
	var r mediaRef
	var dp, mt, mime sql.NullString
	err := h.db.QueryRow(`SELECT direct_path, enc_sha256, sha256, media_key, media_type, mime
		FROM media_keys WHERE msgid=?`, msgid).
		Scan(&dp, &r.encSHA256, &r.sha256, &r.mediaKey, &mt, &mime)
	if err != nil || dp.String == "" {
		return mediaRef{}, false
	}
	r.directPath, r.mediaType, r.mime = dp.String, mt.String, mime.String
	return r, true
}

// mediaGap is one stored attachment we can't currently download, because its
// keys were never captured.
type mediaGap struct {
	Chat   string
	MsgID  string
	TS     int64
	FromMe bool
}

// mediaMessagesWithoutKeys lists attachments that have no stored keys, newest
// first. These are the messages an on-demand history re-fetch needs to cover.
func (h *histStore) mediaMessagesWithoutKeys() []mediaGap {
	if h == nil || h.db == nil {
		return nil
	}
	rows, err := h.db.Query(`
		SELECT m.chat, m.msgid, m.ts, m.fromme FROM messages m
		WHERE m.kind IN ('image','video','audio','gif','sticker','doc')
		  AND NOT EXISTS (SELECT 1 FROM media_keys k WHERE k.msgid = m.msgid)
		ORDER BY m.ts DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []mediaGap
	for rows.Next() {
		var g mediaGap
		var fm sql.NullInt64
		var ts sql.NullInt64
		if rows.Scan(&g.Chat, &g.MsgID, &ts, &fm) != nil {
			continue
		}
		g.TS, g.FromMe = ts.Int64, fm.Int64 == 1
		out = append(out, g)
	}
	return out
}

// countStoredMediaKeys is how many attachments are currently downloadable.
func (h *histStore) countStoredMediaKeys() int {
	if h == nil || h.db == nil {
		return 0
	}
	var n int
	h.db.QueryRow(`SELECT COUNT(*) FROM media_keys`).Scan(&n)
	return n
}

// mimeForMessage returns a stored message's mime type, for serving media after
// the in-memory table is gone.
func (h *histStore) mimeForMessage(msgid string) string {
	if h == nil || h.db == nil || msgid == "" {
		return ""
	}
	var mime sql.NullString
	if h.db.QueryRow(`SELECT mime FROM media_keys WHERE msgid=?`, msgid).Scan(&mime) == nil && mime.String != "" {
		return mime.String
	}
	h.db.QueryRow(`SELECT mime FROM messages WHERE msgid=? LIMIT 1`, msgid).Scan(&mime)
	return mime.String
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
	q := `SELECT msgid,chat,sender,sendername,fromme,ts,kind,text,media,mime,quoted_id,quoted_text,quoted_name,COALESCE(forwarded,0)
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
		var fromme, ts, fwd sql.NullInt64
		var sender, sname, text, media, mime, qid, qtext, qname, kind sql.NullString
		if err := rows.Scan(&d.MsgID, &d.ChatJID, &sender, &sname, &fromme,
			&ts, &kind, &text, &media, &mime,
			&qid, &qtext, &qname, &fwd); err != nil {
			continue
		}
		d.Forwarded = fwd.Int64 == 1
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

// markUnsavedNames prefixes "~" onto stored names belonging to people the user
// hasn't saved.
//
// RefreshNames can't do this: it only ever writes authoritative names, and by
// definition there is none for an unsaved contact — so rows holding a push name
// ("David Cannone") were left unmarked. This pass changes no name, it only adds
// the marker, and only for people isSaved confirms are unsaved. Rows already
// marked, empty, or still bare numbers are skipped.
//
// Returns (chats, messages, quotes) rows updated.
func (h *histStore) markUnsavedNames(isSaved func(jid string) bool) (int, int, int) {
	if h == nil || h.db == nil {
		return 0, 0, 0
	}
	chatsFixed, msgsFixed, quotesFixed := 0, 0, 0

	rows, err := h.db.Query(`SELECT DISTINCT sender, sendername FROM messages
		WHERE fromme=0 AND sender<>'' AND sendername<>'' AND sendername NOT LIKE '~%'`)
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

	renames := map[string]string{}
	for _, s := range senders {
		// A bare number isn't a name; leave it for the name passes to resolve.
		if isNumeric(s.name) || isSaved(s.jid) {
			continue
		}
		want := "~" + s.name
		res, _ := h.db.Exec(`UPDATE messages SET sendername=? WHERE sender=? AND fromme=0
			AND sendername=?`, want, s.jid, s.name)
		if res != nil {
			n, _ := res.RowsAffected()
			msgsFixed += int(n)
		}
		renames[s.name] = want
	}

	for old, want := range renames {
		res, _ := h.db.Exec(`UPDATE messages SET quoted_name=? WHERE quoted_name=?`, want, old)
		if res != nil {
			n, _ := res.RowsAffected()
			quotesFixed += int(n)
		}
	}

	crows, err := h.db.Query(`SELECT jid, name FROM chats
		WHERE COALESCE(is_group,0)=0 AND name<>'' AND name NOT LIKE '~%'`)
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
		if isNumeric(c.name) || isSaved(c.jid) {
			continue
		}
		res, _ := h.db.Exec(`UPDATE chats SET name=? WHERE jid=?`, "~"+c.name, c.jid)
		if res != nil {
			n, _ := res.RowsAffected()
			chatsFixed += int(n)
		}
	}
	return chatsFixed, msgsFixed, quotesFixed
}

// rewriteMessageText runs a regexp replacement over every stored message body
// that matches, using resolve to turn each captured group into a replacement.
// resolve returning "" leaves that occurrence alone. Returns rows changed.
func (h *histStore) rewriteMessageText(re *regexp.Regexp, resolve func(string) string) int {
	if h == nil || h.db == nil {
		return 0
	}
	rows, err := h.db.Query(`SELECT chat, msgid, text FROM messages
		WHERE text LIKE '%@%' AND text<>''`)
	if err != nil {
		return 0
	}
	type row struct{ chat, msgid, text string }
	var pending []row
	for rows.Next() {
		var c, m string
		var t sql.NullString
		if rows.Scan(&c, &m, &t) == nil && t.String != "" {
			pending = append(pending, row{c, m, t.String})
		}
	}
	rows.Close()

	fixed := 0
	// Cache resolutions: the same handful of people get mentioned repeatedly,
	// and each miss would otherwise re-query the contact tables per message.
	seen := map[string]string{}
	for _, r := range pending {
		out := re.ReplaceAllStringFunc(r.text, func(match string) string {
			digits := re.FindStringSubmatch(match)[1]
			rep, known := seen[digits]
			if !known {
				rep = resolve(digits)
				seen[digits] = rep
			}
			if rep == "" {
				return match
			}
			return rep
		})
		if out == r.text {
			continue
		}
		res, _ := h.db.Exec(`UPDATE messages SET text=? WHERE chat=? AND msgid=?`,
			out, r.chat, r.msgid)
		if res != nil {
			n, _ := res.RowsAffected()
			fixed += int(n)
		}
	}
	return fixed
}

// rebuildPreviews regenerates every chat's list preview from its newest stored
// message.
//
// Previews are denormalized (the "Name: body" string is written at insert time),
// so a name repair that fixes the messages table leaves the chat list still
// showing whatever it said before — including bare LID numbers. Returns rows
// changed.
func (h *histStore) rebuildPreviews() int {
	if h == nil || h.db == nil {
		return 0
	}
	rows, err := h.db.Query(`
		SELECT c.jid, COALESCE(c.is_group,0), COALESCE(c.preview,''),
		       m.sendername, m.fromme, m.kind, m.text
		FROM chats c
		JOIN messages m ON m.chat = c.jid
		WHERE m.ts = (SELECT MAX(ts) FROM messages WHERE chat = c.jid)`)
	if err != nil {
		return 0
	}
	type row struct {
		jid, oldPreview, sender, kind, text string
		isGroup, fromMe                     bool
	}
	var pending []row
	for rows.Next() {
		var jid, oldPrev string
		var grp, fromMe sql.NullInt64
		var sname, kind, text sql.NullString
		if rows.Scan(&jid, &grp, &oldPrev, &sname, &fromMe, &kind, &text) != nil {
			continue
		}
		pending = append(pending, row{
			jid: jid, oldPreview: oldPrev, sender: sname.String,
			kind: kind.String, text: text.String,
			isGroup: grp.Int64 == 1, fromMe: fromMe.Int64 == 1,
		})
	}
	rows.Close()

	fixed := 0
	for _, r := range pending {
		body := r.text
		if r.kind != "text" {
			body = "[" + r.kind + "]"
		}
		if r.isGroup && !r.fromMe && r.sender != "" {
			body = r.sender + ": " + body
		}
		if body == r.oldPreview {
			continue
		}
		res, _ := h.db.Exec(`UPDATE chats SET preview=? WHERE jid=?`, body, r.jid)
		if res != nil {
			n, _ := res.RowsAffected()
			fixed += int(n)
		}
	}
	return fixed
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
