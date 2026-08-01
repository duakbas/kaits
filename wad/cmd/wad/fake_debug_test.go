package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"wad/internal/ws"
)

func TestFakeMessageHandler(t *testing.T) {
	hub := ws.NewHub()
	h := fakeMessageHandler(hub, "secret")

	// Wrong token must be refused.
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/debug/message?token=nope", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token got %d, want 401", w.Code)
	}

	w = httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/debug/message?token=secret&name=Mum&text=call+me", nil))
	if w.Code != 200 {
		t.Fatalf("good token got %d, want 200", w.Code)
	}

	// The frame the app would receive.
	env, err := ws.Frame(ws.TMessage, ws.MsgData{
		ChatName: "Mum", SenderName: "Mum", Text: "call me", Kind: "text",
	})
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	var out map[string]any
	b, _ := json.Marshal(env)
	json.Unmarshal(b, &out)
	if out["t"] != ws.TMessage {
		t.Errorf("frame type = %v, want %v", out["t"], ws.TMessage)
	}
	d, _ := out["data"].(map[string]any)
	if d["fromme"] != false {
		t.Errorf("fromme = %v, want false — a fromme message is skipped and would notify nothing", d["fromme"])
	}
}
