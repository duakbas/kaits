package wa

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	_ "github.com/mattn/go-sqlite3"
)

// newFakeSession builds a database shaped like whatsmeow's session store so the
// direct-table resolver can be exercised without a real paired account. The
// schema mirrors store/sqlstore: whatsmeow_lid_map(lid, pn) holds BARE user ids
// (no @server), whatsmeow_contacts is keyed by the full non-AD JID string.
func newFakeSession(t *testing.T, withTables bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wa-session.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open fake session db: %v", err)
	}
	defer db.Close()

	if !withTables {
		if _, err := db.Exec(`CREATE TABLE unrelated (x TEXT)`); err != nil {
			t.Fatalf("create unrelated: %v", err)
		}
		return path
	}

	_, err = db.Exec(`
		CREATE TABLE whatsmeow_lid_map (
			lid TEXT PRIMARY KEY,
			pn  TEXT UNIQUE
		);
		CREATE TABLE whatsmeow_contacts (
			our_jid       TEXT,
			their_jid     TEXT,
			first_name    TEXT,
			full_name     TEXT,
			push_name     TEXT,
			business_name TEXT,
			PRIMARY KEY (our_jid, their_jid)
		);`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return path
}

func mustExec(t *testing.T, path, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open for write: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func TestSessionStoreMapsLIDToPhoneBothWays(t *testing.T) {
	path := newFakeSession(t, true)
	mustExec(t, path, `INSERT INTO whatsmeow_lid_map (lid, pn) VALUES (?,?)`,
		"158041961934877", "41791234567")

	s, err := openSessionStore(path)
	if err != nil {
		t.Fatalf("openSessionStore: %v", err)
	}
	defer s.close()

	if got := s.pnForLID("158041961934877"); got != "41791234567" {
		t.Errorf("pnForLID = %q, want %q", got, "41791234567")
	}
	if got := s.lidForPN("41791234567"); got != "158041961934877" {
		t.Errorf("lidForPN = %q, want %q", got, "158041961934877")
	}
	// A LID the map has never heard of must resolve to "" rather than guessing.
	if got := s.pnForLID("999999999999"); got != "" {
		t.Errorf("pnForLID(unknown) = %q, want empty", got)
	}
}

// A hit must populate the reverse direction too, since the row read covers both.
func TestSessionStoreCachesBothDirections(t *testing.T) {
	path := newFakeSession(t, true)
	mustExec(t, path, `INSERT INTO whatsmeow_lid_map (lid, pn) VALUES (?,?)`, "111", "222")

	s, err := openSessionStore(path)
	if err != nil {
		t.Fatalf("openSessionStore: %v", err)
	}
	defer s.close()

	s.pnForLID("111") // forward lookup only

	s.mu.RLock()
	reverse, ok := s.pnToLID["222"]
	s.mu.RUnlock()
	if !ok || reverse != "111" {
		t.Errorf("reverse cache = %q/%v, want 111/true", reverse, ok)
	}
}

// The bug this whole change exists to fix: the address book stores the saved
// name under the PHONE jid, but group messages arrive addressed by LID. Looking
// up the LID alone finds nothing; following the lid map to the phone jid works.
func TestContactNameFoundViaLIDCounterpart(t *testing.T) {
	path := newFakeSession(t, true)
	mustExec(t, path, `INSERT INTO whatsmeow_lid_map (lid, pn) VALUES (?,?)`,
		"158041961934877", "41791234567")
	mustExec(t, path, `INSERT INTO whatsmeow_contacts (our_jid, their_jid, full_name) VALUES (?,?,?)`,
		"me@s.whatsapp.net", "41791234567@s.whatsapp.net", "Alex Rivera")

	s, err := openSessionStore(path)
	if err != nil {
		t.Fatalf("openSessionStore: %v", err)
	}
	defer s.close()

	lid := types.JID{User: "158041961934877", Server: types.HiddenUserServer}
	if got := s.contactName(lid); got != "Alex Rivera" {
		t.Errorf("contactName(lid) = %q, want %q", got, "Alex Rivera")
	}

	// And the same lookup from the phone side still works.
	pn := types.JID{User: "41791234567", Server: types.DefaultUserServer}
	if got := s.contactName(pn); got != "Alex Rivera" {
		t.Errorf("contactName(pn) = %q, want %q", got, "Alex Rivera")
	}
}

// A device suffix (<lid>:43@lid) must not defeat the lookup — the contacts
// table is keyed by non-AD JIDs.
func TestContactNameIgnoresDeviceSuffix(t *testing.T) {
	path := newFakeSession(t, true)
	mustExec(t, path, `INSERT INTO whatsmeow_contacts (our_jid, their_jid, full_name) VALUES (?,?,?)`,
		"me@s.whatsapp.net", "41791234567@s.whatsapp.net", "Alex Rivera")

	s, err := openSessionStore(path)
	if err != nil {
		t.Fatalf("openSessionStore: %v", err)
	}
	defer s.close()

	withDevice := types.JID{User: "41791234567", Device: 43, Server: types.DefaultUserServer}
	if got := s.contactName(withDevice); got != "Alex Rivera" {
		t.Errorf("contactName(device jid) = %q, want %q", got, "Alex Rivera")
	}
}

// The name the user saved themselves must beat the name the contact set for
// themselves, even when the push name sits on the row that matched first.
func TestContactNamePrefersSavedNameAcrossRows(t *testing.T) {
	path := newFakeSession(t, true)
	mustExec(t, path, `INSERT INTO whatsmeow_lid_map (lid, pn) VALUES (?,?)`, "5550001", "41791234567")
	// The LID row carries only a self-set push name...
	mustExec(t, path, `INSERT INTO whatsmeow_contacts (our_jid, their_jid, push_name) VALUES (?,?,?)`,
		"me@s.whatsapp.net", "5550001@lid", "xX_alex_Xx")
	// ...while the phone row carries the name from the address book.
	mustExec(t, path, `INSERT INTO whatsmeow_contacts (our_jid, their_jid, full_name) VALUES (?,?,?)`,
		"me@s.whatsapp.net", "41791234567@s.whatsapp.net", "Alex Rivera")

	s, err := openSessionStore(path)
	if err != nil {
		t.Fatalf("openSessionStore: %v", err)
	}
	defer s.close()

	lid := types.JID{User: "5550001", Server: types.HiddenUserServer}
	if got := s.contactName(lid); got != "Alex Rivera" {
		t.Errorf("contactName = %q, want saved name %q", got, "Alex Rivera")
	}
}

// When BOTH rows carry a full_name — your saved nickname on the phone row and
// the contact's own profile name on the LID row — the saved one must win. Row
// order from sqlite is arbitrary, so without an explicit address ordering the
// same person renders under different names in different groups.
func TestContactNamePrefersPhoneRowWhenBothHaveFullName(t *testing.T) {
	path := newFakeSession(t, true)
	mustExec(t, path, `INSERT INTO whatsmeow_lid_map (lid, pn) VALUES (?,?)`,
		"104570072096833", "905322873800")
	// Inserted LID-row first on purpose: it is what a naive query returns first.
	mustExec(t, path, `INSERT INTO whatsmeow_contacts (our_jid, their_jid, full_name) VALUES (?,?,?)`,
		"me@s.whatsapp.net", "104570072096833@lid", "Sarp Doruk Gerenli")
	mustExec(t, path, `INSERT INTO whatsmeow_contacts (our_jid, their_jid, full_name) VALUES (?,?,?)`,
		"me@s.whatsapp.net", "905322873800@s.whatsapp.net", "bulgayrian")

	s, err := openSessionStore(path)
	if err != nil {
		t.Fatalf("openSessionStore: %v", err)
	}
	defer s.close()

	// Reached from the group (a LID) and from the DM (a phone JID), the answer
	// has to be the same — that consistency is the whole point.
	lid := types.JID{User: "104570072096833", Server: types.HiddenUserServer}
	if got := s.contactName(lid); got != "bulgayrian" {
		t.Errorf("contactName(lid) = %q, want saved name %q", got, "bulgayrian")
	}
	s.reset()
	pn := types.JID{User: "905322873800", Server: types.DefaultUserServer}
	if got := s.contactName(pn); got != "bulgayrian" {
		t.Errorf("contactName(pn) = %q, want saved name %q", got, "bulgayrian")
	}
}

// The phone row wins only within a column: a LID-row full_name still beats a
// phone-row push_name, because "you saved it" outranks "they named themselves".
func TestContactNameColumnBeatsAddressOrder(t *testing.T) {
	path := newFakeSession(t, true)
	mustExec(t, path, `INSERT INTO whatsmeow_lid_map (lid, pn) VALUES (?,?)`, "555", "9051112233")
	mustExec(t, path, `INSERT INTO whatsmeow_contacts (our_jid, their_jid, push_name) VALUES (?,?,?)`,
		"me@s.whatsapp.net", "9051112233@s.whatsapp.net", "selfnamed")
	mustExec(t, path, `INSERT INTO whatsmeow_contacts (our_jid, their_jid, full_name) VALUES (?,?,?)`,
		"me@s.whatsapp.net", "555@lid", "Saved Name")

	s, err := openSessionStore(path)
	if err != nil {
		t.Fatalf("openSessionStore: %v", err)
	}
	defer s.close()

	lid := types.JID{User: "555", Server: types.HiddenUserServer}
	if got := s.contactName(lid); got != "Saved Name" {
		t.Errorf("contactName = %q, want %q (full_name outranks push_name)", got, "Saved Name")
	}
}

func TestContactNameFallsBackThroughPreferenceOrder(t *testing.T) {
	path := newFakeSession(t, true)
	mustExec(t, path, `INSERT INTO whatsmeow_contacts (our_jid, their_jid, push_name, business_name) VALUES (?,?,?,?)`,
		"me@s.whatsapp.net", "41790000001@s.whatsapp.net", "", "Rivera Plumbing")
	mustExec(t, path, `INSERT INTO whatsmeow_contacts (our_jid, their_jid, first_name, push_name) VALUES (?,?,?,?)`,
		"me@s.whatsapp.net", "41790000002@s.whatsapp.net", "Sam", "sammy")

	s, err := openSessionStore(path)
	if err != nil {
		t.Fatalf("openSessionStore: %v", err)
	}
	defer s.close()

	business := types.JID{User: "41790000001", Server: types.DefaultUserServer}
	if got := s.contactName(business); got != "Rivera Plumbing" {
		t.Errorf("business contactName = %q, want %q", got, "Rivera Plumbing")
	}
	first := types.JID{User: "41790000002", Server: types.DefaultUserServer}
	if got := s.contactName(first); got != "Sam" {
		t.Errorf("first_name should beat push_name, got %q", got)
	}
}

// An unknown contact yields "" so the caller can fall back to the number,
// and the miss must not be cached as a permanent negative.
func TestContactNameUnknownIsEmptyAndRetried(t *testing.T) {
	path := newFakeSession(t, true)
	s, err := openSessionStore(path)
	if err != nil {
		t.Fatalf("openSessionStore: %v", err)
	}
	defer s.close()

	jid := types.JID{User: "41790000009", Server: types.DefaultUserServer}
	if got := s.contactName(jid); got != "" {
		t.Errorf("contactName(unknown) = %q, want empty", got)
	}

	s.mu.RLock()
	e, ok := s.names[jid.String()]
	s.mu.RUnlock()
	if !ok || e.at.IsZero() {
		t.Fatalf("miss should be cached with a timestamp for TTL re-check, got %+v", e)
	}
	if e.name != "" {
		t.Errorf("cached miss should hold no name, got %q", e.name)
	}
}

// An unmapped LID must be remembered as a miss, so a bulk history sync doesn't
// re-query sqlite once per message for the same unknown sender — but the miss
// must clear as soon as the mapping actually lands.
func TestLIDMissIsCachedThenClearedOnHit(t *testing.T) {
	path := newFakeSession(t, true)
	s, err := openSessionStore(path)
	if err != nil {
		t.Fatalf("openSessionStore: %v", err)
	}
	defer s.close()

	if got := s.pnForLID("5550009"); got != "" {
		t.Fatalf("pnForLID(unmapped) = %q, want empty", got)
	}
	s.mu.RLock()
	_, missed := s.lidMiss["5550009"]
	s.mu.RUnlock()
	if !missed {
		t.Fatal("miss should have been recorded")
	}

	// The mapping arrives, and the cached miss ages out.
	mustExec(t, path, `INSERT INTO whatsmeow_lid_map (lid, pn) VALUES (?,?)`, "5550009", "41791110000")
	s.mu.Lock()
	s.lidMiss["5550009"] = time.Now().Add(-2 * missTTL)
	s.mu.Unlock()

	if got := s.pnForLID("5550009"); got != "41791110000" {
		t.Errorf("pnForLID after mapping landed = %q, want %q", got, "41791110000")
	}
	s.mu.RLock()
	_, stillMissed := s.lidMiss["5550009"]
	s.mu.RUnlock()
	if stillMissed {
		t.Error("miss entry should be cleared once the mapping resolves")
	}
}

// Schema drift in whatsmeow must be detected at open time, not at the first
// lookup, so the daemon can log it once and fall back cleanly.
func TestOpenSessionStoreRejectsMissingTables(t *testing.T) {
	path := newFakeSession(t, false)
	s, err := openSessionStore(path)
	if err == nil {
		s.close()
		t.Fatal("expected an error when whatsmeow tables are absent")
	}
}

// Every method must tolerate a nil receiver, because the daemon sets sess=nil
// when the session db can't be opened and calls straight through.
func TestNilSessionStoreIsSafe(t *testing.T) {
	var s *sessionStore
	if got := s.pnForLID("123"); got != "" {
		t.Errorf("nil pnForLID = %q", got)
	}
	if got := s.lidForPN("123"); got != "" {
		t.Errorf("nil lidForPN = %q", got)
	}
	if got := s.contactName(types.JID{User: "123", Server: types.DefaultUserServer}); got != "" {
		t.Errorf("nil contactName = %q", got)
	}
	if got := s.counterpart(types.JID{User: "123", Server: types.DefaultUserServer}); !got.IsEmpty() {
		t.Errorf("nil counterpart = %v", got)
	}
	s.logState()
	s.close()
}

// tildeUnsaved implements WhatsApp's convention for distinguishing "a person I
// saved as X" from "a person calling themselves X".
func TestTildeUnsaved(t *testing.T) {
	cases := []struct {
		name  string
		saved bool
		want  string
	}{
		{"bulgayrian", true, "bulgayrian"},                   // user's own name for them
		{"Sarp Doruk Gerenli", false, "~Sarp Doruk Gerenli"}, // their own name
		{"~Already Marked", false, "~Already Marked"},        // no double tilde
		{"", false, ""}, // nothing to mark
		{"", true, ""},
	}
	for _, c := range cases {
		if got := tildeUnsaved(c.name, c.saved); got != c.want {
			t.Errorf("tildeUnsaved(%q, %v) = %q, want %q", c.name, c.saved, got, c.want)
		}
	}
}

// Message types the app can't render must produce a labelled placeholder rather
// than vanishing — a silently dropped message leaves an unexplained hole in the
// thread. Protocol housekeeping is the one thing that should stay invisible.
func TestUnsupportedLabel(t *testing.T) {
	cases := []struct {
		name string
		msg  *waE2E.Message
		want string
	}{
		{"contact with name", &waE2E.Message{
			ContactMessage: &waE2E.ContactMessage{DisplayName: proto.String("Alex")},
		}, "[contact: Alex]"},
		{"contact without name", &waE2E.Message{
			ContactMessage: &waE2E.ContactMessage{},
		}, "[contact card]"},
		{"poll", &waE2E.Message{
			PollCreationMessageV3: &waE2E.PollCreationMessage{Name: proto.String("Lunch?")},
		}, "[poll: Lunch?]"},
		{"view once", &waE2E.Message{
			ViewOnceMessage: &waE2E.FutureProofMessage{},
		}, "[view-once message]"},
		{"protocol housekeeping stays hidden", &waE2E.Message{
			ProtocolMessage: &waE2E.ProtocolMessage{},
		}, ""},
		{"unknown falls back", &waE2E.Message{}, "[unsupported message]"},
	}
	for _, c := range cases {
		if got := unsupportedLabel(c.msg); got != c.want {
			t.Errorf("%s: unsupportedLabel = %q, want %q", c.name, got, c.want)
		}
	}
}

// Status/"Updates" posts are a separate product surface, not a conversation.
// Broadcast LISTS are real chats and must not be caught by the same filter.
func TestIsStatusBroadcast(t *testing.T) {
	cases := []struct {
		name string
		jid  types.JID
		want bool
	}{
		{"status feed", types.StatusBroadcastJID, true},
		{"broadcast list", types.JID{User: "1234567890", Server: types.BroadcastServer}, false},
		{"ordinary dm", types.JID{User: "41791234567", Server: types.DefaultUserServer}, false},
		{"group", types.JID{User: "123-456", Server: types.GroupServer}, false},
		{"lid", types.JID{User: "104570072096833", Server: types.HiddenUserServer}, false},
	}
	for _, c := range cases {
		if got := isStatusBroadcast(c.jid); got != c.want {
			t.Errorf("%s: isStatusBroadcast(%s) = %v, want %v", c.name, c.jid, got, c.want)
		}
	}
}
