package wa

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

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
	-- One row per person per message. WhatsApp models a reaction as a message
	-- carrying the target's key, and re-reacting replaces the previous emoji —
	-- hence the primary key, and why an empty emoji means "removed".
	CREATE TABLE IF NOT EXISTS reactions (
		chat   TEXT,
		msgid  TEXT,
		sender TEXT,
		name   TEXT,
		emoji  TEXT,
		ts     INTEGER,
		PRIMARY KEY (chat, msgid, sender)
	);
	CREATE INDEX IF NOT EXISTS idx_reactions_msg ON reactions(chat, msgid);
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
	// Delivery state for our own outgoing messages: "" (sent) -> delivered -> read.
	db.Exec(`ALTER TABLE messages ADD COLUMN status TEXT DEFAULT ''`)
	// Location payload, kept on the message rather than in a side table since
	// it's four small scalars and always wanted with the message.
	for _, col := range []string{"lat REAL", "lon REAL", "loc_name TEXT", "loc_address TEXT"} {
		db.Exec(`ALTER TABLE messages ADD COLUMN ` + col)
	}
	// WhatsApp ships a rendered map preview with every location message, so the
	// app can show a real map with no external request and no API key.
	db.Exec(`CREATE TABLE IF NOT EXISTS location_thumbs (
		msgid TEXT PRIMARY KEY,
		jpeg  BLOB
	)`)
	// Converted animated stickers. The conversion is an ffmpeg run of a second
	// or two, and a sticker is looked at every time its thread is opened, so
	// doing it once is the difference between a bubble that appears and one
	// that arrives late every single time.
	// Favourite stickers, learned from app state. Only the id and when it was
	// favourited: the downloadable part lives in media_keys under "fav:<id>",
	// which is what lets every existing media path serve and send one without
	// knowing it is not a message.
	db.Exec(`CREATE TABLE IF NOT EXISTS favorite_stickers (
		id TEXT PRIMARY KEY,
		ts INTEGER
	)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS sticker_gifs (
		msgid TEXT PRIMARY KEY,
		gif   BLOB,
		ts    INTEGER
	)`)
	// Unread counts are derived from messages lacking a "read" status, so this
	// index keeps the per-chat count cheap over a large history.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_unread ON messages(chat, fromme, status)`)
	// Small key/value table for one-time upgrade steps.
	db.Exec(`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT)`)
	// Push endpoints the phone has registered. These are capability URLs —
	// holding one lets you wake the device — so they live here with the rest of
	// the gitignored state, never in the repo.
	db.Exec(`CREATE TABLE IF NOT EXISTS push_subs (
		endpoint TEXT PRIMARY KEY,
		created  INTEGER
	)`)

	// Every message stored before read-tracking existed has an empty status,
	// which the unread count would read as "never read" — surfacing tens of
	// thousands of unread messages the moment counts appeared. Treat existing
	// history as already seen, once.
	var done sql.NullString
	db.QueryRow(`SELECT value FROM meta WHERE key='unread_baseline'`).Scan(&done)
	if done.String == "" {
		db.Exec(`UPDATE messages SET status='read' WHERE fromme=0 AND COALESCE(status,'')=''`)
		db.Exec(`INSERT OR REPLACE INTO meta (key,value) VALUES ('unread_baseline','1')`)
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
		(msgid, chat, sender, sendername, fromme, ts, kind, text, media, mime, quoted_id, quoted_text, quoted_name, forwarded, lat, lon, loc_name, loc_address)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.MsgID, d.ChatJID, d.SenderJID, d.SenderName, boolToInt(d.FromMe), d.Timestamp,
		d.Kind, d.Text, d.MediaURL, d.Mime, d.QuotedID, d.QuotedText, d.QuotedName,
		boolToInt(d.Forwarded), d.Lat, d.Lon, d.LocName, d.LocAddress)
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
	// Unread is derived, not stored: it's the incoming messages this chat has
	// that we never marked read. That means it survives restarts and browser
	// refreshes for free, and can't drift out of step with the read receipts we
	// actually sent.
	rows, err := h.db.Query(`SELECT c.jid,c.name,c.is_group,c.pinned,c.last_ts,c.preview,
		COALESCE(c.muted,0),COALESCE(c.archived,0),
		(SELECT COUNT(*) FROM messages m
		   WHERE m.chat=c.jid AND m.fromme=0 AND COALESCE(m.status,'')<>'read')
		FROM chats c ORDER BY c.last_ts DESC`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var jid string
		var name, preview sql.NullString
		var isGroup, pinned, ts, muted, archived, unread sql.NullInt64
		if err := rows.Scan(&jid, &name, &isGroup, &pinned, &ts, &preview,
			&muted, &archived, &unread); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"jid": jid, "name": name.String, "group": isGroup.Int64 == 1,
			"pinned": pinned.Int64 == 1, "ts": ts.Int64, "preview": preview.String,
			"muted": muted.Int64 == 1, "archived": archived.Int64 == 1,
			"unread": unread.Int64,
		})
	}
	return out
}

// searchMessages finds messages whose text matches a query, newest first.
//
// A plain LIKE over the messages table: with tens of thousands of rows this is
// a scan, but it's a scan of one local sqlite file on a query the user typed
// deliberately, so it stays well inside "instant". chat scopes it to one
// conversation; empty searches everywhere.
func (h *histStore) searchMessages(query, chat string, limit int) []ws.MsgData {
	out := []ws.MsgData{}
	if h == nil || h.db == nil || strings.TrimSpace(query) == "" {
		return out
	}
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	// Escape LIKE's own wildcards so searching for "100%" doesn't match
	// everything.
	esc := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(query)
	pattern := "%" + esc + "%"

	q := `SELECT msgid,chat,sender,sendername,fromme,ts,kind,text
	      FROM messages
	      WHERE text LIKE ? ESCAPE '\' AND text<>''`
	args := []any{pattern}
	if chat != "" {
		q += ` AND chat=?`
		args = append(args, chat)
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)

	rows, err := h.db.Query(q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var d ws.MsgData
		var sender, sname, kind, text sql.NullString
		var fromme, ts sql.NullInt64
		if rows.Scan(&d.MsgID, &d.ChatJID, &sender, &sname, &fromme, &ts, &kind, &text) != nil {
			continue
		}
		d.SenderJID, d.SenderName = sender.String, sname.String
		d.Kind, d.Text = kind.String, text.String
		d.Timestamp, d.FromMe = ts.Int64, fromme.Int64 == 1
		out = append(out, d)
	}
	return out
}

// chatNamesFor maps chat JIDs to their display names, so search results can be
// labelled with the conversation they came from.
func (h *histStore) chatNamesFor(jids []string) map[string]string {
	out := map[string]string{}
	if h == nil || h.db == nil || len(jids) == 0 {
		return out
	}
	args := make([]any, len(jids))
	for i, j := range jids {
		args[i] = j
	}
	rows, err := h.db.Query(`SELECT jid, COALESCE(name,'') FROM chats
		WHERE jid IN (?`+repeatPlaceholders(len(jids)-1)+`)`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var jid, name string
		if rows.Scan(&jid, &name) == nil {
			out[jid] = name
		}
	}
	return out
}

// unreadCount is the number of incoming messages in a chat not yet marked read.
func (h *histStore) unreadCount(chat string) int {
	if h == nil || h.db == nil {
		return 0
	}
	var n int
	h.db.QueryRow(`SELECT COUNT(*) FROM messages
		WHERE chat=? AND fromme=0 AND COALESCE(status,'')<>'read'`, chat).Scan(&n)
	return n
}

// markAllRead flags every incoming message in a chat as read.
//
// Separate from the receipts we send: WhatsApp's read receipt means "everything
// up to here", so acknowledging the most recent handful covers the chat — but
// locally every message has to be flagged or the unread count won't reach zero.
func (h *histStore) markAllRead(chat string) int {
	if h == nil || h.db == nil {
		return 0
	}
	res, _ := h.db.Exec(`UPDATE messages SET status='read'
		WHERE chat=? AND fromme=0 AND COALESCE(status,'')<>'read'`, chat)
	if res == nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
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

// editMessageText rewrites a stored message's body. Returns false when we never
// had the original — an edit for a message outside our history is nothing to
// apply, and inventing a row for it would put a bodiless message in the thread.
func (h *histStore) editMessageText(chat, msgid, body string) bool {
	if h == nil || h.db == nil || msgid == "" {
		return false
	}
	res, err := h.db.Exec(`UPDATE messages SET text=? WHERE chat=? AND msgid=?`,
		body, chat, msgid)
	if err != nil || res == nil {
		return false
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false
	}
	// The chat-list preview quotes the message, so it goes stale otherwise.
	h.db.Exec(`UPDATE chats SET preview=? WHERE jid=? AND last_ts=
		(SELECT MAX(ts) FROM messages WHERE chat=?)`, body, chat, chat)
	return true
}

// deleteMessage removes one message from our own store only. Used for "delete
// for me", which WhatsApp has no synced equivalent of.
func (h *histStore) deleteMessage(chat, msgid string) {
	if h == nil || h.db == nil {
		return
	}
	h.db.Exec(`DELETE FROM messages WHERE chat=? AND msgid=?`, chat, msgid)
	h.db.Exec(`DELETE FROM reactions WHERE chat=? AND msgid=?`, chat, msgid)
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

// putThumb stores the small JPEG preview WhatsApp ships inside a message.
//
// The table is named location_thumbs for historical reasons — locations were
// the first thing to need it — but every photo, video and sticker carries one
// too, and serving those instead of the full-size file is the difference
// between a chat thread costing tens of megabytes of decoded bitmap and
// costing a few hundred kilobytes. Renaming the table would mean a migration
// for no behavioural gain, so the name stays and this comment carries the
// explanation.
func (h *histStore) putThumb(msgid string, jpeg []byte) { h.putLocationThumb(msgid, jpeg) }

// thumb returns a stored preview for any message kind, or nil.
func (h *histStore) thumb(msgid string) []byte { return h.locationThumb(msgid) }

// putLocationThumb stores the map preview WhatsApp ships with a location.
func (h *histStore) putLocationThumb(msgid string, jpeg []byte) {
	if h == nil || h.db == nil || msgid == "" || len(jpeg) == 0 {
		return
	}
	h.db.Exec(`INSERT INTO location_thumbs (msgid, jpeg) VALUES (?,?)
		ON CONFLICT(msgid) DO UPDATE SET jpeg=excluded.jpeg`, msgid, jpeg)
}

// locationThumb returns a stored map preview, or nil.
func (h *histStore) locationThumb(msgid string) []byte {
	if h == nil || h.db == nil || msgid == "" {
		return nil
	}
	var jpeg []byte
	if h.db.QueryRow(`SELECT jpeg FROM location_thumbs WHERE msgid=?`, msgid).Scan(&jpeg) != nil {
		return nil
	}
	return jpeg
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

// statusRank orders delivery states so a receipt can never move a message
// backwards. Receipts arrive out of order — a "delivered" for one device can
// land after another device already reported "read".
var statusRank = map[string]int{"": 0, "sent": 1, "delivered": 2, "read": 3, "played": 4}

// setMessageStatus advances a message's delivery state, ignoring anything that
// would downgrade it. Returns true if the row actually changed.
func (h *histStore) setMessageStatus(msgid, status string) bool {
	if h == nil || h.db == nil || msgid == "" || status == "" {
		return false
	}
	var current sql.NullString
	if h.db.QueryRow(`SELECT COALESCE(status,'') FROM messages WHERE msgid=? LIMIT 1`,
		msgid).Scan(&current) != nil {
		return false
	}
	if statusRank[status] <= statusRank[current.String] {
		return false
	}
	res, _ := h.db.Exec(`UPDATE messages SET status=? WHERE msgid=?`, status, msgid)
	if res == nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// putReaction records (or, with an empty emoji, clears) one person's reaction.
func (h *histStore) putReaction(chat, msgid, sender, name, emoji string, ts int64) {
	if h == nil || h.db == nil || chat == "" || msgid == "" || sender == "" {
		return
	}
	if emoji == "" {
		h.db.Exec(`DELETE FROM reactions WHERE chat=? AND msgid=? AND sender=?`, chat, msgid, sender)
		return
	}
	h.db.Exec(`INSERT INTO reactions (chat,msgid,sender,name,emoji,ts) VALUES (?,?,?,?,?,?)
		ON CONFLICT(chat,msgid,sender) DO UPDATE SET
			emoji=excluded.emoji, name=excluded.name, ts=excluded.ts`,
		chat, msgid, sender, name, emoji, ts)
}

// reactionsForChat returns every reaction in a chat, keyed by target message, so
// a history page can be decorated in one query instead of one per message.
func (h *histStore) reactionsForChat(chat string) map[string][]ws.ReactionData {
	out := map[string][]ws.ReactionData{}
	if h == nil || h.db == nil {
		return out
	}
	rows, err := h.db.Query(`SELECT msgid, sender, COALESCE(name,''), emoji, COALESCE(ts,0)
		FROM reactions WHERE chat=? ORDER BY ts ASC`, chat)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var msgid string
		var r ws.ReactionData
		if rows.Scan(&msgid, &r.SenderJID, &r.SenderName, &r.Emoji, &r.Timestamp) != nil {
			continue
		}
		out[msgid] = append(out[msgid], r)
	}
	return out
}

// reactionsForMessage returns the reactions on a single message.
func (h *histStore) reactionsForMessage(chat, msgid string) []ws.ReactionData {
	var out []ws.ReactionData
	if h == nil || h.db == nil {
		return out
	}
	rows, err := h.db.Query(`SELECT sender, COALESCE(name,''), emoji, COALESCE(ts,0)
		FROM reactions WHERE chat=? AND msgid=? ORDER BY ts ASC`, chat, msgid)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var r ws.ReactionData
		if rows.Scan(&r.SenderJID, &r.SenderName, &r.Emoji, &r.Timestamp) != nil {
			continue
		}
		out = append(out, r)
	}
	return out
}

// unreadMessageIDs lists incoming messages in a chat that we haven't marked read
// yet, so the app can tell WhatsApp about them in one go.
func (h *histStore) unreadMessageIDs(chat string, limit int) ([]string, string) {
	if h == nil || h.db == nil {
		return nil, ""
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := h.db.Query(`SELECT msgid, sender FROM messages
		WHERE chat=? AND fromme=0 AND COALESCE(status,'')<>'read'
		ORDER BY ts DESC LIMIT ?`, chat, limit)
	if err != nil {
		return nil, ""
	}
	defer rows.Close()
	var ids []string
	var lastSender string
	for rows.Next() {
		var id string
		var sender sql.NullString
		if rows.Scan(&id, &sender) != nil {
			continue
		}
		ids = append(ids, id)
		if lastSender == "" {
			lastSender = sender.String
		}
	}
	return ids, lastSender
}

// markMessagesRead flags incoming messages as read locally, after WhatsApp has
// been told.
func (h *histStore) markMessagesRead(chat string, ids []string) {
	if h == nil || h.db == nil || len(ids) == 0 {
		return
	}
	q := `UPDATE messages SET status='read' WHERE chat=? AND msgid IN (?` +
		repeatPlaceholders(len(ids)-1) + `)`
	args := make([]any, 0, len(ids)+1)
	args = append(args, chat)
	for _, id := range ids {
		args = append(args, id)
	}
	h.db.Exec(q, args...)
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
	q := `SELECT msgid,chat,sender,sendername,fromme,ts,kind,text,media,mime,quoted_id,quoted_text,quoted_name,COALESCE(forwarded,0),COALESCE(status,''),COALESCE(lat,0),COALESCE(lon,0),COALESCE(loc_name,''),COALESCE(loc_address,'')
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
		var sender, sname, text, media, mime, qid, qtext, qname, kind, status sql.NullString
		var locName, locAddr sql.NullString
		var lat, lon sql.NullFloat64
		if err := rows.Scan(&d.MsgID, &d.ChatJID, &sender, &sname, &fromme,
			&ts, &kind, &text, &media, &mime,
			&qid, &qtext, &qname, &fwd, &status,
			&lat, &lon, &locName, &locAddr); err != nil {
			continue
		}
		d.Lat, d.Lon = lat.Float64, lon.Float64
		d.LocName, d.LocAddress = locName.String, locAddr.String
		d.Forwarded = fwd.Int64 == 1
		d.Status = status.String
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
	// Decorate with reactions in one query rather than one per message.
	byMsg := h.reactionsForChat(chat)
	// Same for thumbnails: one query for the page, not one per photo. Messages
	// stored before thumbnails were kept have no row, and those fall back to
	// the full-size file exactly as they did before.
	haveThumb := h.thumbsFor(tmp)
	for i := len(tmp) - 1; i >= 0; i-- {
		m := tmp[i]
		if rs, ok := byMsg[m.MsgID]; ok {
			m.Reactions = rs
		}
		if m.ThumbURL == "" && haveThumb[m.MsgID] {
			m.ThumbURL = "/thumb/" + m.MsgID
		}
		out = append(out, m)
	}
	return out
}

// thumbsFor reports which of these messages have a stored preview, in one
// query. Asking per message would be a round trip per photo on every page of
// history, which on a 40-message page is 40 queries to answer a question one
// can answer.
func (h *histStore) thumbsFor(msgs []ws.MsgData) map[string]bool {
	found := map[string]bool{}
	if h.db == nil || len(msgs) == 0 {
		return found
	}
	ids := make([]any, 0, len(msgs))
	holes := make([]byte, 0, len(msgs)*2)
	for _, m := range msgs {
		switch m.Kind {
		case "image", "video", "gif", "sticker":
			ids = append(ids, m.MsgID)
			if len(holes) > 0 {
				holes = append(holes, ',')
			}
			holes = append(holes, '?')
		}
	}
	if len(ids) == 0 {
		return found
	}
	rows, err := h.db.Query(
		`SELECT msgid FROM location_thumbs WHERE msgid IN (`+string(holes)+`)`, ids...)
	if err != nil {
		return found
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			found[id] = true
		}
	}
	return found
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

// recentStickers lists the distinct stickers this account has seen, newest
// first, for the sticker picker.
//
// Our own history rather than the account's favourites, and the difference is
// worth being honest about: WhatsApp keeps favourite and recent stickers in app
// state, and whatsmeow exposes no way to read that list — FetchStickerPack
// needs a pack id nothing here hands us. What we do have is every sticker that
// has passed through this daemon, which for anyone who reuses the stickers they
// are sent is much the same set.
//
// Deduplicated on the media reference rather than the message id: the same
// sticker sent five times is one entry in a picker; five is a picker nobody can
// use.
func (h *histStore) recentStickers(limit int) []stickerEntry {
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	rows, err := h.db.Query(`
		SELECT msgid, MAX(ts) AS ts
		  FROM messages
		 WHERE kind = 'sticker' AND media IS NOT NULL AND media != ''
		 GROUP BY media
		 ORDER BY ts DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []stickerEntry
	for rows.Next() {
		var e stickerEntry
		if err := rows.Scan(&e.MsgID, &e.Timestamp); err != nil {
			continue
		}
		e.MediaURL = "/media/" + e.MsgID
		out = append(out, e)
	}
	return out
}

// putStickerGIF caches an animated sticker's converted form.
func (h *histStore) putStickerGIF(msgid string, gif []byte) {
	if h == nil || h.db == nil || msgid == "" || len(gif) == 0 {
		return
	}
	h.db.Exec(`INSERT INTO sticker_gifs (msgid, gif, ts) VALUES (?,?,?)
		ON CONFLICT(msgid) DO UPDATE SET gif=excluded.gif, ts=excluded.ts`,
		msgid, gif, time.Now().Unix())
}

// stickerGIF returns a cached conversion, or nil.
func (h *histStore) stickerGIF(msgid string) []byte {
	if h == nil || h.db == nil || msgid == "" {
		return nil
	}
	var gif []byte
	err := h.db.QueryRow(`SELECT gif FROM sticker_gifs WHERE msgid = ?`, msgid).Scan(&gif)
	if err != nil {
		return nil
	}
	return gif
}
