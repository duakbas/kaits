package wa

import (
	"context"
	"log"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types/events"
)

// Favourite stickers, which really do sync.
//
// WhatsApp keeps them in app state, under the "favoriteSticker" index, and
// whatsmeow carries them here without knowing it: it has no specific handler
// for that index, but it emits a generic events.AppState for EVERY set
// mutation, carrying the index and the decoded action. The action is a
// StickerAction, and a StickerAction holds a direct path, a media key and the
// hashes — which is to say, everything needed to download the file.
//
// So a favourite is stored as an ordinary attachment under a synthetic message
// id, "fav:<sticker id>", in the same media_keys table every received
// attachment uses. That is the whole trick: /media/fav:… serves it, the sticker
// picker lists it and SendStickerByID sends it, with no new download path, no
// new serving path and no new send path. A favourite is a message that never
// arrived.

// favPrefix marks the synthetic message ids that belong to favourites rather
// than to a real message. Deliberately not a valid WhatsApp message id.
const favPrefix = "fav:"

// IsFavouriteID reports whether an id refers to a favourite sticker.
func IsFavouriteID(id string) bool { return strings.HasPrefix(id, favPrefix) }

// handleAppState picks the favourite-sticker mutations out of the app state
// stream and ignores the rest, which whatsmeow already handles or nobody needs.
func (c *Client) handleAppState(v *events.AppState) {
	if v == nil || len(v.Index) < 2 || v.Index[0] != "favoriteSticker" {
		return
	}
	act := v.GetStickerAction()
	if act == nil {
		return
	}
	id := v.Index[1]
	if id == "" {
		return
	}

	// isFavorite false is an unfavourite: the sticker is still a sticker, it is
	// simply no longer in the list. Dropping the row rather than keeping a
	// flag, because a picker that shows things you have removed is a picker
	// that lies.
	if !act.GetIsFavorite() {
		c.hist.deleteFavouriteSticker(id)
		return
	}
	if act.GetDirectPath() == "" || len(act.GetMediaKey()) == 0 {
		// Nothing downloadable. Storing it would produce a grid cell that never
		// loads, which is worse than one that is not there.
		return
	}

	mime := act.GetMimetype()
	if mime == "" {
		mime = "image/webp"
	}
	c.hist.putMediaRef(favPrefix+id, mediaRef{
		directPath: act.GetDirectPath(),
		encSHA256:  act.GetFileEncSHA256(),
		// No plaintext hash in a StickerAction — only the encrypted one. The
		// download path treats an absent hash as "do not verify" rather than
		// as a hash of zeroes, which is what makes this work at all.
		sha256:   nil,
		mediaKey: act.GetMediaKey(),
		// Stickers ride on the image media type; only the message differs.
		mediaType: "image",
		mime:      mime,
	})
	c.hist.putFavouriteSticker(id, v.GetTimestamp())
}

// SyncFavouriteStickers asks WhatsApp to re-send the app state patch that
// carries favourites.
//
// Without this only CHANGES arrive: the daemon learns about a sticker the
// moment you favourite it on the phone, and knows nothing about the ones you
// favourited before it was ever paired. The patch is small and this runs once
// on connect, so the cost is a request and the benefit is a picker that is not
// empty on day one.
func (c *Client) SyncFavouriteStickers(ctx context.Context) {
	if c.WA == nil {
		return
	}
	// regular_high is the patch favourites live in, alongside mutes and stars.
	// A full sync rather than an incremental one: incremental gives us what has
	// changed since a version we may never have had.
	if err := c.WA.FetchAppState(ctx, appstate.WAPatchRegularHigh, true, false); err != nil {
		log.Printf("favourite stickers: app state fetch: %v", err)
	}
}

// ---- storage ----

func (h *histStore) putFavouriteSticker(id string, tsMillis int64) {
	if h == nil || h.db == nil || id == "" {
		return
	}
	ts := tsMillis / 1000
	if ts <= 0 {
		ts = time.Now().Unix()
	}
	h.db.Exec(`INSERT INTO favorite_stickers (id, ts) VALUES (?,?)
		ON CONFLICT(id) DO UPDATE SET ts=excluded.ts`, id, ts)
}

func (h *histStore) deleteFavouriteSticker(id string) {
	if h == nil || h.db == nil || id == "" {
		return
	}
	h.db.Exec(`DELETE FROM favorite_stickers WHERE id = ?`, id)
	h.db.Exec(`DELETE FROM media_keys WHERE msgid = ?`, favPrefix+id)
}

// favouriteStickers lists them newest first, as picker entries shaped exactly
// like the ones built from message history.
func (h *histStore) favouriteStickers(limit int) []stickerEntry {
	if h == nil || h.db == nil {
		return nil
	}
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	rows, err := h.db.Query(`SELECT id, ts FROM favorite_stickers ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []stickerEntry
	for rows.Next() {
		var id string
		var ts int64
		if err := rows.Scan(&id, &ts); err != nil {
			continue
		}
		out = append(out, stickerEntry{
			MsgID:     favPrefix + id,
			MediaURL:  "/media/" + favPrefix + id,
			Timestamp: ts,
			Favourite: true,
		})
	}
	return out
}

// stickerEntry is one cell of the picker. Deliberately not ws.MsgData: a
// favourite is not a message, has no chat and no sender, and a shape carrying
// fifteen fields that are always empty invites someone to read one.
type stickerEntry struct {
	MsgID     string `json:"msgid"`
	MediaURL  string `json:"media"`
	Timestamp int64  `json:"ts"`
	Favourite bool   `json:"fav,omitempty"`
}
