package wa

import (
	"database/sql"
	"path/filepath"
	"strings"
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

// The case the numeric backfills miss entirely: rows hold a real, plausible
// name — the one the contact chose — when the user has them saved as something
// else. Nothing looks broken, so only RefreshNames corrects it.
func TestRefreshNamesFixesWrongButPlausibleNames(t *testing.T) {
	h := newHist(t)
	chat := "41783345556@s.whatsapp.net"
	group := "g@g.us"

	putMsg(h, chat, "m1", chat, "Sarp Doruk Gerenli", "dm", 100, false)
	putMsg(h, group, "m2", chat, "Sarp Doruk Gerenli", "in group", 101, false)
	// A quoted reply stores the author's name, not their JID.
	h.putMessage(ws.MsgData{
		MsgID: "m3", ChatJID: group, SenderJID: "other@s.whatsapp.net",
		SenderName: "Someone", Timestamp: 102, Kind: "text", Text: "re",
		QuotedID: "m2", QuotedText: "in group", QuotedName: "Sarp Doruk Gerenli",
	})

	chats, msgs, quotes := h.RefreshNames(func(jid string) string {
		if jid == chat {
			return "bulgayrian"
		}
		return ""
	})

	if chats != 1 {
		t.Errorf("chat titles fixed = %d, want 1", chats)
	}
	if msgs != 2 {
		t.Errorf("sender names fixed = %d, want 2", msgs)
	}
	if quotes != 1 {
		t.Errorf("quoted names fixed = %d, want 1", quotes)
	}

	if got := h.listChats(); got[0]["name"] != "bulgayrian" && got[1]["name"] != "bulgayrian" {
		t.Errorf("chat title not updated: %+v", got)
	}
	for _, m := range h.history(group, 0, 10) {
		if m.MsgID == "m2" && m.SenderName != "bulgayrian" {
			t.Errorf("group sender = %q, want bulgayrian", m.SenderName)
		}
		if m.MsgID == "m3" && m.QuotedName != "bulgayrian" {
			t.Errorf("quoted author = %q, want bulgayrian", m.QuotedName)
		}
	}
}

// RefreshNames must never touch rows it has no authoritative name for —
// returning "" has to mean "leave it alone", not "blank it".
func TestRefreshNamesLeavesUnknownContactsAlone(t *testing.T) {
	h := newHist(t)
	chat := "999@s.whatsapp.net"
	putMsg(h, chat, "m1", chat, "Their Own Name", "hi", 100, false)

	chats, msgs, quotes := h.RefreshNames(func(string) string { return "" })
	if chats != 0 || msgs != 0 || quotes != 0 {
		t.Errorf("changed %d/%d/%d rows with no resolution, want 0/0/0", chats, msgs, quotes)
	}
	if got := h.history(chat, 0, 10)[0].SenderName; got != "Their Own Name" {
		t.Errorf("sender name = %q, want it untouched", got)
	}
}

// Messages the user sent are labelled "You" and must not be rewritten.
func TestRefreshNamesSkipsOwnMessages(t *testing.T) {
	h := newHist(t)
	chat := "41783345556@s.whatsapp.net"
	putMsg(h, chat, "m1", "me@s.whatsapp.net", "You", "mine", 100, true)

	h.RefreshNames(func(string) string { return "bulgayrian" })

	if got := h.history(chat, 0, 10)[0].SenderName; got != "You" {
		t.Errorf("own message sender = %q, want You", got)
	}
}

// Reply-privately, DM-the-sender and view-profile all resolve a person from a
// message id. They used to read only an in-memory cache of live messages, so
// they failed on anything scrolled to out of stored history or on anything at
// all after a restart. The message table has to be able to answer instead.
func TestMessageByIDFindsStoredMessages(t *testing.T) {
	h := newHist(t)
	group := "120363422154151519@g.us"
	sender := "41783345556@s.whatsapp.net"
	putMsg(h, group, "4A26B05FDFEFC345EA2B", sender, "bulgayrian", "ayagin top tutar mi", 100, false)

	chat, snd, kind, text, fromMe, ok := h.messageByID("4A26B05FDFEFC345EA2B")
	if !ok {
		t.Fatal("stored message not found by id")
	}
	if chat != group {
		t.Errorf("chat = %q, want %q", chat, group)
	}
	if snd != sender {
		t.Errorf("sender = %q, want %q", snd, sender)
	}
	if kind != "text" || text != "ayagin top tutar mi" {
		t.Errorf("kind/text = %q/%q", kind, text)
	}
	if fromMe {
		t.Error("fromMe should be false")
	}
}

func TestMessageByIDMissesAreReported(t *testing.T) {
	h := newHist(t)
	if _, _, _, _, _, ok := h.messageByID("NOPE"); ok {
		t.Error("unknown message id reported as found")
	}
	if _, _, _, _, _, ok := h.messageByID(""); ok {
		t.Error("empty message id reported as found")
	}
}

// Mentions are resolved once at receive time and the result is stored, so any
// that failed then are frozen as raw ids in the message body.
func TestRewriteMessageTextResolvesStoredMentions(t *testing.T) {
	h := newHist(t)
	chat := "g@g.us"
	putMsg(h, chat, "m1", "x@s.whatsapp.net", "X",
		"So @230399326330975 @117432995844197 we're not playing right?", 100, false)
	putMsg(h, chat, "m2", "x@s.whatsapp.net", "X", "no mentions here", 101, false)

	n := h.rewriteMessageText(mentionPattern, func(digits string) string {
		switch digits {
		case "230399326330975":
			return "@Deniz"
		case "117432995844197":
			return "@~Sarp"
		}
		return ""
	})
	if n != 1 {
		t.Errorf("rewrote %d messages, want 1", n)
	}

	got := map[string]string{}
	for _, m := range h.history(chat, 0, 10) {
		got[m.MsgID] = m.Text
	}
	want := "So @Deniz @~Sarp we're not playing right?"
	if got["m1"] != want {
		t.Errorf("text = %q, want %q", got["m1"], want)
	}
	if got["m2"] != "no mentions here" {
		t.Errorf("unrelated message changed to %q", got["m2"])
	}
}

// An unresolvable mention must be left exactly as it was, not blanked.
func TestRewriteMessageTextLeavesUnknownMentions(t *testing.T) {
	h := newHist(t)
	chat := "g@g.us"
	original := "hey @999888777666555 you there"
	putMsg(h, chat, "m1", "x@s.whatsapp.net", "X", original, 100, false)

	if n := h.rewriteMessageText(mentionPattern, func(string) string { return "" }); n != 0 {
		t.Errorf("rewrote %d messages, want 0", n)
	}
	if got := h.history(chat, 0, 10)[0].Text; got != original {
		t.Errorf("text = %q, want it untouched", got)
	}
}

// Short digit runs in ordinary prose (years, prices) must not be treated as
// mentions.
func TestMentionPatternIgnoresShortNumbers(t *testing.T) {
	for _, s := range []string{"costs @50", "in @2026", "@12345"} {
		if mentionPattern.MatchString(s) {
			t.Errorf("%q should not match the mention pattern", s)
		}
	}
	if !mentionPattern.MatchString("@123456") {
		t.Error("six digits should match")
	}
}

// Chat previews embed the sender's name, so a name repair on the messages table
// leaves the chat list still showing the old (often numeric) name.
func TestRebuildPreviewsUsesCurrentSenderNames(t *testing.T) {
	h := newHist(t)
	group := "g@g.us"
	dm := "dm@s.whatsapp.net"

	// Stored while the sender was still an unresolved number.
	putMsg(h, group, "m1", "x@s.whatsapp.net", "101194261385405", "hi", 100, false)
	putMsg(h, dm, "m2", dm, "Alex", "yo", 101, false)
	// The name pass has since fixed the message row.
	h.db.Exec(`UPDATE messages SET sendername='~Ahmet' WHERE msgid='m1'`)
	h.db.Exec(`UPDATE chats SET is_group=1 WHERE jid=?`, group)

	if n := h.rebuildPreviews(); n < 1 {
		t.Fatalf("rebuilt %d previews, want at least 1", n)
	}

	prev := map[string]any{}
	for _, c := range h.listChats() {
		prev[c["jid"].(string)] = c["preview"]
	}
	if prev[group] != "~Ahmet: hi" {
		t.Errorf("group preview = %v, want %q", prev[group], "~Ahmet: hi")
	}
	// A 1:1 preview carries no sender prefix.
	if prev[dm] != "yo" {
		t.Errorf("dm preview = %v, want %q", prev[dm], "yo")
	}
}

func TestRebuildPreviewsLabelsMediaByKind(t *testing.T) {
	h := newHist(t)
	group := "g@g.us"
	h.putMessage(ws.MsgData{
		MsgID: "m1", ChatJID: group, SenderJID: "x@s.whatsapp.net",
		SenderName: "~Ahmet", IsGroup: true, Timestamp: 100, Kind: "sticker",
		ChatName: "Group",
	})
	h.db.Exec(`UPDATE chats SET is_group=1 WHERE jid=?`, group)

	h.rebuildPreviews()
	if got := h.listChats()[0]["preview"]; got != "~Ahmet: [sticker]" {
		t.Errorf("preview = %v, want %q", got, "~Ahmet: [sticker]")
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

// Unsaved people's stored names must gain the "~" marker. RefreshNames can't do
// this — it only writes authoritative names, and an unsaved contact has none.
func TestMarkUnsavedNamesAddsTilde(t *testing.T) {
	h := newHist(t)
	unsaved := "111@s.whatsapp.net"
	saved := "222@s.whatsapp.net"
	group := "g@g.us"

	putMsg(h, group, "m1", unsaved, "David Cannone", "hi", 100, false)
	putMsg(h, group, "m2", saved, "bulgayrian", "yo", 101, false)
	putMsg(h, unsaved, "m3", unsaved, "David Cannone", "dm", 102, false)
	h.putMessage(ws.MsgData{
		MsgID: "m4", ChatJID: group, SenderJID: saved, SenderName: "bulgayrian",
		Timestamp: 103, Kind: "text", Text: "re", QuotedID: "m1",
		QuotedText: "hi", QuotedName: "David Cannone",
	})

	chats, msgs, quotes := h.markUnsavedNames(func(jid string) bool {
		return jid == saved
	})

	if msgs != 2 {
		t.Errorf("sender names marked = %d, want 2", msgs)
	}
	if quotes != 1 {
		t.Errorf("quoted names marked = %d, want 1", quotes)
	}
	if chats != 1 {
		t.Errorf("chat titles marked = %d, want 1 (the unsaved DM)", chats)
	}

	for _, m := range h.history(group, 0, 10) {
		switch m.MsgID {
		case "m1":
			if m.SenderName != "~David Cannone" {
				t.Errorf("unsaved sender = %q, want ~David Cannone", m.SenderName)
			}
		case "m2":
			if m.SenderName != "bulgayrian" {
				t.Errorf("saved sender = %q, must stay unmarked", m.SenderName)
			}
		case "m4":
			if m.QuotedName != "~David Cannone" {
				t.Errorf("quoted author = %q, want ~David Cannone", m.QuotedName)
			}
		}
	}
}

// Running twice must not produce "~~Name".
func TestMarkUnsavedNamesIsIdempotent(t *testing.T) {
	h := newHist(t)
	jid := "111@s.whatsapp.net"
	putMsg(h, "g@g.us", "m1", jid, "David Cannone", "hi", 100, false)

	never := func(string) bool { return false }
	h.markUnsavedNames(never)
	_, msgs, _ := h.markUnsavedNames(never)
	if msgs != 0 {
		t.Errorf("second pass changed %d rows, want 0", msgs)
	}
	if got := h.history("g@g.us", 0, 10)[0].SenderName; got != "~David Cannone" {
		t.Errorf("name = %q, want a single tilde", got)
	}
}

// Bare numbers are not names — the marker must not be stuck on them.
func TestMarkUnsavedNamesSkipsBareNumbers(t *testing.T) {
	h := newHist(t)
	jid := "111@s.whatsapp.net"
	putMsg(h, "g@g.us", "m1", jid, "104570072096833", "hi", 100, false)

	if _, msgs, _ := h.markUnsavedNames(func(string) bool { return false }); msgs != 0 {
		t.Errorf("marked %d numeric names, want 0", msgs)
	}
	if got := h.history("g@g.us", 0, 10)[0].SenderName; got != "104570072096833" {
		t.Errorf("name = %q, want it untouched", got)
	}
}

// Media used to break on every restart because the only record of an attachment
// was an in-memory cache. The CDN keys must persist and round-trip.
func TestMediaRefRoundTrip(t *testing.T) {
	h := newHist(t)
	ref := mediaRef{
		directPath: "/v/t62.7118-24/foo.enc",
		encSHA256:  []byte{1, 2, 3},
		sha256:     []byte{4, 5, 6},
		mediaKey:   []byte{7, 8, 9},
		mediaType:  "WhatsApp Image Keys",
		mime:       "image/jpeg",
	}
	h.putMediaRef("MSG1", ref)

	got, ok := h.mediaRefFor("MSG1")
	if !ok {
		t.Fatal("stored media ref not found")
	}
	if got.directPath != ref.directPath || got.mediaType != ref.mediaType || got.mime != ref.mime {
		t.Errorf("scalars round-tripped wrong: %+v", got)
	}
	for _, p := range [][2][]byte{
		{got.encSHA256, ref.encSHA256}, {got.sha256, ref.sha256}, {got.mediaKey, ref.mediaKey},
	} {
		if string(p[0]) != string(p[1]) {
			t.Errorf("blob round-tripped wrong: %v vs %v", p[0], p[1])
		}
	}

	if _, ok := h.mediaRefFor("NOPE"); ok {
		t.Error("unknown msgid reported as found")
	}
}

// A ref with no direct path can't be downloaded from, so it must not be stored
// and must not be reported as usable.
func TestMediaRefRejectsEmptyDirectPath(t *testing.T) {
	h := newHist(t)
	h.putMediaRef("MSG1", mediaRef{mediaType: "WhatsApp Image Keys"})
	if _, ok := h.mediaRefFor("MSG1"); ok {
		t.Error("ref with no direct path should not be usable")
	}
}

// Re-receiving the same message (history sync overlapping live traffic) must
// update the keys, not fail on the primary key.
func TestPutMediaRefUpserts(t *testing.T) {
	h := newHist(t)
	h.putMediaRef("MSG1", mediaRef{directPath: "/old", mediaType: "WhatsApp Image Keys"})
	h.putMediaRef("MSG1", mediaRef{directPath: "/new", mediaType: "WhatsApp Video Keys"})
	got, ok := h.mediaRefFor("MSG1")
	if !ok || got.directPath != "/new" || got.mediaType != "WhatsApp Video Keys" {
		t.Errorf("upsert failed: %+v ok=%v", got, ok)
	}
}

// Serving media needs a content type after a restart, when the in-memory mime
// table is gone. Prefer the media_keys row, fall back to the message row.
func TestMimeForMessageFallsBackToMessageRow(t *testing.T) {
	h := newHist(t)
	h.putMessage(ws.MsgData{
		MsgID: "MSG1", ChatJID: "c@s.whatsapp.net", Timestamp: 1,
		Kind: "image", Mime: "image/png", MediaURL: "/media/MSG1",
	})
	if got := h.mimeForMessage("MSG1"); got != "image/png" {
		t.Errorf("mime from message row = %q, want image/png", got)
	}
	// The media_keys row wins when present.
	h.putMediaRef("MSG1", mediaRef{directPath: "/p", mime: "image/webp"})
	if got := h.mimeForMessage("MSG1"); got != "image/webp" {
		t.Errorf("mime = %q, want the media_keys value image/webp", got)
	}
	if got := h.mimeForMessage("NOPE"); got != "" {
		t.Errorf("unknown id mime = %q, want empty", got)
	}
}

// Only attachments with no stored keys need re-fetching; ones that already have
// keys, and plain text messages, must not be reported as gaps.
func TestMediaMessagesWithoutKeys(t *testing.T) {
	h := newHist(t)
	chat := "g@g.us"
	// Two attachments, one text message.
	h.putMessage(ws.MsgData{MsgID: "IMG_OLD", ChatJID: chat, Timestamp: 100,
		Kind: "image", MediaURL: "/media/IMG_OLD"})
	h.putMessage(ws.MsgData{MsgID: "IMG_NEW", ChatJID: chat, Timestamp: 200,
		Kind: "image", MediaURL: "/media/IMG_NEW"})
	putMsg(h, chat, "TXT", "x@s.whatsapp.net", "X", "hello", 150, false)
	// Only the newer one has keys.
	h.putMediaRef("IMG_NEW", mediaRef{directPath: "/p", mediaType: "WhatsApp Image Keys"})

	gaps := h.mediaMessagesWithoutKeys()
	if len(gaps) != 1 {
		t.Fatalf("gaps = %d (%+v), want 1", len(gaps), gaps)
	}
	if gaps[0].MsgID != "IMG_OLD" {
		t.Errorf("gap msgid = %q, want IMG_OLD", gaps[0].MsgID)
	}
	if gaps[0].Chat != chat || gaps[0].TS != 100 {
		t.Errorf("gap = %+v, want chat %s ts 100", gaps[0], chat)
	}

	if got := h.countStoredMediaKeys(); got != 1 {
		t.Errorf("countStoredMediaKeys = %d, want 1", got)
	}
}

// Gaps must come back newest-first, since the refetch walks history backwards.
func TestMediaMessagesWithoutKeysOrderedNewestFirst(t *testing.T) {
	h := newHist(t)
	chat := "g@g.us"
	for i, ts := range []int64{100, 300, 200} {
		h.putMessage(ws.MsgData{
			MsgID: string(rune('A' + i)), ChatJID: chat, Timestamp: ts, Kind: "image",
		})
	}
	gaps := h.mediaMessagesWithoutKeys()
	if len(gaps) != 3 {
		t.Fatalf("gaps = %d, want 3", len(gaps))
	}
	for i := 1; i < len(gaps); i++ {
		if gaps[i-1].TS < gaps[i].TS {
			t.Errorf("gaps not newest-first: %v", gaps)
			break
		}
	}
}

// Every media kind the app renders must be recognised as a possible gap,
// otherwise those attachments would silently never be recovered.
func TestMediaMessagesWithoutKeysCoversAllKinds(t *testing.T) {
	h := newHist(t)
	kinds := []string{"image", "video", "audio", "gif", "sticker", "doc"}
	for i, k := range kinds {
		h.putMessage(ws.MsgData{
			MsgID: "M" + k, ChatJID: "c@s.whatsapp.net",
			Timestamp: int64(100 + i), Kind: k,
		})
	}
	if got := len(h.mediaMessagesWithoutKeys()); got != len(kinds) {
		t.Errorf("gaps = %d, want %d (one per media kind)", got, len(kinds))
	}
}

// Receipts arrive out of order — one recipient's "delivered" can land after
// another's "read" — so a status must never move backwards.
func TestSetMessageStatusOnlyAdvances(t *testing.T) {
	h := newHist(t)
	putMsg(h, "c@s.whatsapp.net", "m1", "me", "You", "hi", 100, true)

	if !h.setMessageStatus("m1", "delivered") {
		t.Error("delivered should have been applied")
	}
	if !h.setMessageStatus("m1", "read") {
		t.Error("read should have been applied over delivered")
	}
	// A late "delivered" must be ignored, not demote the message.
	if h.setMessageStatus("m1", "delivered") {
		t.Error("delivered must not overwrite read")
	}
	if got := h.history("c@s.whatsapp.net", 0, 10)[0].Status; got != "read" {
		t.Errorf("status = %q, want read", got)
	}
	// Re-applying the same status is not a change.
	if h.setMessageStatus("m1", "read") {
		t.Error("re-applying the same status should report no change")
	}
	if h.setMessageStatus("nosuch", "read") {
		t.Error("unknown message should report no change")
	}
}

func TestReactionsReplaceAndRemove(t *testing.T) {
	h := newHist(t)
	chat, msg := "g@g.us", "m1"
	putMsg(h, chat, msg, "x@s.whatsapp.net", "X", "hi", 100, false)

	h.putReaction(chat, msg, "a@s.whatsapp.net", "Alex", "👍", 110)
	h.putReaction(chat, msg, "b@s.whatsapp.net", "Bea", "👍", 111)
	h.putReaction(chat, msg, "c@s.whatsapp.net", "Cem", "❤️", 112)

	if got := h.reactionsForMessage(chat, msg); len(got) != 3 {
		t.Fatalf("reactions = %d, want 3", len(got))
	}

	// Reacting again replaces that person's emoji rather than adding a row.
	h.putReaction(chat, msg, "a@s.whatsapp.net", "Alex", "😂", 120)
	got := h.reactionsForMessage(chat, msg)
	if len(got) != 3 {
		t.Fatalf("after re-react, reactions = %d, want 3", len(got))
	}
	for _, r := range got {
		if r.SenderJID == "a@s.whatsapp.net" && r.Emoji != "😂" {
			t.Errorf("Alex's emoji = %q, want 😂", r.Emoji)
		}
	}

	// An empty emoji means the reaction was withdrawn.
	h.putReaction(chat, msg, "b@s.whatsapp.net", "Bea", "", 130)
	if got := h.reactionsForMessage(chat, msg); len(got) != 2 {
		t.Errorf("after removal, reactions = %d, want 2", len(got))
	}
}

// The history page decorates messages with their reactions in one query; each
// message must get its own, and messages without any must get none.
func TestHistoryCarriesReactions(t *testing.T) {
	h := newHist(t)
	chat := "g@g.us"
	putMsg(h, chat, "m1", "x@s.whatsapp.net", "X", "reacted to", 100, false)
	putMsg(h, chat, "m2", "x@s.whatsapp.net", "X", "plain", 101, false)
	h.putReaction(chat, "m1", "a@s.whatsapp.net", "Alex", "👍", 110)

	byID := map[string][]ws.ReactionData{}
	for _, m := range h.history(chat, 0, 10) {
		byID[m.MsgID] = m.Reactions
	}
	if len(byID["m1"]) != 1 || byID["m1"][0].Emoji != "👍" {
		t.Errorf("m1 reactions = %+v, want one 👍", byID["m1"])
	}
	if len(byID["m2"]) != 0 {
		t.Errorf("m2 should have no reactions, got %+v", byID["m2"])
	}
}

// Read receipts should cover only incoming, not-yet-read messages — never our
// own, and never ones already acknowledged.
func TestUnreadMessageIDsAndMarking(t *testing.T) {
	h := newHist(t)
	chat := "g@g.us"
	putMsg(h, chat, "in1", "x@s.whatsapp.net", "X", "a", 100, false)
	putMsg(h, chat, "in2", "y@s.whatsapp.net", "Y", "b", 101, false)
	putMsg(h, chat, "mine", "me", "You", "c", 102, true)

	ids, sender := h.unreadMessageIDs(chat, 50)
	if len(ids) != 2 {
		t.Fatalf("unread = %v, want the 2 incoming messages", ids)
	}
	if sender == "" {
		t.Error("expected a sender for the group receipt")
	}
	for _, id := range ids {
		if id == "mine" {
			t.Error("our own message must not need a read receipt")
		}
	}

	h.markMessagesRead(chat, ids)
	if left, _ := h.unreadMessageIDs(chat, 50); len(left) != 0 {
		t.Errorf("after marking, unread = %v, want none", left)
	}
}

// Location coordinates and labels must survive a round trip, since the map card
// and the "open in maps" action both depend on them.
func TestLocationRoundTrip(t *testing.T) {
	h := newHist(t)
	chat := "c@s.whatsapp.net"
	h.putMessage(ws.MsgData{
		MsgID: "LOC1", ChatJID: chat, Timestamp: 100, Kind: "location",
		Lat: 41.0082, Lon: 28.9784,
		LocName: "Sultanahmet", LocAddress: "Fatih/İstanbul",
		MediaURL: "/locthumb/LOC1", Mime: "image/jpeg",
	})

	msgs := h.history(chat, 0, 10)
	if len(msgs) != 1 {
		t.Fatalf("stored %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Kind != "location" {
		t.Errorf("kind = %q, want location", m.Kind)
	}
	if m.Lat < 41.008 || m.Lat > 41.009 || m.Lon < 28.978 || m.Lon > 28.979 {
		t.Errorf("coords = %v,%v — precision lost", m.Lat, m.Lon)
	}
	if m.LocName != "Sultanahmet" || m.LocAddress != "Fatih/İstanbul" {
		t.Errorf("labels = %q / %q", m.LocName, m.LocAddress)
	}
}

// The map preview is stored bytes rather than a CDN download, so it must always
// be retrievable — that's what makes locations work with no keys and no network.
func TestLocationThumbRoundTrip(t *testing.T) {
	h := newHist(t)
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}
	h.putLocationThumb("LOC1", jpeg)

	got := h.locationThumb("LOC1")
	if string(got) != string(jpeg) {
		t.Errorf("thumb = %v, want %v", got, jpeg)
	}
	if h.locationThumb("NOPE") != nil {
		t.Error("unknown id should have no thumbnail")
	}
	// Re-receiving the message must replace, not fail on the primary key.
	h.putLocationThumb("LOC1", []byte{1, 2})
	if len(h.locationThumb("LOC1")) != 2 {
		t.Error("thumbnail did not update")
	}
	// An empty thumbnail is not worth a row.
	h.putLocationThumb("LOC2", nil)
	if h.locationThumb("LOC2") != nil {
		t.Error("empty thumbnail should not be stored")
	}
}

// Unread is derived from messages never marked read, which is what makes it
// survive a refresh — nothing is stored per-chat that could drift.
func TestUnreadCountDerivesFromMessages(t *testing.T) {
	h := newHist(t)
	chat := "g@g.us"
	putMsg(h, chat, "in1", "x@s.whatsapp.net", "X", "a", 100, false)
	putMsg(h, chat, "in2", "x@s.whatsapp.net", "X", "b", 101, false)
	putMsg(h, chat, "mine", "me", "You", "c", 102, true)

	if got := h.unreadCount(chat); got != 2 {
		t.Errorf("unread = %d, want 2 (our own message must not count)", got)
	}
	// listChats has to report the same number, since that's what the badge uses.
	rows := h.listChats()
	if len(rows) != 1 || rows[0]["unread"].(int64) != 2 {
		t.Errorf("listChats unread = %v, want 2", rows[0]["unread"])
	}

	if n := h.markAllRead(chat); n != 2 {
		t.Errorf("markAllRead reported %d, want 2", n)
	}
	if got := h.unreadCount(chat); got != 0 {
		t.Errorf("after reading, unread = %d, want 0", got)
	}
	// Re-reading an already-read chat is a no-op.
	if n := h.markAllRead(chat); n != 0 {
		t.Errorf("second markAllRead reported %d, want 0", n)
	}
}

// markAllRead must clear the whole chat, not just the batch that got receipts —
// otherwise the badge sticks above 50 unread.
func TestMarkAllReadClearsBeyondReceiptBatch(t *testing.T) {
	h := newHist(t)
	chat := "busy@g.us"
	for i := 0; i < 120; i++ {
		putMsg(h, chat, "m"+string(rune('A'+i%26))+string(rune('0'+i/26)),
			"x@s.whatsapp.net", "X", "spam", int64(100+i), false)
	}
	before := h.unreadCount(chat)
	if before < 100 {
		t.Fatalf("expected a large unread count, got %d", before)
	}
	h.markAllRead(chat)
	if got := h.unreadCount(chat); got != 0 {
		t.Errorf("unread = %d after markAllRead, want 0", got)
	}
}

// Existing history predates read-tracking and has no status, which a naive count
// would read as tens of thousands of unread messages. Opening an old database
// must treat that history as already seen — exactly once.
func TestUnreadBaselineAppliedOnceOnUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// A database shaped like the pre-read-tracking release.
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE messages (
			msgid TEXT, chat TEXT, sender TEXT, sendername TEXT, fromme INTEGER,
			ts INTEGER, kind TEXT, text TEXT, media TEXT, mime TEXT,
			quoted_id TEXT, quoted_text TEXT, quoted_name TEXT,
			PRIMARY KEY (chat, msgid));
		CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, is_group INTEGER,
			pinned INTEGER, last_ts INTEGER, preview TEXT);
		INSERT INTO chats VALUES ('c@s.whatsapp.net','Alex',0,0,100,'hi');
		INSERT INTO messages VALUES ('m1','c@s.whatsapp.net','c@s.whatsapp.net','Alex',0,100,'text','old',NULL,NULL,NULL,NULL,NULL);
		INSERT INTO messages VALUES ('m2','c@s.whatsapp.net','c@s.whatsapp.net','Alex',0,101,'text','older',NULL,NULL,NULL,NULL,NULL);`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	db.Close()

	h, err := openHistStore(path)
	if err != nil {
		t.Fatalf("openHistStore: %v", err)
	}
	if got := h.unreadCount("c@s.whatsapp.net"); got != 0 {
		t.Errorf("existing history reported %d unread; it should be treated as seen", got)
	}
	h.close()

	// A genuinely new message after the upgrade must still count.
	h2, err := openHistStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer h2.close()
	putMsg(h2, "c@s.whatsapp.net", "new", "c@s.whatsapp.net", "Alex", "fresh", 200, false)
	if got := h2.unreadCount("c@s.whatsapp.net"); got != 1 {
		t.Errorf("new message unread = %d, want 1 (baseline must not re-run)", got)
	}
}

// Push endpoints are capability URLs — anyone holding one can wake the phone —
// so they must round-trip exactly and be removable when the push service says
// a subscription is gone.
func TestPushSubscriptionLifecycle(t *testing.T) {
	h := newHist(t)
	ep := "https://push.kaiostech.com/wpush/v2/gAAAAABm-longopaquetoken"

	if len(h.listPushSubs()) != 0 {
		t.Fatal("expected no subscriptions initially")
	}
	h.putPushSub(ep, 100)
	subs := h.listPushSubs()
	if len(subs) != 1 || subs[0].Endpoint != ep {
		t.Fatalf("subs = %+v, want the one endpoint", subs)
	}

	// Re-registering the same endpoint must update, not duplicate.
	h.putPushSub(ep, 200)
	if len(h.listPushSubs()) != 1 {
		t.Errorf("re-registering created a duplicate: %+v", h.listPushSubs())
	}

	h.deletePushSub(ep)
	if len(h.listPushSubs()) != 0 {
		t.Error("delete did not remove the subscription")
	}
	// An empty endpoint is not a subscription.
	h.putPushSub("", 300)
	if len(h.listPushSubs()) != 0 {
		t.Error("empty endpoint should not be stored")
	}
}

// The endpoint is a secret; logs must not carry the whole thing.
func TestShortenEndpointHidesTheToken(t *testing.T) {
	ep := "https://push.kaiostech.com/wpush/v2/gAAAAABm-verylongsecrettokenhere"
	got := shortenEndpoint(ep)
	if strings.Contains(got, "verylongsecrettokenhere") {
		t.Errorf("shortened form still leaks the token: %q", got)
	}
	if !strings.Contains(got, "push.kaiostech.com") {
		t.Errorf("shortened form should keep the host for diagnosis: %q", got)
	}
}

// An edit must rewrite the stored body in place, and must not invent a row for
// a message we never had — a bodiless message in the thread would be worse than
// ignoring an edit for something outside our history.
func TestEditMessageText(t *testing.T) {
	h := newHist(t)
	chat := "c@s.whatsapp.net"
	putMsg(h, chat, "m1", chat, "Alex", "helo wrold", 100, false)

	if !h.editMessageText(chat, "m1", "hello world") {
		t.Fatal("edit of a known message should apply")
	}
	if got := h.history(chat, 0, 10)[0].Text; got != "hello world" {
		t.Errorf("text = %q, want the edited body", got)
	}
	// The preview quotes the message, so it must follow.
	if got := h.listChats()[0]["preview"]; got != "hello world" {
		t.Errorf("preview = %v, want it updated too", got)
	}

	if h.editMessageText(chat, "nosuch", "whatever") {
		t.Error("edit of an unknown message should report no change")
	}
	if len(h.history(chat, 0, 10)) != 1 {
		t.Error("an unknown edit must not create a message")
	}
}

// "Delete for me" removes our copy and its reactions, and touches nothing else.
func TestDeleteMessageLocalOnly(t *testing.T) {
	h := newHist(t)
	chat := "g@g.us"
	putMsg(h, chat, "m1", "x@s.whatsapp.net", "X", "keep", 100, false)
	putMsg(h, chat, "m2", "x@s.whatsapp.net", "X", "remove", 101, false)
	h.putReaction(chat, "m2", "a@s.whatsapp.net", "Alex", "👍", 110)

	h.deleteMessage(chat, "m2")

	msgs := h.history(chat, 0, 10)
	if len(msgs) != 1 || msgs[0].MsgID != "m1" {
		t.Errorf("after delete, messages = %+v, want only m1", msgs)
	}
	if len(h.reactionsForMessage(chat, "m2")) != 0 {
		t.Error("the deleted message's reactions should go with it")
	}
}

// A message's display name is resolved from the sender JID when it's read, not
// taken from whatever was written into the row when it arrived. That's what
// makes saving a contact fix their entire history at once, and it's why the
// old repair passes existed — rows carried a stale snapshot that nothing
// updated.
func TestSenderNamesResolveAtReadTime(t *testing.T) {
	h := newHist(t)
	c := &Client{hist: h}
	chat := "1234@s.whatsapp.net"
	sender := "9999@s.whatsapp.net"

	// Stored with a name from before the resolver knew any better.
	putMsg(h, chat, "m1", sender, "999999999", "hello", 100, false)

	// Nothing resolves, so this is a stranger: the number gets the same tilde
	// an unsaved person with a push name would get.
	got := c.resolveSenderNames(h.history(chat, 0, 10))
	if len(got) != 1 {
		t.Fatalf("history returned %d messages, want 1", len(got))
	}
	if got[0].SenderName != "~999999999" {
		t.Errorf("unknown sender = %q, want %q", got[0].SenderName, "~999999999")
	}

	// Save a contact — the message was written long before this happened.
	h.setLocalContact(sender, "Bulgayrian", 1)

	got = c.resolveSenderNames(h.history(chat, 0, 10))
	if got[0].SenderName != "Bulgayrian" {
		t.Errorf("after saving = %q, want %q — the stored row was never rewritten",
			got[0].SenderName, "Bulgayrian")
	}

	// The row itself is untouched: resolution happens on the way out.
	raw := h.history(chat, 0, 10)
	if raw[0].SenderName != "999999999" {
		t.Errorf("stored row = %q, want it left alone at %q",
			raw[0].SenderName, "999999999")
	}
}

// Our own messages aren't relabelled — "fromme" has no sender to resolve.
func TestOwnMessagesKeepTheirName(t *testing.T) {
	h := newHist(t)
	c := &Client{hist: h}
	chat := "1234@s.whatsapp.net"
	putMsg(h, chat, "m1", "", "You", "hi", 100, true)

	got := c.resolveSenderNames(h.history(chat, 0, 10))
	if got[0].SenderName != "You" {
		t.Errorf("own message = %q, want %q", got[0].SenderName, "You")
	}
}

// A real name that isn't in your contacts keeps its tilde; a name you saved
// doesn't get one; and a number nobody could name gets one too.
func TestUnknownSenderLabelling(t *testing.T) {
	h := newHist(t)
	c := &Client{hist: h}

	cases := []struct {
		name   string
		jid    string
		stored string
		want   string
	}{
		{"bare number gets a tilde", "9999@s.whatsapp.net", "9999", "~9999"},
		{"a real push name is left as stored", "8888@s.whatsapp.net", "Sarp", "Sarp"},
		{"no stored name falls back to the number", "7777@s.whatsapp.net", "", "~7777"},
		{"a LID with no mapping has no number to show", "5555@lid", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.unknownSenderLabel(tc.jid, tc.stored); got != tc.want {
				t.Errorf("unknownSenderLabel(%q, %q) = %q, want %q",
					tc.jid, tc.stored, got, tc.want)
			}
		})
	}
}

// The picker shows pictures, so the same sticker sent five times has to be one
// entry. Five copies of one sticker is a grid nobody can use, and on a 240px
// screen it is most of the grid.
func TestRecentStickersAreDistinctAndNewestFirst(t *testing.T) {
	h := newHist(t)
	chat := "1234@s.whatsapp.net"

	put := func(id, media string, ts int64) {
		h.putMessage(ws.MsgData{
			MsgID: id, ChatJID: chat, Kind: "sticker",
			MediaURL: media, Mime: "image/webp", Timestamp: ts,
		})
	}
	put("a1", "/media/a1", 100)
	put("b1", "/media/b1", 200)
	put("a2", "/media/a1", 300) // the same sticker again, later
	h.putMessage(ws.MsgData{
		MsgID: "t1", ChatJID: chat, Kind: "text", Text: "not a sticker", Timestamp: 400,
	})

	got := h.recentStickers(0)
	if len(got) != 2 {
		t.Fatalf("got %d stickers, want 2 distinct ones", len(got))
	}
	for _, m := range got {
		if m.MediaURL == "" {
			t.Error("a sticker with no media reached the picker; it would render blank")
		}
		if m.Favourite {
			t.Error("a sticker from message history was marked as a favourite")
		}
	}
	// Newest first, and the repeat carries the LATER timestamp: a sticker you
	// used a minute ago should not sit at the bottom because you first saw it
	// last year.
	if got[0].Timestamp < got[1].Timestamp {
		t.Errorf("order is oldest-first: %d then %d", got[0].Timestamp, got[1].Timestamp)
	}
	if got[0].Timestamp != 300 {
		t.Errorf("newest entry ts = %d, want 300 (the repeat's time, not its first sighting)",
			got[0].Timestamp)
	}
}

// A sticker whose media reference is empty cannot be rendered or resent, so it
// must not reach the grid at all.
func TestRecentStickersSkipsUnfetchableOnes(t *testing.T) {
	h := newHist(t)
	h.putMessage(ws.MsgData{
		MsgID: "x1", ChatJID: "1234@s.whatsapp.net", Kind: "sticker", Timestamp: 100,
	})
	if got := h.recentStickers(0); len(got) != 0 {
		t.Errorf("got %d stickers, want none — that one has no media", len(got))
	}
}

// Favourites are stored as an attachment under a synthetic message id, which
// is what lets the existing media paths serve and send one without knowing it
// is not a message. If that id shape or the media_keys row goes missing, the
// picker shows a cell that never loads.
func TestFavouriteStickersAreStoredAsFetchableAttachments(t *testing.T) {
	h := newHist(t)
	h.putMediaRef("fav:abc", mediaRef{
		directPath: "/v/t62.7118-24/abc", mediaKey: []byte("k"),
		encSHA256: []byte("e"), mediaType: "image", mime: "image/webp",
	})
	h.putFavouriteSticker("abc", 1700000000000)

	got := h.favouriteStickers(0)
	if len(got) != 1 {
		t.Fatalf("got %d favourites, want 1", len(got))
	}
	if got[0].MsgID != "fav:abc" {
		t.Errorf("id = %q, want fav:abc", got[0].MsgID)
	}
	if got[0].MediaURL != "/media/fav:abc" {
		t.Errorf("media url = %q; the picker fetches this literally", got[0].MediaURL)
	}
	if !got[0].Favourite {
		t.Error("a favourite was not marked as one")
	}
	// Milliseconds in, seconds out — everything else in this store is seconds,
	// and a favourite dated in 55000 AD sorts above everything forever.
	if got[0].Timestamp != 1700000000 {
		t.Errorf("ts = %d, want 1700000000 (seconds, not millis)", got[0].Timestamp)
	}
	if _, ok := h.mediaRefFor("fav:abc"); !ok {
		t.Error("no media keys stored; the sticker could never be downloaded")
	}

	// Unfavouriting has to remove BOTH rows. Leaving the media row behind would
	// keep it downloadable but invisible, which is the kind of half-deletion
	// nobody ever notices until the table is huge.
	h.deleteFavouriteSticker("abc")
	if len(h.favouriteStickers(0)) != 0 {
		t.Error("an unfavourited sticker is still in the list")
	}
	if _, ok := h.mediaRefFor("fav:abc"); ok {
		t.Error("its media keys were left behind")
	}
}
