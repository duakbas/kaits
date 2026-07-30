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
