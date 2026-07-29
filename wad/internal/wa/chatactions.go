package wa

import (
	"context"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/types"

	"google.golang.org/protobuf/proto"
)

// Chat-level actions: pin, mute, archive, delete.
//
// These are the one place the daemon WRITES to the WhatsApp account rather than
// just reading it. Each goes out as an app-state patch (SendAppState), the same
// mechanism the official clients use, so the change shows up on the phone and
// on every other linked device.
//
// Two consequences worth knowing:
//   - They are not local preferences. Muting here mutes everywhere.
//   - Delete is a real WhatsApp chat delete and is not undoable from here.
//
// Our own db is updated only after WhatsApp accepts the patch, so a rejected
// write can't leave the app showing a state the account doesn't have.

// muteForever is far enough out to read as permanent, which is what "mute" in
// the app's menu means; WhatsApp itself stores mute as an end timestamp.
const muteForever = 100 * 365 * 24 * time.Hour

// messageAnchor builds the "last message" reference that archive and delete
// patches carry. Both accept a zero value, so a chat with no stored messages
// still works — the patch just isn't anchored.
func (c *Client) messageAnchor(chatJID string, chat types.JID) (time.Time, *waCommon.MessageKey) {
	msgid, ts, fromMe, sender := c.hist.lastMessage(chatJID)
	if msgid == "" || ts == 0 {
		return time.Time{}, nil
	}
	key := &waCommon.MessageKey{
		RemoteJID: proto.String(chat.String()),
		FromMe:    proto.Bool(fromMe),
		ID:        proto.String(msgid),
	}
	// Participant is only meaningful for group messages from someone else.
	if chat.Server == types.GroupServer && !fromMe && sender != "" {
		key.Participant = proto.String(sender)
	}
	return time.Unix(ts, 0), key
}

// SetPin pins or unpins a chat for the whole account.
func (c *Client) SetPin(ctx context.Context, chatJID string, pin bool) error {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}
	if err := c.WA.SendAppState(ctx, appstate.BuildPin(jid, pin)); err != nil {
		return fmt.Errorf("pin: %w", err)
	}
	c.hist.setChatFlag(chatJID, "pinned", pin)
	return nil
}

// SetMute mutes or unmutes a chat. A zero duration means "until unmuted".
func (c *Client) SetMute(ctx context.Context, chatJID string, mute bool, d time.Duration) error {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}
	if mute && d <= 0 {
		d = muteForever
	}
	if err := c.WA.SendAppState(ctx, appstate.BuildMute(jid, mute, d)); err != nil {
		return fmt.Errorf("mute: %w", err)
	}
	c.hist.setChatFlag(chatJID, "muted", mute)
	return nil
}

// SetArchive archives or unarchives a chat. WhatsApp unpins a chat as a side
// effect of archiving it, so we mirror that locally rather than letting the app
// show a pinned-and-archived chat that the account disagrees with.
func (c *Client) SetArchive(ctx context.Context, chatJID string, archive bool) error {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}
	ts, key := c.messageAnchor(chatJID, jid)
	if err := c.WA.SendAppState(ctx, appstate.BuildArchive(jid, archive, ts, key)); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	c.hist.setChatFlag(chatJID, "archived", archive)
	if archive {
		c.hist.setChatFlag(chatJID, "pinned", false)
	}
	return nil
}

// DeleteChat deletes a chat from the WhatsApp account, then drops our stored
// copy. Media is left on disk (deleteMedia=false) — we only cache it.
func (c *Client) DeleteChat(ctx context.Context, chatJID string) error {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}
	ts, key := c.messageAnchor(chatJID, jid)
	if err := c.WA.SendAppState(ctx, appstate.BuildDeleteChat(jid, ts, key, false)); err != nil {
		return fmt.Errorf("delete chat: %w", err)
	}
	c.hist.dropChat(chatJID)
	return nil
}

// ChatFlags reports the current pin/mute/archive state, preferring the
// account's synced view and falling back to what we stored.
func (c *Client) ChatFlags(chatJID string) (pinned, muted, archived bool) {
	pinned, muted, archived = c.hist.chatFlags(chatJID)
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return
	}
	s, err := c.WA.Store.ChatSettings.GetChatSettings(context.Background(), jid)
	if err != nil || !s.Found {
		return
	}
	return s.Pinned, s.MutedUntil.After(time.Now()), s.Archived
}
