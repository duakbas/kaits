package wa

import (
	"database/sql"
	"sort"
	"strings"

	"go.mau.fi/whatsmeow/types"
)

// Contact is one person the app can start a chat with.
type Contact struct {
	JID  string `json:"jid"`
	Name string `json:"name"`
}

// addressBook lists everyone in whatsmeow's contact table under their phone
// address, with the best name the table has for them. Address-book names
// (full/first) win; a push name is the fallback for people who wrote first.
func (s *sessionStore) addressBook() map[string]Contact {
	out := map[string]Contact{}
	if s == nil || s.db == nil {
		return out
	}
	rows, err := s.db.Query(`SELECT their_jid, full_name, first_name, push_name, business_name
		FROM whatsmeow_contacts WHERE their_jid LIKE '%@s.whatsapp.net'`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var jid string
		var full, first, push, biz sql.NullString
		if rows.Scan(&jid, &full, &first, &push, &biz) != nil || jid == "" {
			continue
		}
		name, saved := full.String, true
		if name == "" {
			name = first.String
		}
		if name == "" {
			name, saved = push.String, false
		}
		if name == "" {
			name, saved = biz.String, false
		}
		if name == "" {
			continue // a bare number is no help when searching by name
		}
		out[jid] = Contact{JID: jid, Name: tildeUnsaved(name, saved)}
	}
	return out
}

// Contacts is everyone the account knows, for starting a chat that doesn't
// exist yet: the address book WhatsApp synced, plus nicknames saved in this
// app (which win, as they do everywhere else). Sorted by name.
func (c *Client) Contacts() []Contact {
	all := c.sess.addressBook()
	for jid, name := range c.hist.localContacts() {
		if strings.HasSuffix(jid, "@"+types.DefaultUserServer) {
			all[jid] = Contact{JID: jid, Name: name}
		}
	}
	if c.WA != nil && c.WA.Store != nil && c.WA.Store.ID != nil {
		delete(all, c.WA.Store.ID.ToNonAD().String())
	}
	out := make([]Contact, 0, len(all))
	for _, ct := range all {
		out = append(out, ct)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(strings.TrimPrefix(out[i].Name, "~")) <
			strings.ToLower(strings.TrimPrefix(out[j].Name, "~"))
	})
	return out
}

// localContacts returns every nickname saved in this app, jid -> name.
func (h *histStore) localContacts() map[string]string {
	out := map[string]string{}
	if h == nil || h.db == nil {
		return out
	}
	rows, err := h.db.Query(`SELECT jid, name FROM local_contacts WHERE name<>''`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var j, n string
		if rows.Scan(&j, &n) == nil {
			out[j] = n
		}
	}
	return out
}
