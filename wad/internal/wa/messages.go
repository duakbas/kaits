package wa

import (
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// textMsg builds a simple text message. Kept in its own file so the waE2E
// protobuf import is localized and easy to update if the proto path moves
// (it has changed between whatsmeow versions — verify on go mod tidy).
func textMsg(body string) *waE2E.Message {
	return &waE2E.Message{
		Conversation: proto.String(body),
	}
}

// extractContextInfo pulls the ContextInfo out of whichever message variant
// carries it (text or any media type). Returns nil if none.
func extractContextInfo(m *waE2E.Message) *waE2E.ContextInfo {
	switch {
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetContextInfo()
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetContextInfo()
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetContextInfo()
	case m.GetAudioMessage() != nil:
		return m.GetAudioMessage().GetContextInfo()
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetContextInfo()
	}
	return nil
}

// quotedPreview produces a short text preview of a quoted message for the
// reply bar. For media it returns a bracketed kind label.
func quotedPreview(m *waE2E.Message) string {
	if m == nil {
		return ""
	}
	switch {
	case m.GetConversation() != "":
		return m.GetConversation()
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetText()
	case m.GetImageMessage() != nil:
		return "[photo]"
	case m.GetVideoMessage() != nil:
		if m.GetVideoMessage().GetGifPlayback() {
			return "[gif]"
		}
		return "[video]"
	case m.GetAudioMessage() != nil:
		return "[voice]"
	case m.GetDocumentMessage() != nil:
		return "[document]"
	case m.GetStickerMessage() != nil:
		return "[sticker]"
	}
	return ""
}

// replyTextMsg builds a text reply that quotes an earlier message. WhatsApp
// represents a reply as an ExtendedTextMessage whose ContextInfo carries the
// quoted message's stanza id, the original sender (Participant), and a copy of
// the quoted message body.
func replyTextMsg(body, quotedID string, sender types.JID, quoted *waE2E.Message) *waE2E.Message {
	return &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(body),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:      proto.String(quotedID),
				Participant:   proto.String(sender.String()),
				QuotedMessage: quoted,
			},
		},
	}
}

// privateReplyMsg builds a "reply privately" — a DM to someone that quotes a
// message they sent in a group. It's an ordinary reply plus RemoteJID naming
// the group the quoted message came from; without that, the recipient's client
// can't resolve the quote and shows a bare, contextless bubble.
func privateReplyMsg(body, quotedID string, sender, group types.JID, quoted *waE2E.Message) *waE2E.Message {
	msg := replyTextMsg(body, quotedID, sender, quoted)
	msg.ExtendedTextMessage.ContextInfo.RemoteJID = proto.String(group.String())
	return msg
}

