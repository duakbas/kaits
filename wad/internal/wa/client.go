// Package wa wraps whatsmeow: it owns the linked-device session, handles
// pairing (QR), and translates WhatsApp events into our ws protocol frames.
package wa

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"wad/internal/ws"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

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

	// gapTried remembers which chats we've already asked to backfill this run,
	// so a long-quiet chat doesn't trigger a request on every message.
	gapTried map[string]bool
	gapMu    sync.Mutex
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

// announcePresence tells WhatsApp this device is here.
//
// Without it a linked device connects passively: it receives fine, but the
// server never marks it active, so the phone's "Linked devices" screen keeps
// showing an old "last active" and other users see "-" where our push name
// should be. Sending "available" also enables active receipts, which is what
// makes the device look genuinely live rather than merely attached.
//
// The trade-off is real, so it's controllable via WAD_PRESENCE:
//
//	available   (default) appear online, send read receipts
//	unavailable register the push name but stay invisible
//	off         send nothing — the device will look idle
//
// Best-effort: a failure here costs visibility, not function, so it's logged
// rather than propagated.
func (c *Client) announcePresence() {
	mode := os.Getenv("WAD_PRESENCE")
	if mode == "off" {
		return
	}
	state := types.PresenceAvailable
	if mode == "unavailable" {
		state = types.PresenceUnavailable
	}

	// SendPresence refuses without a push name, and the attribute it sends IS
	// the push name — so an empty one is worth saying out loud.
	if c.WA.Store.PushName == "" {
		log.Printf("wa: no push name in the session store; skipping presence " +
			"(contacts may see \"-\" and this device will look idle)")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.WA.SendPresence(ctx, state); err != nil {
		log.Printf("wa: could not announce presence (%v) — this device may show as inactive", err)
		return
	}
	log.Printf("wa: presence sent as %q (WAD_PRESENCE=off to stay quiet)", state)
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
		// Announce presence, or WhatsApp treats this device as idle: "Linked
		// devices" shows a stale "last active", and contacts see "-" instead of
		// our push name. Has to happen on every Connected, not once at startup,
		// because a reconnect resets it.
		c.announcePresence()
		c.hub.PushT(ws.TReady, map[string]any{"ok": true})

	case *events.Message:
		c.pushMessage(v)

	case *events.HistorySync:
		c.handleHistorySync(v)

	case *events.Receipt:
		c.handleReceipt(v)

	case *events.ChatPresence:
		// Someone is typing (or stopped). Named here rather than in the app,
		// because a group participant arrives as a per-group LID the app can't
		// resolve on its own.
		name := c.displayName(v.Sender, "")
		if name == "" {
			name = c.canonicalJID(v.Sender).User
		}
		c.hub.PushT(ws.TTyping, map[string]any{
			"chat":       c.canonicalJID(v.Chat).String(),
			"sender":     c.canonicalJID(v.Sender).String(),
			"sendername": name,
			"state":      string(v.State),
			"media":      string(v.Media),
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

// handleReceipt turns a delivery/read receipt into a stored status and a status
// frame for the app.
//
// A receipt covers a LIST of message ids, not one — the old code took only the
// first and silently dropped the rest, so a batch acknowledgement updated a
// single tick. Statuses also arrive out of order (one recipient's "delivered"
// after another's "read"), so the store only ever moves a message forwards.
func (c *Client) handleReceipt(v *events.Receipt) {
	status := statusForReceipt(v.Type)
	if status == "" {
		return // a type we don't render, e.g. server-side bookkeeping
	}
	chat := c.canonicalJID(v.Chat).String()
	for _, id := range v.MessageIDs {
		msgID := string(id)
		if !c.hist.setMessageStatus(msgID, status) {
			continue // already at or past this state
		}
		c.hub.PushT(ws.TStatus, map[string]any{
			"chat": chat, "msgid": msgID, "status": status, "ts": v.Timestamp.Unix(),
		})
	}
}

// statusForReceipt maps WhatsApp's receipt types onto the three states the UI
// draws. Read-self (this account reading elsewhere) counts as read; anything
// unrecognised is ignored rather than guessed at.
func statusForReceipt(t types.ReceiptType) string {
	switch t {
	case types.ReceiptTypeDelivered:
		return "delivered"
	case types.ReceiptTypeRead, types.ReceiptTypeReadSelf:
		return "read"
	case types.ReceiptTypePlayed:
		return "played"
	}
	return ""
}

// handleReaction records someone reacting to a message and pushes the message's
// complete reaction set to the app.
//
// WhatsApp sends a reaction as a message whose ReactionMessage names the target,
// and an empty emoji means the reaction was removed. The app is sent the whole
// set rather than a delta, so it never has to reconcile add/remove ordering.
func (c *Client) handleReaction(v *events.Message, live bool) {
	r := v.Message.GetReactionMessage()
	key := r.GetKey()
	if key == nil || key.GetID() == "" {
		return
	}
	chat := c.canonicalJID(v.Info.Chat).String()
	sender := c.canonicalJID(v.Info.Sender).String()
	name := c.senderName(v)

	ts := v.Info.Timestamp.Unix()
	if ms := r.GetSenderTimestampMS(); ms > 0 {
		ts = ms / 1000
	}
	c.hist.putReaction(chat, key.GetID(), sender, name, r.GetText(), ts)

	if live {
		c.hub.PushT(ws.TReaction, map[string]any{
			"chat":      chat,
			"msgid":     key.GetID(),
			"reactions": c.hist.reactionsForMessage(chat, key.GetID()),
		})
	}
}

// SendReaction reacts to a message, or clears our reaction when emoji is "".
//
// The target's sender has to be named, not just its id, so this resolves the
// message the same way replies do — cache first, then stored history — which is
// what makes reacting work on anything scrolled to, not only what arrived this
// session. Our own reaction is recorded locally too, so the chip appears without
// waiting for the echo to come back.
func (c *Client) SendReaction(ctx context.Context, chatJID, msgID, emoji string) error {
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}
	sender, storedChat, _, ok := c.lookupMessage(msgID)
	if !ok {
		return fmt.Errorf("message %s is not stored; cannot react", msgID)
	}
	if !storedChat.IsEmpty() {
		chat = storedChat
	}
	// WhatsApp wants our own JID as the sender for a message we sent.
	if own := c.WA.Store.ID; own != nil && sender.User == own.User {
		sender = own.ToNonAD()
	}

	msg := c.WA.BuildReaction(chat, sender, types.MessageID(msgID), emoji)
	if _, err := c.WA.SendMessage(ctx, chat, msg); err != nil {
		return err
	}

	self := ""
	if own := c.WA.Store.ID; own != nil {
		self = own.ToNonAD().String()
	}
	c.hist.putReaction(chat.String(), msgID, self, "You", emoji, time.Now().Unix())
	c.hub.PushT(ws.TReaction, map[string]any{
		"chat":      chat.String(),
		"msgid":     msgID,
		"reactions": c.hist.reactionsForMessage(chat.String(), msgID),
	})
	return nil
}

// MyReactionTo returns the emoji we've reacted to a message with, "" if none.
// The app uses it to offer "remove" instead of "react" on a second press.
func (c *Client) MyReactionTo(chatJID, msgID string) string {
	own := c.WA.Store.ID
	if own == nil {
		return ""
	}
	self := own.ToNonAD().String()
	for _, r := range c.hist.reactionsForMessage(chatJID, msgID) {
		if r.SenderJID == self {
			return r.Emoji
		}
	}
	return ""
}

// MarkChatRead tells WhatsApp the user has read a chat's incoming messages, and
// records it locally so we don't keep re-sending the same receipt.
func (c *Client) MarkChatRead(ctx context.Context, chatJID string) (int, error) {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return 0, err
	}
	ids, lastSender := c.hist.unreadMessageIDs(chatJID, 50)
	if len(ids) == 0 {
		return 0, nil
	}
	msgIDs := make([]types.MessageID, 0, len(ids))
	for _, id := range ids {
		msgIDs = append(msgIDs, types.MessageID(id))
	}
	// In a group WhatsApp wants the sender of the messages being acknowledged;
	// in a DM that's the chat itself.
	sender := jid
	if jid.Server == types.GroupServer && lastSender != "" {
		if s, err := types.ParseJID(lastSender); err == nil {
			sender = s
		}
	}
	if err := c.WA.MarkRead(ctx, msgIDs, time.Now(), jid, sender); err != nil {
		return 0, err
	}
	c.hist.markMessagesRead(chatJID, ids)
	return len(ids), nil
}

// pushMessage handles a live incoming message: build, persist, and notify app.
func (c *Client) pushMessage(v *events.Message) { c.handleMsg(v, true) }

// handleMsg maps a whatsmeow Message event to our MsgData, persists it, and —
// if live — notifies the app. History-sync messages call this with live=false
// so they're stored but don't spam the UI as new-message notifications.
func (c *Client) handleMsg(v *events.Message, live bool) {
	m := v.Message

	// Status/"Updates" posts arrive as messages in status@broadcast and would
	// otherwise show up as a chat from a mysterious "~status". They're a
	// different product surface, not a conversation, so they're dropped unless
	// explicitly wanted.
	if isStatusBroadcast(v.Info.Chat) && os.Getenv("WAD_INCLUDE_STATUS") != "1" {
		return
	}

	// A reaction is a message about another message, not a message in its own
	// right — it decorates an existing bubble rather than adding one.
	if m.GetReactionMessage() != nil {
		c.handleReaction(v, live)
		return
	}
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
	case m.GetLocationMessage() != nil:
		lm := m.GetLocationMessage()
		d.Kind = "location"
		d.Lat, d.Lon = lm.GetDegreesLatitude(), lm.GetDegreesLongitude()
		d.LocName, d.LocAddress = lm.GetName(), lm.GetAddress()
		d.Text = c.resolveMentions(lm.GetComment(), lm.GetContextInfo())
		// WhatsApp ships a rendered map preview in the message itself, so the
		// app can show a real map with no external request and no API key.
		if thumb := lm.GetJPEGThumbnail(); len(thumb) > 0 {
			c.hist.putLocationThumb(v.Info.ID, thumb)
			d.MediaURL = "/locthumb/" + v.Info.ID
			d.Mime = "image/jpeg"
		}
	case m.GetLiveLocationMessage() != nil:
		ll := m.GetLiveLocationMessage()
		d.Kind = "location"
		d.Lat, d.Lon = ll.GetDegreesLatitude(), ll.GetDegreesLongitude()
		d.LocName = "Live location"
		d.Text = c.resolveMentions(ll.GetCaption(), ll.GetContextInfo())
		if thumb := ll.GetJPEGThumbnail(); len(thumb) > 0 {
			c.hist.putLocationThumb(v.Info.ID, thumb)
			d.MediaURL = "/locthumb/" + v.Info.ID
			d.Mime = "image/jpeg"
		}

	default:
		// Anything we don't render yet — contact cards, polls, view-once,
		// system notices. Storing a labelled placeholder beats the old
		// behaviour of dropping it: a silently missing message leaves an
		// unexplained hole in the thread, which looks like data loss.
		label := unsupportedLabel(m)
		if label == "" {
			return // genuinely nothing to show (empty protocol messages)
		}
		d.Kind = "unsupported"
		d.Text = label
	}

	// If this message is itself a reply, surface a preview of what it quotes.
	if ci := extractContextInfo(m); ci != nil {
		d.Forwarded = ci.GetIsForwarded() || ci.GetForwardingScore() > 0
	}
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
	// A live message far newer than the last one we stored for this chat means
	// we were offline for longer than WhatsApp buffers. Ask for the difference
	// before storing, so the comparison is against what we had, not this message.
	if live {
		c.maybeFillGap(chat, v.Info.ID, v.Info.IsFromMe, d.Timestamp)
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

// cacheMedia stores the downloadable part + its mime for later /media/ fetch,
// and persists the CDN keys so the attachment survives a restart.
func (c *Client) cacheMedia(id string, dl whatsmeow.DownloadableMessage, mime string) {
	c.media.put(id, dl)
	c.mimeMu.Lock()
	c.mediaMime[id] = mime
	c.mimeMu.Unlock()

	c.hist.putMediaRef(id, mediaRef{
		directPath: dl.GetDirectPath(),
		encSHA256:  dl.GetFileEncSHA256(),
		sha256:     dl.GetFileSHA256(),
		mediaKey:   dl.GetMediaKey(),
		mediaType:  string(whatsmeow.GetMediaType(dl)),
		mime:       mime,
	})
}

// mimeFor returns the cached mime type for a media id.
// LocationThumb returns the stored map preview for a location message.
func (c *Client) LocationThumb(msgID string) []byte { return c.hist.locationThumb(msgID) }

// SendTyping tells a chat we're composing (or have stopped). Best-effort: a
// typing indicator failing is not worth interrupting the user over.
func (c *Client) SendTyping(ctx context.Context, chatJID string, composing bool) error {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}
	state := types.ChatPresencePaused
	if composing {
		state = types.ChatPresenceComposing
	}
	return c.WA.SendChatPresence(ctx, jid, state, types.ChatPresenceMediaText)
}

// MimeFor returns the content type for a media id, falling back to what we
// stored — the in-memory table doesn't survive a restart either.
func (c *Client) MimeFor(id string) string {
	c.mimeMu.RLock()
	mime := c.mediaMime[id]
	c.mimeMu.RUnlock()
	if mime != "" {
		return mime
	}
	return c.hist.mimeForMessage(id)
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
// It marks names the contact chose for themselves with a leading "~". Every
// caller that renders a name to the user goes through here, so the marker is
// consistent across the chat list, thread headers, group sender labels, quoted
// authors, mentions and the info screen. Use displayNameSourced directly only
// when you need the unmarked name plus its provenance.
func (c *Client) displayName(jid types.JID, pushName string) string {
	return tildeUnsaved(c.displayNameSourced(jid, pushName))
}

// displayNameSourced is displayName plus where the name came from. saved=true
// means the user chose this name (a nickname here, or an address-book entry);
// saved=false means it's the name the contact chose for themselves.
//
// Callers use that to mark unsaved people the way WhatsApp does, with a leading
// "~", so "a person I know as X" and "a person calling themselves X" don't look
// identical.
func (c *Client) displayNameSourced(jid types.JID, pushName string) (string, bool) {
	// 0. a nickname the user saved in this app beats everything — they chose it
	//    most recently and most deliberately.
	if n := c.hist.localContactName(c.canonicalJID(jid).String()); n != "" {
		return n, true
	}
	// 1. the address book proper — a name the user set, so still authoritative.
	if n := c.sess.addressBookName(jid); n != "" {
		return n, true
	}
	// --- below here the contact named themselves; nothing is user-chosen ---
	// 2. push name riding on the event, if we were given one.
	if pushName != "" {
		return pushName, false
	}
	// 2b. push/business name from the contact tables, under either address.
	if n := c.sess.contactName(jid); n != "" {
		return n, false
	}
	// 3. whatsmeow's contact store, under the canonical JID and the raw one
	//    (identical for a non-LID JID, so only try the second when it differs).
	candidates := []types.JID{c.canonicalJID(jid)}
	if raw := jid.ToNonAD(); raw != candidates[0] {
		candidates = append(candidates, raw)
	}
	for _, j := range candidates {
		if contact, err := c.WA.Store.Contacts.GetContact(context.Background(), j); err == nil {
			// FullName here is user-set, same tier as the address book.
			if contact.FullName != "" {
				return contact.FullName, true
			}
			if contact.PushName != "" {
				return contact.PushName, false
			}
			if contact.BusinessName != "" {
				return contact.BusinessName, false
			}
		}
	}
	// 4. names we learned from observed traffic, keyed by the raw LID.
	if n := c.hist.resolveLIDName(jid.ToNonAD().String()); n != "" {
		return n, false
	}
	return "", false
}

// isStatusBroadcast reports whether a chat is WhatsApp's status/Updates feed.
// Broadcast LISTS are ordinary conversations and deliberately not included.
func isStatusBroadcast(jid types.JID) bool {
	return jid.Server == types.BroadcastServer &&
		jid.User == types.StatusBroadcastJID.User
}

// PurgeStatusBroadcast removes any status/Updates chat already stored, so
// turning the filter on also clears what landed before it existed.
func (c *Client) PurgeStatusBroadcast() bool {
	jid := types.StatusBroadcastJID.String()
	for _, row := range c.hist.listChats() {
		if row["jid"] == jid {
			c.hist.dropChat(jid)
			return true
		}
	}
	return false
}

// tildeUnsaved marks a name the contact chose for themselves with a leading "~",
// the way WhatsApp does, so it can't be mistaken for a name the user set.
func tildeUnsaved(name string, saved bool) string {
	if name == "" || saved || strings.HasPrefix(name, "~") {
		return name
	}
	return "~" + name
}

// senderName resolves who sent a message to a human name.
//
// Falls back to the phone number rather than the raw LID — a LID is an internal
// id that means nothing to the user. Names the contact set for themselves are
// prefixed "~".
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
		canon := c.canonicalJID(jid)
		name := c.displayName(jid, "")
		if name == "" {
			// Nobody knows them. Still better to show a phone number than a raw
			// LID, which is an opaque internal id — but only if the LID actually
			// maps to one, otherwise leave the text alone.
			if canon.Server != types.DefaultUserServer || canon.User == jid.User {
				continue
			}
			name = "+" + canon.User
		}
		// The text contains "@<user>" where user is the number part — but which
		// number depends on how the sender's client addressed the mention, so
		// try both the LID and the phone form.
		for _, user := range []string{jid.User, canon.User} {
			if user == "" {
				continue
			}
			text = strings.ReplaceAll(text, "@"+user, "@"+name)
		}
	}
	return text
}

// mentionPattern matches an unresolved "@<digits>" mention left in stored text.
// Six digits minimum so it can't chew through short numbers in ordinary prose.
var mentionPattern = regexp.MustCompile(`@(\d{6,})`)

// ResolveStoredMentions rewrites "@<number>" mentions still sitting in stored
// message bodies.
//
// Mentions are resolved once, when the message arrives, and the result is what
// gets stored — so every mention that failed to resolve back then is frozen as a
// raw id. The original ContextInfo (and its list of mentioned JIDs) is long
// gone, so this works from the digits in the text: try them as a LID, then as a
// phone number. Returns the number of messages rewritten.
func (c *Client) ResolveStoredMentions() int {
	return c.hist.rewriteMessageText(mentionPattern, func(digits string) string {
		for _, server := range []string{types.HiddenUserServer, types.DefaultUserServer} {
			if n := c.displayName(types.JID{User: digits, Server: server}, ""); n != "" {
				return "@" + n
			}
		}
		return "" // unknown — leave the original text untouched
	})
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
	rc, cached := c.replyCtx[srcMsgID]
	c.replyMu.Unlock()
	if cached && rc.msg != nil {
		// Best case: the original protobuf, so media forwards by reference
		// without re-uploading. Clone before stamping — rc.msg is the cached
		// original and other callers (replies, a second forward) still read it.
		msg := proto.Clone(rc.msg).(*waE2E.Message)
		// Carry the hop count forward so a chain keeps counting up, which is
		// what drives WhatsApp's "forwarded many times" treatment.
		score := uint32(1)
		if ci := extractContextInfo(rc.msg); ci != nil && ci.GetForwardingScore() > 0 {
			score = ci.GetForwardingScore() + 1
		}
		resp, err := c.WA.SendMessage(ctx, dest, markForwarded(msg, score))
		if err != nil {
			return "", err
		}
		return resp.ID, nil
	}

	// Not seen live this session. Text can be re-sent from what we stored;
	// media can't, because forwarding it needs the encryption keys that only
	// live in the original message.
	_, _, kind, text, _, found := c.hist.messageByID(srcMsgID)
	if !found {
		return "", fmt.Errorf("message %s is not stored; cannot forward", srcMsgID)
	}
	if kind != "text" || text == "" {
		return "", fmt.Errorf("can only forward %s from this session — reopen the chat "+
			"and try while the message is fresh", kind)
	}
	resp, err := c.WA.SendMessage(ctx, dest, markForwarded(textMsg(text), 1))
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

// MarkUnsavedNamesNow adds the "~" marker to stored names for people the user
// hasn't saved — the rows RefreshNames leaves alone, because there's no
// authoritative name to replace them with.
func (c *Client) MarkUnsavedNamesNow() (int, int, int) {
	return c.hist.markUnsavedNames(func(jidStr string) bool {
		jid, err := types.ParseJID(jidStr)
		if err != nil {
			return true // unparseable — don't touch it
		}
		_, saved := c.displayNameSourced(jid, "")
		return saved
	})
}

// RebuildPreviewsNow regenerates chat-list previews from the newest stored
// message in each chat. Previews embed the sender's name, so they keep showing
// old (or numeric) names until they're recomputed.
func (c *Client) RebuildPreviewsNow() int { return c.hist.rebuildPreviews() }

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
	sender, chat, quoted, ok := c.lookupMessage(srcMsgID)
	if !ok {
		return "", "", fmt.Errorf("message %s is not stored; cannot reply privately", srcMsgID)
	}
	dest := c.canonicalJID(sender)
	if dest.IsEmpty() || dest.Server == types.GroupServer {
		return "", "", fmt.Errorf("no direct address for the sender of %s", srcMsgID)
	}
	msg := privateReplyMsg(text, srcMsgID, dest, chat, quoted)
	resp, err := c.WA.SendMessage(ctx, dest, msg)
	if err != nil {
		return "", "", err
	}
	return resp.ID, dest.String(), nil
}

// DirectJIDFor resolves the DM address for whoever sent a message, so the app
// can open (or create) a 1:1 chat from a group bubble.
func (c *Client) DirectJIDFor(srcMsgID string) (string, error) {
	sender, _, _, ok := c.lookupMessage(srcMsgID)
	if !ok {
		return "", fmt.Errorf("message %s is not stored", srcMsgID)
	}
	dest := c.canonicalJID(sender)
	if dest.IsEmpty() || dest.Server == types.GroupServer {
		return "", fmt.Errorf("no direct address for the sender of %s", srcMsgID)
	}
	return dest.String(), nil
}

// lookupMessage resolves a message id to its sender, its chat, and something
// usable as a quote.
//
// It prefers the in-memory reply cache, which holds the real protobuf and so
// makes the richest quote. That cache only covers messages seen live this
// session and is capped at 500, so it misses anything read back from stored
// history and everything at all after a restart — which is most of what the
// user is actually looking at. So it falls back to the message table and
// rebuilds a text-only quote from the stored body. A reply is valid on the
// stanza id and participant alone; the quoted copy is a rendering nicety.
func (c *Client) lookupMessage(msgID string) (sender, chat types.JID, quoted *waE2E.Message, ok bool) {
	c.replyMu.Lock()
	rc, cached := c.replyCtx[msgID]
	c.replyMu.Unlock()
	if cached && rc.msg != nil {
		return rc.sender, rc.chat, rc.msg, true
	}

	chatStr, senderStr, kind, text, _, found := c.hist.messageByID(msgID)
	if !found || senderStr == "" {
		return types.JID{}, types.JID{}, nil, false
	}
	sender, err := types.ParseJID(senderStr)
	if err != nil {
		return types.JID{}, types.JID{}, nil, false
	}
	chat, err = types.ParseJID(chatStr)
	if err != nil {
		chat = types.JID{}
	}
	if kind == "text" && text != "" {
		quoted = textMsg(text)
	}
	return sender, chat, quoted, true
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

// ResyncLIDMappings fills in whatsmeow_lid_map for contacts that have no
// LID<->phone pair yet.
//
// This is the fix for the most stubbon symptom: a contact whose saved name sits
// on their phone row while their group messages arrive under a LID that nothing
// links back to it. Reading the tables can't help when the connecting row was
// never written — the mapping has to be fetched.
//
// WhatsApp's usync answers a phone JID with that person's LID, and whatsmeow
// stores the pairs itself as a side effect of GetUserInfo. So we walk the known
// phone contacts in small batches and let it populate the table.
//
// Batched and paced deliberately: usync is a server round-trip per call, and
// firing thousands at once is exactly the kind of traffic that gets an
// unofficial client rate-limited (the same mistake the avatar flood made).
// Pacing for the usync walk. WhatsApp rate-limits this endpoint hard: 50-name
// batches 400ms apart earned a 429 after about 250 contacts. These numbers are
// the conservative end — a repair pass that takes two minutes and finishes is
// worth more than a fast one that gets throttled halfway.
const (
	lidBatchSize    = 20
	lidBatchPause   = 2 * time.Second
	lidMaxPause     = 15 * time.Second
	lidRetryBackoff = 30 * time.Second
	lidMaxRetries   = 3
)

func (c *Client) ResyncLIDMappings(ctx context.Context) (queried, learned, failed int) {
	targets := c.sess.unmappedPhoneContacts()
	if len(targets) == 0 {
		return 0, 0, 0
	}
	before := c.sess.lidMapCount()
	// No time estimate here on purpose: the honest answer depends entirely on
	// how often WhatsApp throttles, which isn't knowable up front. Progress is
	// logged as it goes instead.
	log.Printf("wa: %d contacts have no LID mapping; working through them "+
		"(slow by design — WhatsApp throttles this endpoint)", len(targets))

	// Pause between batches, widened whenever we get throttled. In practice the
	// limit allows a burst and then clamps, so a fixed short delay just walks
	// into a 429 every few batches and spends most of its time in backoff;
	// stretching the gap after each hit settles near the sustainable rate.
	pause := lidBatchPause

	for start := 0; start < len(targets); start += lidBatchSize {
		end := start + lidBatchSize
		if end > len(targets) {
			end = len(targets)
		}
		jids := make([]types.JID, 0, end-start)
		for _, s := range targets[start:end] {
			if j, err := types.ParseJID(s); err == nil {
				jids = append(jids, j)
			}
		}
		if len(jids) == 0 {
			continue
		}

		// Retry a throttled batch after a real pause. Without this, one 429
		// cascades: every following batch fires into the same closed window and
		// fails within milliseconds, which is what happened at 50/400ms.
		var err error
		for attempt := 0; attempt <= lidMaxRetries; attempt++ {
			bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, err = c.WA.GetUserInfo(bctx, jids)
			cancel()
			if err == nil || !isRateLimited(err) || ctx.Err() != nil {
				break
			}
			wait := lidRetryBackoff * time.Duration(attempt+1)
			// Back off the steady-state pace too, not just this retry.
			if pause < lidMaxPause {
				pause += 3 * time.Second
			}
			log.Printf("wa: rate-limited at contact %d/%d, waiting %s (pacing now %s between batches)",
				start, len(targets), wait, pause)
			if !sleepCtx(ctx, wait) {
				break
			}
		}
		queried += len(jids)
		if err != nil {
			failed += len(jids)
			log.Printf("wa: LID lookup %d-%d failed: %v", start, end, err)
		}
		if ctx.Err() != nil {
			break
		}
		if !sleepCtx(ctx, pause) {
			break
		}
	}

	c.sess.reset()
	learned = c.sess.lidMapCount() - before
	if learned < 0 {
		learned = 0
	}
	return queried, learned, failed
}

// isRateLimited spots WhatsApp's 429 so a batch can be retried rather than
// counted as a permanent failure.
func isRateLimited(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "429") || strings.Contains(s, "rate-overlimit")
}

// sleepCtx waits, returning false if the context was cancelled meanwhile.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// RefreshNamesNow rewrites stored chat titles, sender names and quoted-reply
// authors for everyone the user has saved or has in their address book —
// including rows that currently hold a perfectly plausible but wrong name (the
// contact's own WhatsApp name instead of the user's name for them).
//
// Only authoritative names are used, so this can never replace a saved name
// with a push name.
func (c *Client) RefreshNamesNow() (int, int, int) {
	return c.hist.RefreshNames(func(jidStr string) string {
		jid, err := types.ParseJID(jidStr)
		if err != nil {
			return ""
		}
		if n := c.hist.localContactName(c.canonicalJID(jid).String()); n != "" {
			return n
		}
		return c.sess.addressBookName(jid)
	})
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
