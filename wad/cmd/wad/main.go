// Command wad is the WhatsApp daemon: one paired session, serving one phone
// over a WebSocket. Messages work end-to-end; calls are stubbed behind a
// backend interface (wire meowcaller into internal/calls to enable them).
//
// Run:
//   WAD_TOKEN=somesecret go run ./cmd/wad
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
	if os.Getenv("WAD_MIGRATE_LIDS") == "1" {
		if err := waCli.Connect(ctx); err != nil {
			log.Fatalf("connect for migration: %v", err)
		}
		time.Sleep(3 * time.Second) // let the LID store settle
		seen, merged, unmapped := waCli.RunLIDMigration()
		log.Printf("LID migration: %d lid-chats seen, %d merged, %d unmapped", seen, merged, unmapped)
		log.Printf("LID migration: %d chat names backfilled", waCli.BackfillChatNamesNow())
		log.Printf("LID migration: %d message sender names backfilled", waCli.BackfillSenderNamesNow())
		log.Printf("LID migration: %d quoted-reply names backfilled", waCli.BackfillQuotedNamesNow())
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
	mux.HandleFunc("/qr", qrHandler(waCli)) // convenience: view current QR in a browser

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
		}
		// TODO: kind == "image" -> decode d.MediaB64, upload via whatsmeow

	case ws.TCallAnswer, ws.TCallReject, ws.TCallHangup, ws.TCallSignalA:
		cm.HandleAppFrame(ctx, e)

	case ws.TDelete:
		var d struct {
			Chat  string `json:"chat"`
			MsgID string `json:"msgid"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			hub.PushT(ws.TError, map[string]string{"code": "baddelete", "msg": err.Error()})
			return
		}
		if err := waCli.DeleteMessage(ctx, d.Chat, d.MsgID); err != nil {
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
