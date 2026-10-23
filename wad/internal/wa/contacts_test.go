package wa

import "testing"

// Search lists people you have no chat with yet, so the address book has to
// come back with names: saved ones as they are, self-chosen ones marked "~",
// nameless entries and LID rows left out.
func TestAddressBookListsNamedPhoneContacts(t *testing.T) {
	path := newFakeSession(t, true)
	ins := `INSERT INTO whatsmeow_contacts (our_jid, their_jid, full_name, first_name, push_name) VALUES (?,?,?,?,?)`
	mustExec(t, path, ins, "me@s.whatsapp.net", "41791111111@s.whatsapp.net", "Alex Rivera", "", "alex")
	mustExec(t, path, ins, "me@s.whatsapp.net", "41792222222@s.whatsapp.net", "", "", "Sam")
	mustExec(t, path, ins, "me@s.whatsapp.net", "41793333333@s.whatsapp.net", "", "", "")
	mustExec(t, path, ins, "me@s.whatsapp.net", "158041961934877@lid", "Lid Person", "", "")

	s, err := openSessionStore(path)
	if err != nil {
		t.Fatalf("openSessionStore: %v", err)
	}
	defer s.close()

	got := s.addressBook()
	if len(got) != 2 {
		t.Fatalf("got %d contacts, want 2: %v", len(got), got)
	}
	if n := got["41791111111@s.whatsapp.net"].Name; n != "Alex Rivera" {
		t.Errorf("saved contact named %q, want the address-book name", n)
	}
	if n := got["41792222222@s.whatsapp.net"].Name; n != "~Sam" {
		t.Errorf("self-named contact shown as %q, want ~Sam", n)
	}
}
