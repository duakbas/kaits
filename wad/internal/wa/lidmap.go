package wa

import (
	"database/sql"
	"log"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"

	_ "github.com/mattn/go-sqlite3"
)

// sessionStore reads whatsmeow's OWN session database directly, read-only,
// alongside the normal store API.
//
// Why bother, when whatsmeow already exposes GetPNForLID and GetContact?
// Because modern WhatsApp addresses people by LID (<id>@lid), and a person has
// a *different LID per group*. Two things go wrong if you only use the API:
//
//  1. GetPNForLID answers from whatsmeow's LID cache, which is populated as
//     mappings get "activated" by traffic. The whatsmeow_lid_map table itself
//     holds far more pairs than the client has activated in a given session.
//  2. Address-book names sync keyed by the *phone* JID, while the chat and its
//     messages arrive keyed by a LID JID. Looking a name up under one form
//     alone misses it — you must check the phone<->LID counterpart too.
//
// So we read whatsmeow_lid_map and whatsmeow_contacts at resolution time. This
// mirrors the approach in calmog/whatsapp-mcp, which fixed the same bug.
//
// Read-only and best-effort: if the file or tables aren't there (whatsmeow
// schema drift), openSessionStore fails and every caller falls back to the old
// GetPNForLID + learned-table path. Nothing breaks, names just get worse.
type sessionStore struct {
	db *sql.DB

	mu      sync.RWMutex
	lidToPN map[string]string    // bare LID user -> bare phone user
	pnToLID map[string]string    // bare phone user -> bare LID user
	names   map[string]nameEntry // non-AD JID string -> address-book name

	// Misses get their own maps, keyed the same way as the hit maps above but
	// holding the time we last drew a blank. Without these, a history sync of
	// tens of thousands of messages re-queries sqlite for every unmapped
	// sender, message after message. They're separate per direction because a
	// LID id and a phone id are different namespaces that can collide.
	lidMiss map[string]time.Time
	pnMiss  map[string]time.Time
}

// nameEntry caches one name lookup. Hits are cached forever (a saved contact
// name effectively never changes); misses carry a timestamp and are re-checked
// after missTTL, so a contact saved later still shows up without a restart.
type nameEntry struct {
	name string
	at   time.Time // when a miss was recorded; zero on a hit
}

// missTTL is how long a negative lookup is trusted. Long enough to keep a bulk
// history sync from hammering sqlite, short enough that a mapping or contact
// learned mid-session starts resolving without a daemon restart.
const missTTL = 5 * time.Minute

// openSessionStore opens whatsmeow's session db read-only and verifies the
// tables we depend on exist in this whatsmeow version.
func openSessionStore(path string) (*sessionStore, error) {
	// mode=ro so we can never block or corrupt the writer (whatsmeow itself,
	// in this same process). busy_timeout rides out its write transactions.
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	// One connection: we only ever do tiny indexed lookups, and a pool would
	// just multiply the locks we take against the writer.
	db.SetMaxOpenConns(1)

	// Confirm the schema before anyone relies on it — whatsmeow's tables move.
	for _, probe := range []string{
		`SELECT lid, pn FROM whatsmeow_lid_map LIMIT 1`,
		`SELECT their_jid, full_name, first_name, push_name, business_name FROM whatsmeow_contacts LIMIT 1`,
	} {
		rows, qerr := db.Query(probe)
		if qerr != nil {
			db.Close()
			return nil, qerr
		}
		rows.Close()
	}

	return &sessionStore{
		db:      db,
		lidToPN: make(map[string]string),
		pnToLID: make(map[string]string),
		names:   make(map[string]nameEntry),
		lidMiss: make(map[string]time.Time),
		pnMiss:  make(map[string]time.Time),
	}, nil
}

func (s *sessionStore) close() {
	if s != nil && s.db != nil {
		s.db.Close()
	}
}

// pnForLID maps a bare LID user id to its bare phone user id, "" if unknown.
// Positive results are cached (mappings are stable); misses are not, because
// new mappings land in the table as the session learns them.
//
// The nil check has to happen here rather than only inside lookup: passing
// s.lidToPN as an argument would dereference a nil receiver before the callee
// ever runs, and s is nil whenever the session db failed to open.
func (s *sessionStore) pnForLID(lidUser string) string {
	if s == nil || s.db == nil {
		return ""
	}
	return s.lookup(lidUser, s.lidToPN, s.pnToLID, s.lidMiss,
		`SELECT pn FROM whatsmeow_lid_map WHERE lid = ?`)
}

// lidForPN is the reverse of pnForLID.
func (s *sessionStore) lidForPN(pnUser string) string {
	if s == nil || s.db == nil {
		return ""
	}
	return s.lookup(pnUser, s.pnToLID, s.lidToPN, s.pnMiss,
		`SELECT lid FROM whatsmeow_lid_map WHERE pn = ?`)
}

// lookup is the shared cache-then-query path for both map directions. It fills
// the reverse map on a hit too, since the row it read covers both directions,
// and records misses so a bulk history sync doesn't re-query for every message
// from the same unmapped person.
func (s *sessionStore) lookup(key string, forward, reverse map[string]string, miss map[string]time.Time, query string) string {
	if s == nil || s.db == nil || key == "" {
		return ""
	}
	s.mu.RLock()
	got, ok := forward[key]
	missedAt, missed := miss[key]
	s.mu.RUnlock()
	if ok {
		return got
	}
	if missed && time.Since(missedAt) < missTTL {
		return ""
	}

	var val string
	if err := s.db.QueryRow(query, key).Scan(&val); err != nil || val == "" {
		// No row, or a locked/broken db — the caller falls back. Remember it
		// briefly so we don't ask again on the very next message.
		s.mu.Lock()
		miss[key] = time.Now()
		s.mu.Unlock()
		return ""
	}

	s.mu.Lock()
	forward[key] = val
	reverse[val] = key
	delete(miss, key)
	s.mu.Unlock()
	return val
}

// counterpart returns a JID's phone<->LID opposite number, or the zero JID if
// the map doesn't know it.
func (s *sessionStore) counterpart(jid types.JID) types.JID {
	if s == nil {
		return types.JID{}
	}
	switch jid.Server {
	case types.HiddenUserServer:
		if pn := s.pnForLID(jid.User); pn != "" {
			return types.JID{User: pn, Server: types.DefaultUserServer}
		}
	case types.DefaultUserServer:
		if lid := s.lidForPN(jid.User); lid != "" {
			return types.JID{User: lid, Server: types.HiddenUserServer}
		}
	}
	return types.JID{}
}

// contactName returns the best address-book name for a JID, or "".
//
// It checks the JID *and* its phone<->LID counterpart, because a name you saved
// on your phone syncs keyed by the phone JID while the message that needs the
// name arrives keyed by a LID. Preference order across whichever row has it:
// full_name (you saved it) > first_name > push_name (they set it) >
// business_name.
func (s *sessionStore) contactName(jid types.JID) string {
	if s == nil || s.db == nil || jid.User == "" {
		return ""
	}
	// The contacts table is keyed by non-AD JIDs; drop any device/agent part.
	key := jid.ToNonAD().String()

	s.mu.RLock()
	e, ok := s.names[key]
	s.mu.RUnlock()
	if ok && (e.name != "" || time.Since(e.at) < missTTL) {
		return e.name
	}

	jids := []any{key}
	if alt := s.counterpart(jid.ToNonAD()); !alt.IsEmpty() {
		jids = append(jids, alt.String())
	}

	q := `SELECT full_name, first_name, push_name, business_name
	      FROM whatsmeow_contacts WHERE their_jid IN (?` + repeatPlaceholders(len(jids)-1) + `)`
	rows, err := s.db.Query(q, jids...)
	if err != nil {
		return ""
	}
	defer rows.Close()

	// Collect every matching row, then apply the preference order *across* rows
	// — the phone-JID row may hold the saved name while the LID row holds only
	// a push name, and the saved name should win regardless of row order.
	var cols [4][]string
	for rows.Next() {
		var full, first, push, business sql.NullString
		if rows.Scan(&full, &first, &push, &business) != nil {
			continue
		}
		for i, v := range []sql.NullString{full, first, push, business} {
			if v.String != "" {
				cols[i] = append(cols[i], v.String)
			}
		}
	}

	name := ""
	for _, candidates := range cols {
		if len(candidates) > 0 {
			name = candidates[0]
			break
		}
	}

	s.mu.Lock()
	if name != "" {
		s.names[key] = nameEntry{name: name}
	} else {
		s.names[key] = nameEntry{at: time.Now()}
	}
	s.mu.Unlock()
	return name
}

// repeatPlaceholders returns n additional ",?" placeholders for an IN clause.
func repeatPlaceholders(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += ",?"
	}
	return out
}

// logSessionStoreState reports what the direct-table resolver found at startup,
// so a broken or empty lid map is obvious in the logs rather than showing up
// later as names silently rendering as raw numbers.
func (s *sessionStore) logState() {
	if s == nil || s.db == nil {
		return
	}
	var lidRows, contactRows int
	s.db.QueryRow(`SELECT COUNT(*) FROM whatsmeow_lid_map`).Scan(&lidRows)
	s.db.QueryRow(`SELECT COUNT(*) FROM whatsmeow_contacts`).Scan(&contactRows)
	log.Printf("wa: direct LID resolver ready (%d lid mappings, %d contacts)", lidRows, contactRows)
}
