package wa

import (
	"context"
	"log"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// Recovering media for messages we already have.
//
// An attachment can only be downloaded with its keys — direct path, media key,
// two hashes — which ride on the original message protobuf. Those are persisted
// now, but messages stored before that was true have nothing, so their photos
// are permanently un-fetchable from what's on disk. No repair pass can invent
// them.
//
// They can be asked for again, though. WhatsApp lets a linked device request
// history on demand: BuildHistorySyncRequest + SendPeerMessage asks the primary
// phone for the N messages immediately BEFORE a given one, and the answer
// arrives as an ordinary HistorySync event carrying full protobufs. That runs
// through the normal handleMsg path, so cacheMedia persists the keys as a side
// effect and the photos become viewable.
//
// Caveats worth knowing, because this depends on someone else's cooperation:
//   - the primary phone must be online and willing; it can simply not answer.
//   - responses are asynchronous, so this fires requests and waits rather than
//     confirming each one.
//   - it re-delivers messages we already have, which re-runs handleMsg on them.
//     That's safe (INSERT OR REPLACE, and names re-resolve with the current,
//     better resolver) but it does rewrite rows.
//   - requests are paced, since this is exactly the sort of chatter that gets an
//     unofficial client throttled.

const (
	// WhatsApp's recommended request size.
	refetchBatch = 50
	// Gap between requests. The phone answers asynchronously; this is about not
	// flooding it, not about waiting for replies.
	refetchPause = 3 * time.Second
)

// RefetchMediaKeys walks the chats that contain attachments with no stored keys
// and asks the primary device to re-send that history.
//
// maxRequests bounds the work — every request is a peer message and a round trip
// through the phone. Returns how many requests were sent and how many chats they
// covered. What actually lands arrives later as HistorySync events, so the
// caller has to stay connected and give it time.
func (c *Client) RefetchMediaKeys(ctx context.Context, maxRequests int) (sent, chats int) {
	gaps := c.hist.mediaMessagesWithoutKeys()
	if len(gaps) == 0 {
		log.Printf("wa: every stored attachment already has keys; nothing to refetch")
		return 0, 0
	}

	// Group by chat, keeping the newest-first order within each. Requests ask
	// for messages *before* an anchor, so walking newest to oldest and stepping
	// one batch at a time covers a chat's whole span.
	byChat := map[string][]mediaGap{}
	var order []string
	for _, g := range gaps {
		if _, seen := byChat[g.Chat]; !seen {
			order = append(order, g.Chat)
		}
		byChat[g.Chat] = append(byChat[g.Chat], g)
	}
	log.Printf("wa: %d attachments without keys across %d chats; requesting history (max %d requests)",
		len(gaps), len(order), maxRequests)

	for _, chatJID := range order {
		if sent >= maxRequests || ctx.Err() != nil {
			break
		}
		chat, err := types.ParseJID(chatJID)
		if err != nil {
			continue
		}
		list := byChat[chatJID]
		chats++

		// Anchor on every refetchBatch-th missing attachment. Each request
		// covers the 50 messages before its anchor, so this sweeps the region
		// the gaps live in without asking about the same span twice.
		for i := 0; i < len(list); i += refetchBatch {
			if sent >= maxRequests || ctx.Err() != nil {
				break
			}
			anchor := list[i]
			info := &types.MessageInfo{
				ID:        anchor.MsgID,
				Timestamp: time.Unix(anchor.TS, 0),
				MessageSource: types.MessageSource{
					Chat:     chat,
					IsFromMe: anchor.FromMe,
					IsGroup:  chat.Server == types.GroupServer,
				},
			}
			req := c.WA.BuildHistorySyncRequest(info, refetchBatch)
			if req == nil {
				continue
			}
			if _, err := c.WA.SendPeerMessage(ctx, req); err != nil {
				log.Printf("wa: history request for %s failed: %v", chatJID, err)
				// A refusal here usually means the phone or the link is
				// unhappy; pushing harder won't help.
				break
			}
			sent++
			if !sleepCtx(ctx, refetchPause) {
				return sent, chats
			}
		}
	}
	return sent, chats
}

// MediaKeyStats reports how many attachments are downloadable versus not, so the
// effect of a refetch is visible.
func (c *Client) MediaKeyStats() (withKeys, without int) {
	return c.hist.countStoredMediaKeys(), len(c.hist.mediaMessagesWithoutKeys())
}
