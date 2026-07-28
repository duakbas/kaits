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

	if os.Getenv("WAD_MIGRATE_LIDS") == "1" {
		if err := waCli.Connect(ctx); err != nil { log.Fatalf("connect for migration: %v", err) }
		time.Sleep(3 * time.Second) // let the LID store settle
		seen, merged, unmapped := waCli.RunLIDMigration()
		log.Printf("LID migration done: %d lid-chats seen, %d merged, %d unmapped", seen, merged, unmapped)
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
	log.Printf("FRAME-DEBUG received: %q", e.T)
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
			if d.QuotedID != "" {
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

	case ws.TGetChats:
		{
			cl := waCli.ListChats()
			log.Printf("CHATLIST-DEBUG: ListChats returned %d chats, pushing", len(cl))
			hub.PushT(ws.TChatList, cl)
		}

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
