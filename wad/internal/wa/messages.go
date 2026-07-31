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

// editedMessageFrom pulls an edit out of whichever shape it arrived in.
//
// WhatsApp wraps an edit in a ProtocolMessage of type MESSAGE_EDIT, which may
// itself sit inside an EditedMessage envelope depending on the sending client.
// Returns the id of the message being corrected and its new text.
func editedMessageFrom(m *waE2E.Message) (string, string, bool) {
	pm := m.GetProtocolMessage()
	if pm == nil && m.GetEditedMessage() != nil {
		pm = m.GetEditedMessage().GetMessage().GetProtocolMessage()
	}
	if pm == nil || pm.GetType() != waE2E.ProtocolMessage_MESSAGE_EDIT {
		return "", "", false
	}
	key := pm.GetKey()
	if key == nil || key.GetID() == "" {
		return "", "", false
	}
	edited := pm.GetEditedMessage()
	if edited == nil {
		return "", "", false
	}
	body := edited.GetConversation()
	if body == "" {
		body = edited.GetExtendedTextMessage().GetText()
	}
	if body == "" {
		// Media captions can be edited too.
		body = edited.GetImageMessage().GetCaption()
		if body == "" {
			body = edited.GetVideoMessage().GetCaption()
		}
	}
	if body == "" {
		return "", "", false
	}
	return key.GetID(), body, true
}

// unsupportedLabel names a message type the app can't render yet, so it can be
// shown as a placeholder instead of vanishing. Returns "" for message types that
// legitimately have nothing to display — protocol housekeeping the user never
// sent and shouldn't see.
func unsupportedLabel(m *waE2E.Message) string {
	switch {
	case m.GetContactMessage() != nil:
		if n := m.GetContactMessage().GetDisplayName(); n != "" {
			return "[contact: " + n + "]"
		}
		return "[contact card]"
	case m.GetContactsArrayMessage() != nil:
		return "[contact cards]"
	case m.GetPollCreationMessageV3() != nil:
		if n := m.GetPollCreationMessageV3().GetName(); n != "" {
			return "[poll: " + n + "]"
		}
		return "[poll]"
	case m.GetPollUpdateMessage() != nil:
		return "[poll vote]"
	case m.GetEventMessage() != nil:
		return "[event]"
	case m.GetProductMessage() != nil:
		return "[product]"
	case m.GetGroupInviteMessage() != nil:
		return "[group invite]"
	case m.GetViewOnceMessage() != nil, m.GetViewOnceMessageV2() != nil,
		m.GetViewOnceMessageV2Extension() != nil:
		return "[view-once message]"
	case m.GetPtvMessage() != nil:
		return "[video note]"
	case m.GetProtocolMessage() != nil, m.GetSenderKeyDistributionMessage() != nil:
		return "" // housekeeping, not a message the user sent
	}
	return "[unsupported message]"
}

// markForwarded stamps a message as a forward, so the recipient's client shows
// the "Forwarded" label instead of presenting it as freshly written.
//
// WhatsApp carries this in ContextInfo: IsForwarded plus a ForwardingScore that
// counts hops (at 5+ clients show "forwarded many times"). The field lives on
// whichever message variant is set, so this has to walk them; a variant with no
// ContextInfo yet gets one.
func markForwarded(m *waE2E.Message, score uint32) *waE2E.Message {
	if m == nil {
		return m
	}
	if score < 1 {
		score = 1
	}
	stamp := func(ci *waE2E.ContextInfo) *waE2E.ContextInfo {
		if ci == nil {
			ci = &waE2E.ContextInfo{}
		}
		ci.IsForwarded = proto.Bool(true)
		ci.ForwardingScore = proto.Uint32(score)
		return ci
	}
	switch {
	case m.GetExtendedTextMessage() != nil:
		m.ExtendedTextMessage.ContextInfo = stamp(m.ExtendedTextMessage.GetContextInfo())
	case m.GetImageMessage() != nil:
		m.ImageMessage.ContextInfo = stamp(m.ImageMessage.GetContextInfo())
	case m.GetVideoMessage() != nil:
		m.VideoMessage.ContextInfo = stamp(m.VideoMessage.GetContextInfo())
	case m.GetAudioMessage() != nil:
		m.AudioMessage.ContextInfo = stamp(m.AudioMessage.GetContextInfo())
	case m.GetDocumentMessage() != nil:
		m.DocumentMessage.ContextInfo = stamp(m.DocumentMessage.GetContextInfo())
	case m.GetStickerMessage() != nil:
		m.StickerMessage.ContextInfo = stamp(m.StickerMessage.GetContextInfo())
	case m.GetConversation() != "":
		// A plain Conversation has nowhere to put ContextInfo — promote it to an
		// ExtendedTextMessage, which is what WhatsApp itself does when forwarding.
		return &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text:        proto.String(m.GetConversation()),
				ContextInfo: stamp(nil),
			},
		}
	}
	return m
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
