// Command wad is the WhatsApp daemon: one paired session, serving one phone
// over a WebSocket. Messages work end-to-end; calls are stubbed behind a
// backend interface (wire meowcaller into internal/calls to enable them).
//
// Run:
//
//	WAD_TOKEN=somesecret go run ./cmd/wad
//
// First run prints a QR — scan it from WhatsApp > Linked devices.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"wad/internal/calls"
	"wad/internal/wa"
	"wad/internal/ws"

	"github.com/mdp/qrterminal/v3"
)

func main() {
	token := env("WAD_TOKEN", "changeme")
	addr := env("WAD_ADDR", ":8080")
	dbPath := env("WAD_DB", "wa-session.db")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hub := ws.NewHub()

	waCli, err := wa.New(ctx, dbPath, hub)
	if err != nil {
		log.Fatalf("wa init: %v", err)
	}

	// Call manager with the Noop backend for now. Swap calls.Noop{} for your
	// real meowcaller adapter once implemented — nothing else changes.
	callMgr := calls.NewManager(calls.Noop{}, hub)
	waCli.SetCallHook(calls.WACallHook(callMgr))

	// WAD_MIGRATE_LIDS=1 does a one-shot repair of already-stored rows against
	// the current resolver, then exits: merge duplicate @lid chats into their
	// phone JID, then re-resolve chat and sender names that are still raw
	// numbers. Safe to re-run — each pass only touches rows still unresolved.
	// WAD_RESYNC=1 additionally asks WhatsApp to re-send the whole contact list
	// before repairing, which is what to reach for when stored history still
	// shows raw numbers. It refreshes the same tables a fresh pairing would,
	// without unlinking the session.
	resync := os.Getenv("WAD_RESYNC") == "1"
	if os.Getenv("WAD_MIGRATE_LIDS") == "1" || resync {
		if err := waCli.Connect(ctx); err != nil {
			log.Fatalf("connect for migration: %v", err)
		}
		time.Sleep(3 * time.Second) // let the LID store settle
		if resync {
			log.Printf("LID migration: requesting a full contact resync…")
			if err := waCli.ResyncContacts(ctx); err != nil {
				log.Printf("LID migration: contact resync failed (%v) — repairing with what we have", err)
			} else {
				// The sync arrives as app-state patches; give them a moment to
				// land in whatsmeow's tables before we read them back.
				time.Sleep(10 * time.Second)
				log.Printf("LID migration: contact resync done")
			}
			// Then fill in the LID<->phone pairs. This is the step that fixes a
			// contact whose saved name is on their phone row while their group
			// messages arrive under an unlinked LID.
			log.Printf("LID migration: looking up LIDs for contacts that have none…")
			queried, learned, failed := waCli.ResyncLIDMappings(ctx)
			log.Printf("LID migration: asked about %d contacts, learned %d new LID mappings", queried, learned)
			if failed > 0 {
				log.Printf("LID migration: %d contacts could not be looked up (rate limit) — "+
					"re-run WAD_RESYNC=1 later to pick up the rest", failed)
			}
		}
		seen, merged, unmapped := waCli.RunLIDMigration()
		log.Printf("LID migration: %d lid-chats seen, %d merged, %d unmapped", seen, merged, unmapped)
		log.Printf("LID migration: %d chat names backfilled", waCli.BackfillChatNamesNow())
		log.Printf("LID migration: %d message sender names backfilled", waCli.BackfillSenderNamesNow())
		log.Printf("LID migration: %d quoted-reply names backfilled", waCli.BackfillQuotedNamesNow())
		// Finally, correct rows holding a wrong-but-plausible name — the passes
		// above only touch names that look like bare numbers.
		rc, rm, rq := waCli.RefreshNamesNow()
		log.Printf("LID migration: refreshed %d chat titles, %d sender names, %d quoted names "+
			"to saved/address-book names", rc, rm, rq)
		// Then mark everyone the user hasn't saved with "~". Must come after the
		// refresh above, which is what un-marks anyone who IS saved.
		mc, mm, mq := waCli.MarkUnsavedNamesNow()
		log.Printf("LID migration: marked %d chat titles, %d sender names, %d quoted names "+
			"as unsaved (~)", mc, mm, mq)
		// Mentions are baked into the stored message body at receive time, so
		// ones that failed to resolve then are frozen as raw ids until now.
		log.Printf("LID migration: %d messages had @mentions resolved", waCli.ResolveStoredMentions())
		// Previews embed the sender's name, so they must be recomputed last —
		// after every name pass above has settled.
		log.Printf("LID migration: %d chat previews rebuilt", waCli.RebuildPreviewsNow())
		if os.Getenv("WAD_INCLUDE_STATUS") != "1" && waCli.PurgeStatusBroadcast() {
			log.Printf("LID migration: removed the stored status/Updates chat " +
				"(WAD_INCLUDE_STATUS=1 to keep it in future)")
		}
		return
	}

	// WAD_REFETCH_MEDIA=1 asks the phone to re-send history for chats whose
	// attachments have no stored keys, so old photos become viewable again.
	// Answers arrive asynchronously as HistorySync events, so this stays
	// connected for a while after the requests go out rather than exiting
	// immediately.
	if os.Getenv("WAD_REFETCH_MEDIA") == "1" {
		if err := waCli.Connect(ctx); err != nil {
			log.Fatalf("connect for media refetch: %v", err)
		}
		time.Sleep(5 * time.Second) // let the session settle
		before, missing := waCli.MediaKeyStats()
		log.Printf("media refetch: %d attachments have keys, %d don't", before, missing)

		maxReq := 40
		if v := os.Getenv("WAD_REFETCH_MAX"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				maxReq = n
			}
		}
		sent, chats := waCli.RefetchMediaKeys(ctx, maxReq)
		log.Printf("media refetch: sent %d history requests across %d chats", sent, chats)

		if sent > 0 {
			// The phone replies whenever it feels like it. Hold the connection
			// open so the HistorySync handler can store what comes back.
			wait := 90 * time.Second
			log.Printf("media refetch: waiting %s for responses (Ctrl-C to stop early)…", wait)
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			after, stillMissing := waCli.MediaKeyStats()
			log.Printf("media refetch: now %d attachments have keys (+%d), %d still missing",
				after, after-before, stillMissing)
			if after == before {
				log.Printf("media refetch: nothing came back — the phone may be offline, " +
					"or may refuse to serve history this old")
			}
		}
		waCli.WA.Disconnect()
		return
	}

	// Render pairing QR to the terminal on first run.
	waCli.SetQRHook(func(code string) {
		qrterminal.GenerateHalfBlock(code, qrterminal.L, os.Stdout)
	})

	// Route frames coming from the phone.
	hub.OnFrame(func(e ws.Envelope) {
		routeAppFrame(ctx, e, waCli, callMgr, hub)
	})

	// HTTP: the WebSocket endpoint + a media fetch endpoint (stub).
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.ServeHTTP(token))
	mux.HandleFunc("/media/", mediaHandler(waCli))
	mux.HandleFunc("/avatar/", avatarHandler(waCli))
	mux.HandleFunc("/locthumb/", locThumbHandler(waCli))
	mux.HandleFunc("/notify-summary", notifySummaryHandler(waCli, token))
	mux.HandleFunc("/qr", qrHandler(waCli)) // convenience: view current QR in a browser
	mux.HandleFunc("/debug/message", fakeMessageHandler(hub, token))

	// A closed laptop lid is the most common reason the phone stops receiving,
	// and it is indistinguishable from the app being killed unless someone says
	// so out loud.
	go watchForSuspend(ctx)

	go func() {
		log.Printf("wad: listening on %s (ws at /ws?token=...)", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Fatalf("http: %v", err)
		}
	}()

	// Connect / pair. The wa package drives the QR channel, pushes TQR to the
	// phone, and logs the code for first-run scanning on the server.
	if err := waCli.Connect(ctx); err != nil {
		log.Fatalf("connect: %v", err)
	}

	<-ctx.Done()
	log.Printf("wad: shutting down")
	waCli.WA.Disconnect()
}

func routeAppFrame(ctx context.Context, e ws.Envelope, waCli *wa.Client, cm *calls.Manager, hub *ws.Hub) {
	switch e.T {
	case ws.TSend:
		var d ws.SendData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "badsend", "msg": err.Error()})
			return
		}
		if d.Kind == "text" {
			var id string
			var err error
			if d.Private && d.QuotedID != "" {
				var dest string
				id, dest, err = waCli.SendPrivateReply(ctx, d.QuotedID, d.Text)
				if err == nil {
					// Tell the app which DM it landed in so it can open it.
					hub.Push(ws.Envelope{T: ws.TReceipt, ID: e.ID,
						Data: mustJSON(map[string]any{"msgid": id, "type": "sent", "chat": dest})})
					return
				}
			} else if d.QuotedID != "" {
				id, err = waCli.SendReply(ctx, d.ChatJID, d.Text, d.QuotedID)
			} else {
				id, err = waCli.SendText(ctx, d.ChatJID, d.Text)
			}
			if err != nil {
				hub.PushT(ws.TError, map[string]string{"code": "send", "msg": err.Error()})
				return
			}
			// echo back an ack with the assigned id, correlating on e.ID
			hub.Push(ws.Envelope{T: ws.TReceipt, ID: e.ID,
				Data: mustJSON(map[string]any{"msgid": id, "type": "sent"})})
			waCli.RecordSent(ws.MsgData{
				MsgID: id, ChatJID: d.ChatJID, Kind: "text", Text: d.Text,
				QuotedID: d.QuotedID,
			})
		}
		if d.Kind == "location" {
			id, err := waCli.SendLocation(ctx, d.ChatJID, d.Lat, d.Lon, d.Accuracy,
				d.LocName, d.LocAddress, d.QuotedID)
			if err != nil {
				hub.PushT(ws.TError, map[string]string{"code": "sendloc", "msg": err.Error()})
				return
			}
			hub.Push(ws.Envelope{T: ws.TReceipt, ID: e.ID,
				Data: mustJSON(map[string]any{"msgid": id, "type": "sent"})})
			waCli.RecordSent(ws.MsgData{
				MsgID: id, ChatJID: d.ChatJID, Kind: "location",
				Lat: d.Lat, Lon: d.Lon, LocName: d.LocName,
				LocAddress: d.LocAddress, QuotedID: d.QuotedID,
			})
		}
		if d.Kind == "image" || d.Kind == "video" || d.Kind == "audio" ||
			d.Kind == "gif" || d.Kind == "doc" {
			id, err := waCli.SendMedia(ctx, d.ChatJID, d.Kind, d.MediaB64, d.Mime,
				d.Text, d.FileName, d.QuotedID)
			if err != nil {
				hub.PushT(ws.TError, map[string]string{"code": "sendmedia", "msg": err.Error()})
				return
			}
			hub.Push(ws.Envelope{T: ws.TReceipt, ID: e.ID,
				Data: mustJSON(map[string]any{"msgid": id, "type": "sent"})})
			waCli.RecordSent(ws.MsgData{
				MsgID: id, ChatJID: d.ChatJID, Kind: d.Kind, Text: d.Text,
				Mime: d.Mime, MediaURL: "/media/" + id, QuotedID: d.QuotedID,
			})
		}

	case ws.TLiveLoc:
		var d ws.LiveLocData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "badliveloc", "msg": err.Error()})
			return
		}
		switch d.Action {
		case "start":
			_, endsAt, err := waCli.StartLiveLocation(ctx, d.ChatJID, d.Lat, d.Lon, d.Accuracy, d.Secs)
			if err != nil {
				hub.PushT(ws.TError, map[string]string{"code": "liveloc", "msg": err.Error()})
				return
			}
			hub.PushT(ws.TLiveLocState, map[string]any{
				"chat": d.ChatJID, "active": true, "until": endsAt.Unix()})
		case "update":
			// A share that has run out stops the app producing fixes, which is
			// what turns the GPS back off — so the reply matters even though
			// there is nothing to report.
			running, err := waCli.UpdateLiveLocation(ctx, d.ChatJID, d.Lat, d.Lon, d.Accuracy)
			if err != nil && running {
				hub.PushT(ws.TError, map[string]string{"code": "liveloc", "msg": err.Error()})
			}
			if !running {
				hub.PushT(ws.TLiveLocState, map[string]any{
					"chat": d.ChatJID, "active": false, "until": 0})
			}
		case "stop":
			waCli.StopLiveLocation(d.ChatJID)
			hub.PushT(ws.TLiveLocState, map[string]any{
				"chat": d.ChatJID, "active": false, "until": 0})
		default:
			hub.PushT(ws.TError, map[string]string{
				"code": "badliveloc", "msg": "unknown action " + d.Action})
		}

	case ws.TCallAnswer, ws.TCallReject, ws.TCallHangup, ws.TCallSignalA:
		cm.HandleAppFrame(ctx, e)

	case ws.TEdit:
		var d struct {
			Chat  string `json:"chat"`
			MsgID string `json:"msgid"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "badedit", "msg": err.Error()})
			return
		}
		if err := waCli.EditMessage(ctx, d.Chat, d.MsgID, d.Text); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "edit", "msg": err.Error()})
		}

	case ws.TDelete:
		var d struct {
			Chat  string `json:"chat"`
			MsgID string `json:"msgid"`
			Scope string `json:"scope"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "baddelete", "msg": err.Error()})
			return
		}
		if err := waCli.DeleteMessage(ctx, d.Chat, d.MsgID, d.Scope); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "delete", "msg": err.Error()})
			return
		}
		hub.PushT(ws.TDeleted, map[string]string{"chat": d.Chat, "msgid": d.MsgID})

	case ws.TForward:
		var d struct {
			SrcMsgID string `json:"srcmsgid"`
			Dest     string `json:"dest"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "badforward", "msg": err.Error()})
			return
		}
		if _, err := waCli.ForwardMessage(ctx, d.SrcMsgID, d.Dest); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "forward", "msg": err.Error()})
		}

	case ws.TChatAction:
		var d ws.ChatActionData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "badchataction", "msg": err.Error()})
			return
		}
		var err error
		switch d.Action {
		case "pin":
			err = waCli.SetPin(ctx, d.ChatJID, d.On)
		case "mute":
			err = waCli.SetMute(ctx, d.ChatJID, d.On, time.Duration(d.Duration)*time.Second)
		case "archive":
			err = waCli.SetArchive(ctx, d.ChatJID, d.On)
		case "delete":
			err = waCli.DeleteChat(ctx, d.ChatJID)
		default:
			err = fmt.Errorf("unknown chat action %q", d.Action)
		}
		if err != nil {
			// The account rejected the write, so our stored state was left
			// untouched. Tell the app so it can roll its optimistic UI back.
			hub.PushT(ws.TError, map[string]string{"code": "chataction", "msg": err.Error()})
			hub.PushT(ws.TChatList, waCli.ListChats())
			return
		}
		if d.Action == "delete" {
			hub.PushT(ws.TChatUpdate, map[string]any{"chat": d.ChatJID, "removed": true})
		} else {
			pinned, muted, archived := waCli.ChatFlags(d.ChatJID)
			hub.PushT(ws.TChatUpdate, map[string]any{
				"chat": d.ChatJID, "pinned": pinned, "muted": muted, "archived": archived,
			})
		}

	case ws.TGetProfile:
		var d struct {
			JID      string `json:"jid"`
			SrcMsgID string `json:"srcmsgid"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "badprofile", "msg": err.Error()})
			return
		}
		// A group bubble carries a per-group LID, not a usable address; when the
		// app asks by message id instead, resolve the sender's DM JID first.
		jid := d.JID
		if d.SrcMsgID != "" {
			resolved, err := waCli.DirectJIDFor(d.SrcMsgID)
			if err != nil {
				hub.PushT(ws.TError, map[string]string{"code": "profile", "msg": err.Error()})
				return
			}
			jid = resolved
		}
		hub.Push(ws.Envelope{T: ws.TProfile, ID: e.ID,
			Data: mustJSON(waCli.ProfileFor(ctx, jid))})

	case ws.TSaveContact:
		var d struct {
			JID  string `json:"jid"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "badsavecontact", "msg": err.Error()})
			return
		}
		hub.Push(ws.Envelope{T: ws.TProfile, ID: e.ID,
			Data: mustJSON(waCli.SaveContact(d.JID, d.Name))})
		hub.PushT(ws.TChatList, waCli.ListChats())

	case ws.TTyping:
		var d struct {
			Chat  string `json:"chat"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return
		}
		// Silent on failure: a typing indicator is not worth an error frame.
		if err := waCli.SendTyping(ctx, d.Chat, d.State == "composing"); err != nil {
			log.Printf("typing %s: %v", d.Chat, err)
		}

	case ws.TSendReaction:
		var d struct {
			Chat  string `json:"chat"`
			MsgID string `json:"msgid"`
			Emoji string `json:"emoji"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "badreaction", "msg": err.Error()})
			return
		}
		if err := waCli.SendReaction(ctx, d.Chat, d.MsgID, d.Emoji); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "reaction", "msg": err.Error()})
		}

	case ws.TSearch:
		var d struct {
			Q     string `json:"q"`
			Chat  string `json:"chat"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "badsearch", "msg": err.Error()})
			return
		}
		hub.Push(ws.Envelope{T: ws.TSearchResult, ID: e.ID,
			Data: mustJSON(map[string]any{"q": d.Q, "results": waCli.Search(d.Q, d.Chat, d.Limit)})})

	case ws.TPushSub:
		var d struct {
			Endpoint string `json:"endpoint"`
			Remove   bool   `json:"remove"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return
		}
		if d.Remove {
			waCli.RemovePushSubscription(d.Endpoint)
		} else if err := waCli.AddPushSubscription(d.Endpoint); err != nil {
			log.Printf("pushsub: %v", err)
		}

	case ws.TWatch:
		var d struct {
			JID string `json:"jid"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return
		}
		// Best-effort: presence is a nicety, and WhatsApp refuses for people
		// who have it turned off.
		if err := waCli.WatchPresence(ctx, d.JID); err != nil {
			log.Printf("watch %s: %v", d.JID, err)
		}

	case ws.TMarkRead:
		var d struct {
			JID string `json:"jid"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "badmarkread", "msg": err.Error()})
			return
		}
		// Quiet on failure: the user opened a chat, they didn't ask to send a
		// receipt, so an error here shouldn't interrupt them.
		if n, err := waCli.MarkChatRead(ctx, d.JID); err != nil {
			log.Printf("markread %s: %v", d.JID, err)
		} else if n > 0 {
			log.Printf("markread: %d messages in %s", n, d.JID)
			// Tell the app the badge is clear. It clears optimistically too,
			// but this is what makes it stick across a refresh.
			hub.PushT(ws.TChatUpdate, map[string]any{"chat": d.JID, "unread": 0})
		}

	case ws.TGetChats:
		hub.PushT(ws.TChatList, waCli.ListChats())

	case ws.TGetHistory:
		var d struct {
			JID    string `json:"jid"`
			Before int64  `json:"before"`
			Limit  int    `json:"limit"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "badhistory", "msg": err.Error()})
			return
		}
		msgs := waCli.History(d.JID, d.Before, d.Limit)
		hub.Push(ws.Envelope{T: ws.THistory, ID: e.ID,
			Data: mustJSON(map[string]any{"jid": d.JID, "messages": msgs})})

	default:
		log.Printf("main: unhandled app frame %q", e.T)
	}
}

func avatarHandler(c *wa.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jid := strings.TrimPrefix(r.URL.Path, "/avatar/")
		if jid == "" {
			http.Error(w, "missing jid", http.StatusBadRequest)
			return
		}
		data, mime, ok := c.AvatarFor(r.Context(), jid)
		if !ok {
			// 404 tells the app to draw its initials fallback
			http.Error(w, "no avatar", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Write(data)
	}
}

// locThumbHandler serves the map preview WhatsApp embeds in a location message.
// These are stored bytes, not CDN downloads, so they're always available and
// need no keys.
// notifySummaryHandler tells a woken service worker what to say.
//
// The push itself carries no payload, so the worker fetches this to turn a bare
// wake-up into "Alex — 3 new messages". Token-guarded like the socket: it
// reports who is messaging the user, which isn't public.
// fakeMessageHandler pushes a made-up INCOMING message to the phone.
//
// Testing notifications properly needs a message you didn't send, arriving
// while the phone is shut — and messaging yourself doesn't work, because those
// come back as fromme and are skipped on purpose. Asking someone to text you on
// demand doesn't scale to debugging.
//
// This sends the same frame a real message sends, so the app can't tell the
// difference: it goes through pushMsg, alertForMessage, and the notification
// decision exactly as it would at 3am.
//
//	curl "http://localhost:8080/debug/message?token=$WAD_TOKEN"
//	curl "http://localhost:8080/debug/message?token=$WAD_TOKEN&name=Mum&text=call+me"
//
// Nothing is stored: the app shows it until the next chat list arrives from the
// daemon, and it never touches the database.
func fakeMessageHandler(hub *ws.Hub, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		name := r.URL.Query().Get("name")
		if name == "" {
			name = "Test contact"
		}
		text := r.URL.Query().Get("text")
		if text == "" {
			text = "Test message from the daemon"
		}
		now := time.Now()
		msg := ws.MsgData{
			MsgID:      fmt.Sprintf("debug-%d", now.UnixNano()),
			ChatJID:    "debug@s.whatsapp.net",
			ChatName:   name,
			SenderJID:  "debug@s.whatsapp.net",
			SenderName: name,
			FromMe:     false,
			Timestamp:  now.Unix(),
			Kind:       "text",
			Text:       text,
		}
		hub.PushT(ws.TMessage, msg)
		log.Printf("debug: pushed a fake message from %q", name)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "sent %q from %q\n", text, name)
	}
}

func notifySummaryHandler(c *wa.Client, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(c.SummaryForNotification())
	}
}

func locThumbHandler(c *wa.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/locthumb/")
		data := c.LocationThumb(id)
		if len(data) == 0 {
			http.Error(w, "no thumbnail", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Write(data)
	}
}

// thumbHandler serves the small preview shipped inside a message. The app asks
// for this in a chat bubble and only fetches /media/ when a photo is opened
// full-screen, which keeps a thread's decoded-image cost in kilobytes.
func thumbHandler(c *wa.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/thumb/")
		data := c.Thumb(id)
		if len(data) == 0 {
			http.Error(w, "no thumbnail", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Write(data)
	}
}

func mediaHandler(c *wa.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// path is /media/<messageID>
		id := strings.TrimPrefix(r.URL.Path, "/media/")
		if id == "" {
			http.Error(w, "missing media id", http.StatusBadRequest)
			return
		}
		data, err := c.DownloadMedia(r.Context(), id)
		if err != nil {
			log.Printf("media %s: %v", id, err)
			http.Error(w, "media unavailable", http.StatusNotFound)
			return
		}
		mime := c.MimeFor(id)
		if mime == "" {
			mime = "application/octet-stream"
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Cache-Control", "private, max-age=3600")
		w.Write(data)
	}
}

func qrHandler(c *wa.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c.WA.Store.ID != nil {
			w.Write([]byte("already paired"))
			return
		}
		w.Write([]byte("not paired — scan the QR shown in the daemon logs"))
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
