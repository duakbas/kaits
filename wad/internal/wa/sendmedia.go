package wa

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"google.golang.org/protobuf/proto"
)

// Sending attachments.
//
// The app hands over base64 bytes rather than a URL, because on the phone the
// bytes come from a Web Activity (the Camera or Gallery app returning a blob),
// not from anywhere the daemon could fetch. Upload encrypts and stores them on
// WhatsApp's CDN, then the message carries the keys — the same ones we persist
// on the way in so media survives a restart.

// maxUploadBytes caps what we'll accept in one frame. The bytes arrive base64'd
// over the WebSocket, so they cost ~4/3 their size in memory on both sides, and
// a feature phone has little to spare.
const maxUploadBytes = 16 << 20 // 16 MiB

// SendMedia uploads an attachment and sends it. kind is "image", "video",
// "audio" or "doc"; b64 is the raw file, caption is optional (ignored for
// audio, which WhatsApp doesn't caption). filename is used for documents.
func (c *Client) SendMedia(ctx context.Context, chatJID, kind, b64, mime, caption, filename, quotedID string) (string, error) {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return "", err
	}
	// Browsers hand over data: URLs; keep only the payload.
	if i := strings.Index(b64, ","); i >= 0 && strings.HasPrefix(b64, "data:") {
		b64 = b64[i+1:]
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", fmt.Errorf("decode media: %w", err)
	}
	if len(data) == 0 {
		return "", fmt.Errorf("empty media")
	}
	if len(data) > maxUploadBytes {
		return "", fmt.Errorf("attachment is %d MiB; the limit is %d MiB",
			len(data)>>20, maxUploadBytes>>20)
	}

	mediaType, ok := uploadTypeFor(kind)
	if !ok {
		return "", fmt.Errorf("cannot send media of kind %q", kind)
	}
	up, err := c.WA.Upload(ctx, data, mediaType)
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	if mime == "" {
		mime = defaultMimeFor(kind)
	}

	msg := buildMediaMessage(kind, up, mime, caption, filename, uint64(len(data)))
	if msg == nil {
		return "", fmt.Errorf("cannot build a %q message", kind)
	}
	// Quoting works the same as for text: the reply context rides on the
	// media message's ContextInfo.
	if quotedID != "" {
		if sender, chat, quoted, found := c.lookupMessage(quotedID); found {
			_ = chat
			attachQuote(msg, quotedID, sender, quoted)
		}
	}

	resp, err := c.WA.SendMessage(ctx, jid, msg)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// uploadTypeFor maps our kind names onto whatsmeow's media types.
func uploadTypeFor(kind string) (whatsmeow.MediaType, bool) {
	switch kind {
	case "image":
		return whatsmeow.MediaImage, true
	case "video", "gif":
		return whatsmeow.MediaVideo, true
	case "audio":
		return whatsmeow.MediaAudio, true
	case "doc":
		return whatsmeow.MediaDocument, true
	}
	return "", false
}

func defaultMimeFor(kind string) string {
	switch kind {
	case "image":
		return "image/jpeg"
	case "video", "gif":
		return "video/mp4"
	case "audio":
		return "audio/ogg; codecs=opus"
	}
	return "application/octet-stream"
}

// buildMediaMessage assembles the protobuf for an uploaded attachment.
func buildMediaMessage(kind string, up whatsmeow.UploadResponse, mime, caption, filename string, size uint64) *waE2E.Message {
	switch kind {
	case "image":
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Caption: strOrNil(caption), Mimetype: proto.String(mime),
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: proto.Uint64(size),
		}}
	case "video", "gif":
		vm := &waE2E.VideoMessage{
			Caption: strOrNil(caption), Mimetype: proto.String(mime),
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: proto.Uint64(size),
		}
		if kind == "gif" {
			vm.GifPlayback = proto.Bool(true)
		}
		return &waE2E.Message{VideoMessage: vm}
	case "audio":
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			Mimetype: proto.String(mime),
			URL:      proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: proto.Uint64(size),
			// Sent as a voice note rather than a music file, which is what a
			// recording from the phone should be.
			PTT: proto.Bool(true),
		}}
	case "doc":
		if filename == "" {
			filename = "file"
		}
		return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			Title: proto.String(filename), FileName: proto.String(filename),
			Caption: strOrNil(caption), Mimetype: proto.String(mime),
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: proto.Uint64(size),
		}}
	}
	return nil
}

// attachQuote adds reply context to whichever media variant is set.
func attachQuote(m *waE2E.Message, quotedID string, sender types.JID, quoted *waE2E.Message) {
	ci := &waE2E.ContextInfo{
		StanzaID:      proto.String(quotedID),
		Participant:   proto.String(sender.String()),
		QuotedMessage: quoted,
	}
	switch {
	case m.GetImageMessage() != nil:
		m.ImageMessage.ContextInfo = ci
	case m.GetVideoMessage() != nil:
		m.VideoMessage.ContextInfo = ci
	case m.GetAudioMessage() != nil:
		m.AudioMessage.ContextInfo = ci
	case m.GetDocumentMessage() != nil:
		m.DocumentMessage.ContextInfo = ci
	}
}

// strOrNil keeps empty captions out of the protobuf rather than sending "".
func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return proto.String(s)
}
