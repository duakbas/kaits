package wa

import (
	"database/sql"
	"path/filepath"
	"testing"

	"wad/internal/ws"

	_ "github.com/mattn/go-sqlite3"
)

func newHist(t *testing.T) *histStore {
	t.Helper()
	h, err := openHistStore(filepath.Join(t.TempDir(), "hist.db"))
	if err != nil {
		t.Fatalf("openHistStore: %v", err)
	}
	t.Cleanup(h.close)
	return h
}

func putMsg(h *histStore, chat, id, sender, sname, text string, ts int64, fromMe bool) {
	h.putMessage(ws.MsgData{
		MsgID: id, ChatJID: chat, SenderJID: sender, SenderName: sname,
		FromMe: fromMe, Timestamp: ts, Kind: "text", Text: text,
		ChatName: sname,
	})
}

func TestLocalContactRoundTripAndClear(t *testing.T) {
	h := newHist(t)
	jid := "41791234567@s.whatsapp.net"

	if got := h.localContactName(jid); got != "" {
		t.Errorf("unsaved contact = %q, want empty", got)
	}
	h.setLocalContact(jid, "Peder", 1)
	if got := h.localContactName(jid); got != "Peder" {
		t.Errorf("after save = %q, want Peder", got)
	}
	// Saving again must update rather than fail on the primary key.
	h.setLocalContact(jid, "Peder A", 2)
	if got := h.localContactName(jid); got != "Peder A" {
		t.Errorf("after rename = %q, want 'Peder A'", got)
	}
	// An empty name clears the nickname.
	h.setLocalContact(jid, "", 3)
	if got := h.localContactName(jid); got != "" {
		t.Errorf("after clear = %q, want empty", got)
	}
}

func TestChatFlagsPersistAndAreIsolated(t *testing.T) {
	h := newHist(t)
	putMsg(h, "a@s.whatsapp.net", "m1", "a@s.whatsapp.net", "A", "hi", 100, false)
	putMsg(h, "b@s.whatsapp.net", "m2", "b@s.whatsapp.net", "B", "yo", 101, false)

	h.setChatFlag("a@s.whatsapp.net", "muted", true)
	h.setChatFlag("a@s.whatsapp.net", "archived", true)

	p, m, a := h.chatFlags("a@s.whatsapp.net")
	if p || !m || !a {
		t.Errorf("chat a flags = pinned:%v muted:%v archived:%v, want false/true/true", p, m, a)
	}
	// The other chat must be untouched.
	if p2, m2, a2 := h.chatFlags("b@s.whatsapp.net"); p2 || m2 || a2 {
		t.Errorf("chat b flags = %v/%v/%v, want all false", p2, m2, a2)
	}

	h.setChatFlag("a@s.whatsapp.net", "muted", false)
	if _, m3, _ := h.chatFlags("a@s.whatsapp.net"); m3 {
		t.Error("unmute did not stick")
	}
}

// An unknown column name must be rejected rather than interpolated into SQL.
func TestSetChatFlagRejectsUnknownColumn(t *testing.T) {
	h := newHist(t)
	putMsg(h, "a@s.whatsapp.net", "m1", "a@s.whatsapp.net", "A", "hi", 100, false)
	h.setChatFlag("a@s.whatsapp.net", "name=1 WHERE 1=1--", true)
	// The chat row must survive intact.
	if got := h.listChats(); len(got) != 1 || got[0]["name"] != "A" {
		t.Errorf("chat row damaged by bad column: %+v", got)
	}
}

func TestListChatsReportsMuteAndArchive(t *testing.T) {
	h := newHist(t)
	putMsg(h, "a@s.whatsapp.net", "m1", "a@s.whatsapp.net", "A", "hi", 100, false)
	h.setChatFlag("a@s.whatsapp.net", "muted", true)

	rows := h.listChats()
	if len(rows) != 1 {
		t.Fatalf("listChats returned %d rows, want 1", len(rows))
	}
	if rows[0]["muted"] != true || rows[0]["archived"] != false {
		t.Errorf("muted=%v archived=%v, want true/false", rows[0]["muted"], rows[0]["archived"])
	}
}

func TestLastMessagePicksNewest(t *testing.T) {
	h := newHist(t)
	chat := "g@g.us"
	putMsg(h, chat, "old", "x@s.whatsapp.net", "X", "first", 100, false)
	putMsg(h, chat, "new", "y@s.whatsapp.net", "Y", "latest", 200, false)

	id, ts, fromMe, sender := h.lastMessage(chat)
	if id != "new" || ts != 200 || fromMe || sender != "y@s.whatsapp.net" {
		t.Errorf("lastMessage = %q/%d/%v/%q, want new/200/false/y@...", id, ts, fromMe, sender)
	}
	if id, _, _, _ := h.lastMessage("nosuch@s.whatsapp.net"); id != "" {
		t.Errorf("lastMessage(unknown) = %q, want empty", id)
	}
}

func TestDropChatRemovesChatAndMessages(t *testing.T) {
	h := newHist(t)
	putMsg(h, "a@s.whatsapp.net", "m1", "a@s.whatsapp.net", "A", "hi", 100, false)
	putMsg(h, "b@s.whatsapp.net", "m2", "b@s.whatsapp.net", "B", "yo", 101, false)

	h.dropChat("a@s.whatsapp.net")

	if rows := h.listChats(); len(rows) != 1 || rows[0]["jid"] != "b@s.whatsapp.net" {
		t.Errorf("after drop, chats = %+v, want only b", rows)
	}
	if msgs := h.history("a@s.whatsapp.net", 0, 10); len(msgs) != 0 {
		t.Errorf("dropped chat still has %d messages", len(msgs))
	}
	if msgs := h.history("b@s.whatsapp.net", 0, 10); len(msgs) != 1 {
		t.Errorf("other chat lost messages: %d", len(msgs))
	}
}

func TestBackfillQuotedNamesOnlyRewritesNumbers(t *testing.T) {
	h := newHist(t)
	chat := "g@g.us"
	h.putMessage(ws.MsgData{
		MsgID: "m1", ChatJID: chat, Timestamp: 100, Kind: "text", Text: "a",
		QuotedID: "q1", QuotedText: "orig", QuotedName: "95954434789442",
	})
	h.putMessage(ws.MsgData{
		MsgID: "m2", ChatJID: chat, Timestamp: 101, Kind: "text", Text: "b",
		QuotedID: "q2", QuotedText: "orig", QuotedName: "Alex Rivera",
	})

	n := h.BackfillQuotedNames(func(num string) string {
		if num == "95954434789442" {
			return "Deniz"
		}
		return ""
	})
	if n != 1 {
		t.Errorf("backfilled %d rows, want 1", n)
	}

	msgs := h.history(chat, 0, 10)
	got := map[string]string{}
	for _, m := range msgs {
		got[m.MsgID] = m.QuotedName
	}
	if got["m1"] != "Deniz" {
		t.Errorf("m1 quoted name = %q, want Deniz", got["m1"])
	}
	if got["m2"] != "Alex Rivera" {
		t.Errorf("m2 quoted name changed to %q; real names must be left alone", got["m2"])
	}
}

// A resolver that returns another bare number must not be written back —
// that would just swap one unreadable id for another.
func TestBackfillQuotedNamesIgnoresNumericResolution(t *testing.T) {
	h := newHist(t)
	h.putMessage(ws.MsgData{
		MsgID: "m1", ChatJID: "g@g.us", Timestamp: 100, Kind: "text",
		QuotedID: "q1", QuotedText: "o", QuotedName: "12345",
	})
	if n := h.BackfillQuotedNames(func(string) string { return "67890" }); n != 0 {
		t.Errorf("backfilled %d rows, want 0", n)
	}
}

func TestBackfillChatNamesSkipsRealNames(t *testing.T) {
	h := newHist(t)
	putMsg(h, "num@s.whatsapp.net", "m1", "num@s.whatsapp.net", "41791234567", "hi", 100, false)
	putMsg(h, "named@s.whatsapp.net", "m2", "named@s.whatsapp.net", "Alex", "yo", 101, false)

	n := h.BackfillChatNames(func(jid string) string {
		if jid == "num@s.whatsapp.net" {
			return "Peder"
		}
		return "SHOULD NOT BE USED"
	})
	if n != 1 {
		t.Errorf("renamed %d chats, want 1", n)
	}
	names := map[string]any{}
	for _, c := range h.listChats() {
		names[c["jid"].(string)] = c["name"]
	}
	if names["num@s.whatsapp.net"] != "Peder" {
		t.Errorf("numeric chat name = %v, want Peder", names["num@s.whatsapp.net"])
	}
	if names["named@s.whatsapp.net"] != "Alex" {
		t.Errorf("already-named chat was overwritten: %v", names["named@s.whatsapp.net"])
	}
}

func TestRenameChatAndMessagesAppliesSavedName(t *testing.T) {
	h := newHist(t)
	chat := "41791234567@s.whatsapp.net"
	putMsg(h, chat, "m1", chat, "41791234567", "hi", 100, false)

	h.renameChatAndMessages(chat, "Peder")

	if got := h.listChats()[0]["name"]; got != "Peder" {
		t.Errorf("chat name = %v, want Peder", got)
	}
	msgs := h.history(chat, 0, 10)
	if len(msgs) != 1 || msgs[0].SenderName != "Peder" {
		t.Errorf("sender name = %q, want Peder", msgs[0].SenderName)
	}
}

// Existing databases predate the muted/archived columns. Opening one must add
// them in place rather than failing or losing the stored chats.
func TestOpenHistStoreUpgradesOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// Build a chats table shaped like the pre-upgrade release.
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE chats (
		jid TEXT PRIMARY KEY, name TEXT, is_group INTEGER,
		pinned INTEGER, last_ts INTEGER, preview TEXT);
		INSERT INTO chats VALUES ('a@s.whatsapp.net','Alex',0,1,100,'hi');`)
	if err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	db.Close()

	h, err := openHistStore(path)
	if err != nil {
		t.Fatalf("openHistStore on old db: %v", err)
	}
	defer h.close()

	rows := h.listChats()
	if len(rows) != 1 || rows[0]["name"] != "Alex" {
		t.Fatalf("existing chat lost on upgrade: %+v", rows)
	}
	if rows[0]["muted"] != false || rows[0]["archived"] != false {
		t.Errorf("new columns should default false, got %v/%v", rows[0]["muted"], rows[0]["archived"])
	}
	// And the new columns must actually be writable.
	h.setChatFlag("a@s.whatsapp.net", "archived", true)
	if _, _, a := h.chatFlags("a@s.whatsapp.net"); !a {
		t.Error("archived flag did not persist after in-place upgrade")
	}
}
