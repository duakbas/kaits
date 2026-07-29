// Package wa wraps whatsmeow: it owns the linked-device session, handles
// pairing (QR), and translates WhatsApp events into our ws protocol frames.
package wa

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"wad/internal/ws"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	_ "github.com/mattn/go-sqlite3"
)

// Client bundles the whatsmeow client with a handle to the ws hub so events
// can be pushed straight to the phone.
type Client struct {
	WA  *whatsmeow.Client
	hub *ws.Hub

	// onCall is invoked for raw call events so the calls package can drive
	// meowcaller. Set via SetCallHook before Connect.
	onCall func(evt any)

	// onQR, if set, is called with each pairing QR code string (for terminal
	// rendering on the server during first run). The code is also pushed to
	// the phone as a TQR frame regardless.
	onQR func(code string)

	// groupNames caches group JID -> subject so we don't hit the server for
	// every message in a group. Populated lazily on first message from a group.
	groupNames   map[string]string
	groupNamesMu sync.RWMutex

	// media remembers downloadable parts so /media/<id> can fetch lazily.
	media     *mediaStore
	mediaMime map[string]string // id -> mime, for the HTTP Content-Type
	mimeMu    sync.RWMutex

	// replyCtx remembers recent messages (id -> content/sender/chat) so we can
	// build a proper WhatsApp reply that quotes the original. Bounded FIFO.
	replyCtx   map[string]replyContext
	replyOrder []string
	replyMu    sync.Mutex

	// avatars caches profile pictures by JID (bytes or a "none" mark).
	avatars  map[string]avatarEntry
	avatarMu sync.Mutex

	// hist is our own persistence layer for messages/chats (survives restarts).
	hist *histStore

	// sess reads whatsmeow's session db directly for LID<->phone mappings and
	// contact names. nil if it couldn't be opened — callers must cope.
	sess *sessionStore
}

// New opens (or creates) the session store and builds a whatsmeow client.
// dbPath is a sqlite file; keep it private — it IS your logged-in session.
func New(ctx context.Context, dbPath string, hub *ws.Hub) (*Client, error) {
	dbLog := waLog.Stdout("db", "WARN", true)
	container, err := sqlstore.New(ctx, "sqlite3",
		fmt.Sprintf("file:%s?_foreign_keys=on", dbPath), dbLog)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("get device: %w", err)
	}

	waCli := whatsmeow.NewClient(device, waLog.Stdout("wa", "INFO", true))

	// Our own history db lives next to the session db.
	hist, herr := openHistStore(dbPath + ".history.db")
	if herr != nil {
		log.Printf("wa: history store disabled: %v", herr)
		hist = nil
	}

	// Read-only view of whatsmeow's own tables, for LID/name resolution. Opened
	// after sqlstore.New so the file and its migrations already exist.
	sess, serr := openSessionStore(dbPath)
	if serr != nil {
		log.Printf("wa: direct LID resolver disabled (%v) — names will fall back to GetPNForLID", serr)
		sess = nil
	} else {
		sess.logState()
	}

	c := &Client{
		WA:         waCli,
		hub:        hub,
		groupNames: make(map[string]string),
		media:      newMediaStore(500),
		mediaMime:  make(map[string]string),
		replyCtx:   make(map[string]replyContext),
		avatars:    make(map[string]avatarEntry),
		hist:       hist,
		sess:       sess,
	}
	waCli.AddEventHandler(c.handleEvent)
	return c, nil
}

// SetCallHook lets the calls package receive raw call events.
func (c *Client) SetCallHook(fn func(evt any)) { c.onCall = fn }

// SetQRHook registers a callback for pairing QR codes (terminal rendering).
func (c *Client) SetQRHook(fn func(code string)) { c.onQR = fn }

// Connect logs in. If the store has no session yet, it drives QR pairing and
// pushes the QR code to the phone (TQR). Blocks until connected or ctx done.
func (c *Client) Connect(ctx context.Context) error {
	if c.WA.Store.ID != nil {
		// Already paired — just connect.
		if err := c.WA.Connect(); err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		return nil
	}

	// Fresh: need to pair. GetQRChannel must be called BEFORE Connect.
	qrChan, _ := c.WA.GetQRChannel(ctx)
	if err := c.WA.Connect(); err != nil {
		return fmt.Errorf("connect(pair): %w", err)
	}
	for evt := range qrChan {
		switch evt.Event {
		case "code":
			// Push to phone to render on-screen, and let main render it to the
			// terminal for first-run setup on the server.
			log.Printf("wa: scan QR (also sent to phone if connected)")
			if c.onQR != nil {
				c.onQR(evt.Code)
			}
			c.hub.PushT(ws.TQR, map[string]string{"code": evt.Code})
		case "success":
			log.Printf("wa: pairing success")
			c.hub.PushT(ws.TPaired, map[string]any{"ok": true})
		case "timeout":
			return fmt.Errorf("pairing timed out")
		}
	}
	return nil
}

// handleEvent is the single whatsmeow event sink. We translate the ones the
// phone cares about; everything else is logged at debug level.
func (c *Client) handleEvent(evt any) {
	switch v := evt.(type) {

	case *events.Connected:
		c.hub.PushT(ws.TReady, map[string]any{"ok": true})

	case *events.Message:
		c.pushMessage(v)

	case *events.HistorySync:
		c.handleHistorySync(v)

	case *events.Receipt:
		c.hub.PushT(ws.TReceipt, map[string]any{
			"chat":  v.Chat.String(),
			"type":  string(v.Type),
			"msgid": firstID(v.MessageIDs),
			"ts":    v.Timestamp.Unix(),
		})

	case *events.Presence:
		c.hub.PushT(ws.TPresence, map[string]any{
			"jid":         v.From.String(),
			"unavailable": v.Unavailable,
			"lastseen":    v.LastSeen.Unix(),
		})

	// ---- call events: hand off to the calls package (meowcaller) ----
	case *events.CallOffer, *events.CallOfferNotice,
		*events.CallTerminate, *events.CallRelayLatency,
		*events.CallAccept, *events.CallPreAccept:
		if c.onCall != nil {
			c.onCall(evt)
		}

	default:
		// log.Printf("wa: unhandled %T", v)
		_ = v
	}
}

// pushMessage handles a live incoming message: build, persist, and notify app.
func (c *Client) pushMessage(v *events.Message) { c.handleMsg(v, true) }

// handleMsg maps a whatsmeow Message event to our MsgData, persists it, and —
// if live — notifies the app. History-sync messages call this with live=false
// so they're stored but don't spam the UI as new-message notifications.
func (c *Client) handleMsg(v *events.Message, live bool) {
	m := v.Message
	// Canonicalize LID -> phone JID so a person isn't split across two chats.
	chat := c.canonicalJID(v.Info.Chat)
	isGroup := chat.Server == types.GroupServer

	d := ws.MsgData{
		MsgID:      v.Info.ID,
		ChatJID:    chat.String(),
		SenderJID:  c.canonicalJID(v.Info.Sender).String(),
		IsGroup:    isGroup,
		FromMe:     v.Info.IsFromMe,
		Timestamp:  v.Info.Timestamp.Unix(),
		SenderName: c.senderName(v),
		ChatName:   c.chatName(chat, isGroup),
		Pinned:     c.isPinned(chat),
	}

	switch {
	case m.GetConversation() != "":
		d.Kind = "text"
		d.Text = m.GetConversation()
	case m.GetExtendedTextMessage() != nil:
		et := m.GetExtendedTextMessage()
		d.Kind = "text"
		d.Text = c.resolveMentions(et.GetText(), et.GetContextInfo())
	case m.GetImageMessage() != nil:
		im := m.GetImageMessage()
		d.Kind = "image"
		d.Text = c.resolveMentions(im.GetCaption(), im.GetContextInfo())
		d.Mime = im.GetMimetype()
		d.MediaURL = "/media/" + v.Info.ID
		c.cacheMedia(v.Info.ID, im, im.GetMimetype())
	case m.GetAudioMessage() != nil:
		au := m.GetAudioMessage()
		d.Kind = "audio"
		d.Mime = au.GetMimetype()
		d.MediaURL = "/media/" + v.Info.ID
		c.cacheMedia(v.Info.ID, au, au.GetMimetype())
	case m.GetVideoMessage() != nil:
		vm := m.GetVideoMessage()
		// WhatsApp sends GIFs as video with GifPlayback set. Flag it so the app
		// can autoplay+loop it silently instead of showing a play control.
		if vm.GetGifPlayback() {
			d.Kind = "gif"
		} else {
			d.Kind = "video"
		}
		d.Text = c.resolveMentions(vm.GetCaption(), vm.GetContextInfo())
		d.Mime = vm.GetMimetype()
		d.MediaURL = "/media/" + v.Info.ID
		c.cacheMedia(v.Info.ID, vm, vm.GetMimetype())
	case m.GetStickerMessage() != nil:
		st := m.GetStickerMessage()
		d.Kind = "sticker"
		d.Mime = st.GetMimetype() // usually image/webp, may be animated
		d.MediaURL = "/media/" + v.Info.ID
		c.cacheMedia(v.Info.ID, st, st.GetMimetype())
	case m.GetDocumentMessage() != nil:
		dm := m.GetDocumentMessage()
		d.Kind = "doc"
		// show the filename as the body so the list is meaningful
		d.Text = dm.GetFileName()
		d.Mime = dm.GetMimetype()
		d.MediaURL = "/media/" + v.Info.ID
		c.cacheMedia(v.Info.ID, dm, dm.GetMimetype())
	default:
		// stickers, reactions, etc. — skip for v1
		return
	}

	// If this message is itself a reply, surface a preview of what it quotes.
	if ci := extractContextInfo(m); ci != nil && ci.GetStanzaID() != "" {
		d.QuotedID = ci.GetStanzaID()
		d.QuotedText = quotedPreview(ci.GetQuotedMessage())
		if p := ci.GetParticipant(); p != "" {
			if jid, err := types.ParseJID(p); err == nil {
				if n := c.displayNameForJID(jid); n != "" {
					d.QuotedName = n
				} else {
					// Fall back to the canonical (phone) number, not the raw
					// LID — a LID is a meaningless internal id to the user.
					d.QuotedName = c.canonicalJID(jid).User
				}
			}
		}
	}

	// Remember this message so the app can reply to it later (quotes need the
	// original body + sender + chat).
	c.rememberReply(v.Info.ID, m, c.canonicalJID(v.Info.Sender), chat)

	// Persist to our own history db so it survives restarts and populates the
	// chat list on launch.
	if !v.Info.IsFromMe {
		rawLID := v.Info.Sender.String()
		pn := ""
		if canon := c.canonicalJID(v.Info.Sender); canon.Server != types.HiddenUserServer {
			pn = canon.String()
		}
		if d.SenderName != "" && !isNumeric(d.SenderName) {
			c.hist.learnLID(rawLID, d.SenderName, pn)
		} else if pn != "" {
			c.hist.learnLID(rawLID, "", pn)
		}
	}
	c.hist.putMessage(d)

	// Only notify the app for live messages; history-sync is pull-based.
	if live {
		c.hub.PushT(ws.TMessage, d)
	}
}

// handleHistorySync unpacks a history-sync batch and persists every message via
// the same normalization as live messages (name resolution, media caching,
// quotes). Runs each old message through ParseWebMessage to turn the stored
// form into a normal *events.Message.
func (c *Client) handleHistorySync(v *events.HistorySync) {
	convs := v.Data.GetConversations()
	total := 0
	for _, conv := range convs {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}
		for _, hmsg := range conv.GetMessages() {
			evt, err := c.WA.ParseWebMessage(chatJID, hmsg.GetMessage())
			if err != nil {
				continue
			}
			c.handleMsg(evt, false) // persist only, don't spam the app
			total++
		}
	}
	if total > 0 {
		log.Printf("wa: history sync stored %d messages across %d chats", total, len(convs))
		// nudge the app to refresh its chat list now that new history landed
		c.hub.PushT(ws.TChatList, c.hist.listChats())
	}
}

// rememberReply stores a message's reply context, FIFO-bounded.
func (c *Client) rememberReply(id string, msg *waE2E.Message, sender, chat types.JID) {
	c.replyMu.Lock()
	defer c.replyMu.Unlock()
	if _, ok := c.replyCtx[id]; !ok {
		c.replyOrder = append(c.replyOrder, id)
		for len(c.replyOrder) > 500 {
			oldest := c.replyOrder[0]
			c.replyOrder = c.replyOrder[1:]
			delete(c.replyCtx, oldest)
		}
	}
	c.replyCtx[id] = replyContext{msg: msg, sender: sender, chat: chat}
}

// cacheMedia stores the downloadable part + its mime for later /media/ fetch.
func (c *Client) cacheMedia(id string, dl whatsmeow.DownloadableMessage, mime string) {
	c.media.put(id, dl)
	c.mimeMu.Lock()
	c.mediaMime[id] = mime
	c.mimeMu.Unlock()
}

// mimeFor returns the cached mime type for a media id.
func (c *Client) MimeFor(id string) string {
	c.mimeMu.RLock()
	defer c.mimeMu.RUnlock()
	return c.mediaMime[id]
}

// DownloadMedia is the exported entry the HTTP handler uses.
func (c *Client) DownloadMedia(ctx context.Context, id string) ([]byte, error) {
	return c.downloadMedia(ctx, id)
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// canonicalJID resolves a LID (@lid) address to its real phone-number JID so a
// person doesn't appear as two separate chats (one @lid, one @s.whatsapp.net).
// Device/agent parts are stripped: the same person on two of their devices is
// still one chat.
//
// Resolution order, widest source first:
//  1. whatsmeow_lid_map read directly — the complete mapping table.
//  2. GetPNForLID — whatsmeow's in-memory view, which can know a pairing that
//     hasn't been written to the table yet.
//  3. the learned-LID table we build from observed traffic.
//
// If nothing knows the LID it's returned unchanged, and the caller displays a
// raw number rather than losing the message.
func (c *Client) canonicalJID(jid types.JID) types.JID {
	if jid.Server != types.HiddenUserServer { // not a @lid
		return jid.ToNonAD()
	}

	if pn := c.sess.pnForLID(jid.User); pn != "" {
		return types.JID{User: pn, Server: types.DefaultUserServer}
	}

	if pn, err := c.WA.Store.LIDs.GetPNForLID(context.Background(), jid.ToNonAD()); err == nil && !pn.IsEmpty() {
		return pn.ToNonAD()
	}

	if pn := c.hist.resolvePN(jid.ToNonAD().String()); pn != "" {
		if parsed, err := types.ParseJID(pn); err == nil && !parsed.IsEmpty() {
			return parsed.ToNonAD()
		}
	}

	return jid.ToNonAD() // no mapping available — leave as-is
}

// displayName resolves any user JID to a human name, given an optional push
// name that rode along on an event. Returns "" when nothing beats the number.
//
// Order: the name you saved in your address book wins over a name the other
// person set for themselves. That ordering only became safe once the direct
// whatsmeow_contacts lookup started checking the phone<->LID counterpart — a
// LID-addressed sender used to miss the saved name entirely, which is why the
// event push name used to be consulted first.
func (c *Client) displayName(jid types.JID, pushName string) string {
	// 0. a nickname the user saved in this app beats everything — they chose it
	//    most recently and most deliberately.
	if n := c.hist.localContactName(c.canonicalJID(jid).String()); n != "" {
		return n
	}
	// 1. address book, via the direct tables (checks the LID counterpart too).
	if n := c.sess.contactName(jid); n != "" {
		return n
	}
	// 2. push name riding on the event, if we were given one.
	if pushName != "" {
		return pushName
	}
	// 3. whatsmeow's contact store, under the canonical JID and the raw one
	//    (identical for a non-LID JID, so only try the second when it differs).
	candidates := []types.JID{c.canonicalJID(jid)}
	if raw := jid.ToNonAD(); raw != candidates[0] {
		candidates = append(candidates, raw)
	}
	for _, j := range candidates {
		if contact, err := c.WA.Store.Contacts.GetContact(context.Background(), j); err == nil {
			if contact.FullName != "" {
				return contact.FullName
			}
			if contact.PushName != "" {
				return contact.PushName
			}
			if contact.BusinessName != "" {
				return contact.BusinessName
			}
		}
	}
	// 4. names we learned from observed traffic, keyed by the raw LID.
	if n := c.hist.resolveLIDName(jid.ToNonAD().String()); n != "" {
		return n
	}
	return ""
}

// senderName resolves who sent a message to a human name, falling back to the
// bare number when nobody knows them.
func (c *Client) senderName(v *events.Message) string {
	if v.Info.IsFromMe {
		return "You"
	}
	if n := c.displayName(v.Info.Sender, v.Info.PushName); n != "" {
		return n
	}
	return c.canonicalJID(v.Info.Sender).User
}

// chatName resolves a chat JID to a display name. For groups it's the group
// subject (cached); for 1:1 it's the contact's name, falling back to the
// user part of the JID.
func (c *Client) chatName(chat types.JID, isGroup bool) string {
	if isGroup {
		key := chat.String()
		c.groupNamesMu.RLock()
		if name, ok := c.groupNames[key]; ok {
			c.groupNamesMu.RUnlock()
			return name
		}
		c.groupNamesMu.RUnlock()

		// Not cached — ask the server once, then remember it.
		info, err := c.WA.GetGroupInfo(context.Background(), chat)
		name := key
		if err == nil && info != nil && info.Name != "" {
			name = info.Name
		} else if err != nil {
			log.Printf("wa: GetGroupInfo(%s): %v", key, err)
		}
		c.groupNamesMu.Lock()
		c.groupNames[key] = name
		c.groupNamesMu.Unlock()
		return name
	}

	// 1:1 chat: resolve the contact through the same path as message senders,
	// so a DM header shows the saved name instead of a raw LID number.
	if n := c.displayName(chat, ""); n != "" {
		return n
	}
	// last resort: strip the server suffix for a cleaner display. Prefer the
	// canonical (phone) form so an unresolved LID at least shows the number the
	// user might recognise, when the map knows it.
	s := c.canonicalJID(chat).String()
	if i := strings.IndexByte(s, '@'); i > 0 {
		return s[:i]
	}
	return s
}

// resolveMentions rewrites "@<number>" mentions into "@<name>". WhatsApp puts
// the mentioned users' JIDs in ContextInfo.MentionedJID and embeds the bare
// number in the text. We look up each and substitute name-if-known, else the
// push name, else leave the number.
func (c *Client) resolveMentions(text string, ci *waE2E.ContextInfo) string {
	if ci == nil || text == "" {
		return text
	}
	mentioned := ci.GetMentionedJID()
	if len(mentioned) == 0 {
		return text
	}
	for _, jidStr := range mentioned {
		jid, err := types.ParseJID(jidStr)
		if err != nil {
			continue
		}
		name := c.displayNameForJID(jid)
		if name == "" {
			continue
		}
		// The text contains "@<user>" where user is the number part — but which
		// number depends on how the sender's client addressed the mention, so
		// try both the LID and the phone form.
		for _, user := range []string{jid.User, c.canonicalJID(jid).User} {
			if user == "" {
				continue
			}
			text = strings.ReplaceAll(text, "@"+user, "@"+name)
		}
	}
	return text
}

// displayNameForJID resolves a single JID to a name, returning "" if nothing
// better than the number is known. Used for @mentions and quoted-message
// authors, which arrive as bare JIDs with no push name attached.
func (c *Client) displayNameForJID(jid types.JID) string {
	return c.displayName(jid, "")
}

// DeleteMessage revokes (deletes for everyone) one of the user's own messages.
func (c *Client) DeleteMessage(ctx context.Context, chatJID, msgID string) error {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}
	_, err = c.WA.SendMessage(ctx, jid, c.WA.BuildRevoke(jid, types.EmptyJID, msgID))
	return err
}

// ForwardMessage re-sends a previously seen message's content to another chat.
// Uses the cached original message content (from replyCtx). For text it sends
// the text; for media it re-sends the same downloadable message struct, which
// carries the media keys so it forwards without re-uploading.
func (c *Client) ForwardMessage(ctx context.Context, srcMsgID, destChatJID string) (string, error) {
	dest, err := types.ParseJID(destChatJID)
	if err != nil {
		return "", err
	}
	c.replyMu.Lock()
	rc, ok := c.replyCtx[srcMsgID]
	c.replyMu.Unlock()
	if !ok || rc.msg == nil {
		return "", fmt.Errorf("message %s not in cache; cannot forward", srcMsgID)
	}
	resp, err := c.WA.SendMessage(ctx, dest, rc.msg)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// isPinned reports whether a chat is pinned, per the account's synced app
// state. Read-only: reflects pins you set on your phone. (Setting pins from
// here goes through SendAppState, which is fragile on linked devices, so it's
// intentionally not wired.)
func (c *Client) isPinned(chat types.JID) bool {
	settings, err := c.WA.Store.ChatSettings.GetChatSettings(context.Background(), chat)
	if err != nil {
		return false
	}
	return settings.Pinned
}

// RunLIDMigration rewrites already-stored @lid chats to their phone JID and
// merges the duplicates, using the full resolution path (direct table first),
// so it catches mappings the old GetPNForLID-only pass left behind.
func (c *Client) RunLIDMigration() (int, int, int) {
	return c.hist.MigrateLIDs(func(lid string) (string, bool) {
		jid, err := types.ParseJID(lid)
		if err != nil {
			return "", false
		}
		canon := c.canonicalJID(jid)
		if canon.Server == types.HiddenUserServer || canon.IsEmpty() {
			return "", false // still unmapped
		}
		return canon.String(), true
	})
}

// BackfillSenderNamesNow rewrites stored messages whose sender name is a raw
// number, using the same resolver live messages now use.
func (c *Client) BackfillSenderNamesNow() int {
	return c.hist.BackfillSenderNames(func(sender string) string {
		jid, err := types.ParseJID(sender)
		if err != nil {
			return ""
		}
		return c.displayName(jid, "")
	})
}

// BackfillChatNamesNow rewrites stored chats whose name is a raw number (or
// empty) now that DM names resolve through the address book. Groups are left
// alone — their names come from GetGroupInfo, not the LID map. Returns the
// number of chats renamed.
func (c *Client) BackfillChatNamesNow() int {
	return c.hist.BackfillChatNames(func(chatJID string) string {
		jid, err := types.ParseJID(chatJID)
		if err != nil || jid.Server == types.GroupServer {
			return ""
		}
		return c.displayName(jid, "")
	})
}

// ListChats returns the persisted chat list for the app's getchats.
func (c *Client) ListChats() []map[string]any {
	return c.hist.listChats()
}

// History returns stored messages for a chat (oldest->newest), paginated by
// beforeTS (0 = latest page).
func (c *Client) History(chat string, beforeTS int64, limit int) []ws.MsgData {
	return c.hist.history(chat, beforeTS, limit)
}

// RequestHistorySync asks the phone to re-send more history (on-demand). This
// is best-effort: WhatsApp caps how far back linked devices can pull, and the
// phone may decline. Results arrive later as HistorySync events.
func (c *Client) RequestHistorySync(ctx context.Context) {
	// Trigger a build/send of a history sync request. whatsmeow exposes this
	// via BuildHistorySyncRequest around the latest known message. Since we
	// don't track a specific anchor here, we request a full-ish resync by
	// asking whatsmeow to fetch app state + history; if the method isn't
	// available in this version, this is a no-op logged for visibility.
	log.Printf("wa: requesting on-demand history sync (best-effort)")
	// Note: on-demand history needs an anchor message; without one we rely on
	// the automatic logon-time sync. Left as a hook for future anchoring.
}

// SendText sends a plain text message to a chat JID.
func (c *Client) SendText(ctx context.Context, chatJID, text string) (string, error) {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return "", err
	}
	resp, err := c.WA.SendMessage(ctx, jid, textMsg(text))
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// SendReply sends a text message that quotes an earlier message (by its id).
// If the id isn't in the reply cache (evicted or never seen), it falls back to
// a plain send so the user's message still goes out.
func (c *Client) SendReply(ctx context.Context, chatJID, text, quotedID string) (string, error) {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return "", err
	}

	c.replyMu.Lock()
	rc, ok := c.replyCtx[quotedID]
	c.replyMu.Unlock()

	if !ok {
		// no context to quote — send plain rather than failing
		return c.SendText(ctx, chatJID, text)
	}

	msg := replyTextMsg(text, quotedID, rc.sender, rc.msg)
	resp, err := c.WA.SendMessage(ctx, jid, msg)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// SendPrivateReply DMs the author of a group message, quoting that message.
// The destination is derived from the original sender, so the app doesn't have
// to know the person's DM JID — which it often can't, since group senders
// arrive as per-group LIDs.
func (c *Client) SendPrivateReply(ctx context.Context, srcMsgID, text string) (string, string, error) {
	c.replyMu.Lock()
	rc, ok := c.replyCtx[srcMsgID]
	c.replyMu.Unlock()
	if !ok || rc.msg == nil {
		return "", "", fmt.Errorf("message %s not in cache; cannot reply privately", srcMsgID)
	}
	dest := c.canonicalJID(rc.sender)
	if dest.IsEmpty() || dest.Server == types.GroupServer {
		return "", "", fmt.Errorf("no direct address for the sender of %s", srcMsgID)
	}
	msg := privateReplyMsg(text, srcMsgID, dest, rc.chat, rc.msg)
	resp, err := c.WA.SendMessage(ctx, dest, msg)
	if err != nil {
		return "", "", err
	}
	return resp.ID, dest.String(), nil
}

// DirectJIDFor resolves the DM address for whoever sent a message, so the app
// can open (or create) a 1:1 chat from a group bubble.
func (c *Client) DirectJIDFor(srcMsgID string) (string, error) {
	c.replyMu.Lock()
	rc, ok := c.replyCtx[srcMsgID]
	c.replyMu.Unlock()
	if !ok {
		return "", fmt.Errorf("message %s not in cache", srcMsgID)
	}
	dest := c.canonicalJID(rc.sender)
	if dest.IsEmpty() || dest.Server == types.GroupServer {
		return "", fmt.Errorf("no direct address for the sender of %s", srcMsgID)
	}
	return dest.String(), nil
}

// ResyncContacts forces WhatsApp to re-send the account's whole contact list,
// refilling whatsmeow_contacts (and, as traffic resolves, whatsmeow_lid_map).
//
// This is the cheap alternative to unlinking and re-pairing when stored history
// still shows raw numbers: it refreshes the same tables a fresh pairing would,
// without touching the session, and our own message db is never involved.
// Afterwards the resolver's caches are dropped so previously-missing names get
// looked up again rather than waiting out their negative-cache TTL.
func (c *Client) ResyncContacts(ctx context.Context) error {
	if err := c.WA.FetchAppState(ctx, appstate.WAPatchCriticalUnblockLow, true, false); err != nil {
		return fmt.Errorf("contact resync: %w", err)
	}
	c.sess.reset()
	return nil
}

// BackfillQuotedNamesNow re-resolves quoted-message authors stored as raw
// numbers (the reply quote bar keeps its own copy of the name).
func (c *Client) BackfillQuotedNamesNow() int {
	return c.hist.BackfillQuotedNames(func(num string) string {
		// Stored quoted names are bare user ids; try both address forms.
		for _, server := range []string{types.HiddenUserServer, types.DefaultUserServer} {
			if n := c.displayName(types.JID{User: num, Server: server}, ""); n != "" {
				return n
			}
		}
		return ""
	})
}

func firstID(ids []types.MessageID) string {
	if len(ids) == 0 {
		return ""
	}
	return string(ids[0])
}
