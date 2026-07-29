package wa

import (
	"context"
	"strings"
	"time"

	"wad/internal/ws"

	"go.mau.fi/whatsmeow/types"
)

// ProfileFor gathers everything the app's contact/group info screen shows.
//
// The name chain deliberately matches what the rest of the daemon displays, so
// the info screen never disagrees with the chat list: saved nickname, then
// address book, then the name the person set on WhatsApp themselves, then the
// bare number.
func (c *Client) ProfileFor(ctx context.Context, jidStr string) ws.ProfileData {
	p := ws.ProfileData{JID: jidStr}
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		p.Name = jidStr
		return p
	}
	jid = jid.ToNonAD()
	canon := c.canonicalJID(jid)

	p.IsGroup = jid.Server == types.GroupServer
	p.AvatarURL = "/avatar/" + jidStr
	p.Pinned, p.Muted, p.Archived = c.ChatFlags(jidStr)

	if p.IsGroup {
		c.fillGroupProfile(ctx, jid, &p)
		return p
	}

	// --- 1:1 contact ---
	p.SavedName = c.hist.localContactName(canon.String())
	if p.SavedName == "" && canon != jid {
		p.SavedName = c.hist.localContactName(jid.String())
	}

	info := c.contactInfoFor(jid)
	p.PushName = info.PushName
	if info.BusinessName != "" {
		p.PushName = info.BusinessName
		p.IsBusiness = true
	}
	// "In your address book" means a name YOU set — either a nickname saved
	// here or a full/first name synced from the phone's contacts. A push name
	// is the contact naming themselves and doesn't count.
	p.InAddressBook = info.FullName != "" || info.FirstName != ""
	p.Saved = p.SavedName != "" || p.InAddressBook

	p.Name = c.displayName(jid, "")
	if p.Name == "" {
		p.Name = canon.User
	}

	// A phone number is only knowable when the JID is (or maps to) a real phone
	// address. A LID with no mapping stays unknown, and WhatsApp may offer only
	// a redacted form for people you've never saved.
	if canon.Server == types.DefaultUserServer && canon.User != "" {
		p.Phone = formatPhone(canon.User)
	}
	p.RedactedPhone = info.RedactedPhone

	if st := c.userStatus(ctx, canon, jid); st != "" {
		p.Status = st
	}
	return p
}

// fillGroupProfile adds subject, description and membership for a group chat.
func (c *Client) fillGroupProfile(ctx context.Context, jid types.JID, p *ws.ProfileData) {
	p.Name = c.chatName(jid, true)
	info, err := c.WA.GetGroupInfo(ctx, jid)
	if err != nil || info == nil {
		return
	}
	if info.Name != "" {
		p.Name = info.Name
	}
	p.Status = info.Topic
	p.MemberCount = len(info.Participants)
	if !info.GroupCreated.IsZero() {
		p.CreatedAt = info.GroupCreated.Unix()
	}
	// A handful of member names is enough for a 240px screen; the app shows a
	// "+N more" tail rather than a scrolling roster.
	for i, part := range info.Participants {
		if i >= 12 {
			break
		}
		name := c.displayName(part.JID, "")
		if name == "" {
			name = c.canonicalJID(part.JID).User
		}
		p.Members = append(p.Members, ws.MemberData{
			JID:     part.JID.ToNonAD().String(),
			Name:    name,
			IsAdmin: part.IsAdmin || part.IsSuperAdmin,
		})
	}
}

// userStatus fetches the "about" text, trying the canonical JID then the raw
// one. Best-effort: WhatsApp rate-limits this and hides it by privacy setting.
func (c *Client) userStatus(ctx context.Context, canon, raw types.JID) string {
	tried := []types.JID{canon}
	if raw != canon {
		tried = append(tried, raw)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, j := range tried {
		resp, err := c.WA.GetUserInfo(ctx, []types.JID{j})
		if err != nil {
			continue
		}
		if info, ok := resp[j]; ok && info.Status != "" {
			return info.Status
		}
	}
	return ""
}

// contactInfoFor merges whatsmeow's contact rows for a JID and its phone<->LID
// counterpart, since the address book and the live traffic key them differently.
func (c *Client) contactInfoFor(jid types.JID) types.ContactInfo {
	var out types.ContactInfo
	seen := map[types.JID]bool{}
	for _, j := range []types.JID{jid.ToNonAD(), c.canonicalJID(jid), c.sess.counterpart(jid.ToNonAD())} {
		if j.IsEmpty() || seen[j] {
			continue
		}
		seen[j] = true
		info, err := c.WA.Store.Contacts.GetContact(context.Background(), j)
		if err != nil {
			continue
		}
		if out.FullName == "" {
			out.FullName = info.FullName
		}
		if out.FirstName == "" {
			out.FirstName = info.FirstName
		}
		if out.PushName == "" {
			out.PushName = info.PushName
		}
		if out.BusinessName == "" {
			out.BusinessName = info.BusinessName
		}
		if out.RedactedPhone == "" {
			out.RedactedPhone = info.RedactedPhone
		}
	}
	return out
}

// SaveContact stores a nickname for a JID and applies it to already-stored
// chats and messages.
//
// This is local to the daemon. WhatsApp has no contact-write API — the address
// book syncs one way, from a phone into the account — so nothing here reaches
// the user's real contacts. On KaiOS the app additionally writes the number to
// the device address book via mozContacts / a webcontacts activity; that half
// lives in the app because only the phone can do it.
func (c *Client) SaveContact(chatJID, name string) ws.ProfileData {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return ws.ProfileData{JID: chatJID}
	}
	key := c.canonicalJID(jid).String()
	name = strings.TrimSpace(name)
	c.hist.setLocalContact(key, name, time.Now().Unix())
	if name != "" {
		c.hist.renameChatAndMessages(key, name)
		if key != jid.ToNonAD().String() {
			c.hist.renameChatAndMessages(jid.ToNonAD().String(), name)
		}
	}
	return c.ProfileFor(context.Background(), chatJID)
}

// formatPhone renders a bare user id as an E.164-ish number for display.
func formatPhone(user string) string {
	if user == "" || !isNumeric(user) {
		return ""
	}
	return "+" + user
}
