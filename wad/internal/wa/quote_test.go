package wa

import (
	"testing"

	"wad/internal/ws"
)

// A reply we send has to come back carrying the message it answers.
//
// An incoming reply arrives with the quoted body inside its context info, so
// it renders for free. Our own replies had only the quoted id — which is all
// WhatsApp needs to make the reply valid, and not enough for the app, which
// draws the quote bar on quotedtext. The bubble came back bare.
func TestFillQuoteFromStoredMessage(t *testing.T) {
	h := newHist(t)
	c := &Client{hist: h}
	chat := "1234@s.whatsapp.net"
	them := "9999@s.whatsapp.net"

	putMsg(h, chat, "them1", them, "", "kan kız kesmece diyorsun", 100, false)
	putMsg(h, chat, "mine1", "", "", "irchelde yerimiz aldık", 101, true)

	// Quoting someone else: their body, and a name rather than a raw JID.
	d := ws.MsgData{MsgID: "r1", ChatJID: chat, Kind: "text", Text: "ok", QuotedID: "them1"}
	c.fillQuote(&d)
	if d.QuotedText != "kan kız kesmece diyorsun" {
		t.Errorf("quoted text = %q, want the stored body", d.QuotedText)
	}
	if d.QuotedName == "" {
		t.Error("quoted name is empty; the quote bar would render nameless")
	}
	if d.QuotedName == them {
		t.Errorf("quoted name = %q, want a display name and not the raw JID", d.QuotedName)
	}

	// Quoting ourselves: the app labels its own messages "You" everywhere
	// else, and the sender column is empty for messages we sent, so a JID
	// lookup would produce nothing at all here.
	d = ws.MsgData{MsgID: "r2", ChatJID: chat, Kind: "text", Text: "ok", QuotedID: "mine1"}
	c.fillQuote(&d)
	if d.QuotedText != "irchelde yerimiz aldık" {
		t.Errorf("quoted text = %q, want our own stored body", d.QuotedText)
	}
	if d.QuotedName != "You" {
		t.Errorf("quoted name = %q, want You", d.QuotedName)
	}
}

// The lookup can miss — a message older than our history, or one from before a
// resync. The reply is still valid on the stanza id alone, so this must degrade
// to a bubble without a quote bar rather than refuse to send or invent a body.
func TestFillQuoteToleratesAMissingOriginal(t *testing.T) {
	h := newHist(t)
	c := &Client{hist: h}

	d := ws.MsgData{MsgID: "r1", ChatJID: "1234@s.whatsapp.net", Kind: "text",
		Text: "ok", QuotedID: "never-stored"}
	c.fillQuote(&d)
	if d.QuotedText != "" || d.QuotedName != "" {
		t.Errorf("invented a quote for an unknown id: %q / %q", d.QuotedText, d.QuotedName)
	}
	if d.QuotedID != "never-stored" {
		t.Errorf("quoted id = %q, want it left alone — the reply is valid on it", d.QuotedID)
	}
}

// Nothing here may overwrite a quote that already arrived with the message,
// and a message that is not a reply must come out untouched.
func TestFillQuoteLeavesExistingValuesAlone(t *testing.T) {
	h := newHist(t)
	c := &Client{hist: h}
	chat := "1234@s.whatsapp.net"
	putMsg(h, chat, "them1", "9999@s.whatsapp.net", "", "stored body", 100, false)

	d := ws.MsgData{MsgID: "r1", ChatJID: chat, Kind: "text", QuotedID: "them1",
		QuotedText: "already here", QuotedName: "Someone"}
	c.fillQuote(&d)
	if d.QuotedText != "already here" || d.QuotedName != "Someone" {
		t.Errorf("overwrote an existing quote: %q / %q", d.QuotedText, d.QuotedName)
	}

	plain := ws.MsgData{MsgID: "p1", ChatJID: chat, Kind: "text", Text: "hi"}
	c.fillQuote(&plain)
	if plain.QuotedText != "" || plain.QuotedName != "" {
		t.Errorf("gave a non-reply a quote: %q / %q", plain.QuotedText, plain.QuotedName)
	}
}

// The bug was not in fillQuote — it was that nothing called it. RecordSent is
// the one path every outgoing message takes, so this is the assertion that
// would have caught the original defect: send a reply, read back what was
// stored, and it has to carry the quote.
func TestRecordSentStoresTheQuote(t *testing.T) {
	h := newHist(t)
	c := &Client{hist: h, hub: ws.NewHub()}
	chat := "1234@s.whatsapp.net"
	putMsg(h, chat, "them1", "9999@s.whatsapp.net", "", "kan kız kesmece diyorsun", 100, false)

	c.RecordSent(ws.MsgData{
		MsgID: "r1", ChatJID: chat, Kind: "text", Text: "ok", QuotedID: "them1",
	})

	msgs := h.history(chat, 0, 10)
	var reply *ws.MsgData
	for i := range msgs {
		if msgs[i].MsgID == "r1" {
			reply = &msgs[i]
		}
	}
	if reply == nil {
		t.Fatal("the reply was not stored at all")
	}
	if reply.QuotedText == "" {
		t.Error("stored reply has no quoted text — the app draws the quote bar on it, " +
			"so the bubble renders bare")
	}
	if reply.QuotedName == "" {
		t.Error("stored reply has no quoted name")
	}
}
