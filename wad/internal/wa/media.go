package wa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// replyContext is the minimal info needed to quote a message in a reply:
// the original message body, who sent it, and in which chat.
type replyContext struct {
	msg    *waE2E.Message
	sender types.JID
	chat   types.JID
}

// mediaStore remembers the downloadable part of each media message so the
// HTTP /media/<id> handler can fetch bytes on demand — long after the original
// event has been handled. whatsmeow's Download() needs the message object
// (it carries the media keys + direct path), so we stash the relevant part
// keyed by message ID when the message first arrives.
//
// This is in-memory and bounded: media the app never opens just sits until
// evicted. For a single-user phone that's fine; a cap keeps it from growing
// without limit over a long session.
type mediaStore struct {
	mu    sync.Mutex
	items map[string]whatsmeow.DownloadableMessage
	order []string // insertion order for simple FIFO eviction
	cap   int
}

func newMediaStore(capacity int) *mediaStore {
	return &mediaStore{
		items: make(map[string]whatsmeow.DownloadableMessage),
		cap:   capacity,
	}
}

func (s *mediaStore) put(id string, m whatsmeow.DownloadableMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.items[id]; !exists {
		s.order = append(s.order, id)
		// evict oldest if over cap
		for len(s.order) > s.cap {
			oldest := s.order[0]
			s.order = s.order[1:]
			delete(s.items, oldest)
		}
	}
	s.items[id] = m
}

func (s *mediaStore) get(id string) (whatsmeow.DownloadableMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.items[id]
	return m, ok
}

// AvatarResult is a cached avatar lookup: either image bytes or a "none" mark.
type avatarEntry struct {
	data []byte
	mime string
	none bool // true = looked up, no photo (don't retry constantly)
}

// AvatarFor resolves and fetches a JID's profile picture, caching the result
// (including "no photo"). Returns bytes+mime, or ok=false if there's no avatar.
func (c *Client) AvatarFor(ctx context.Context, jidStr string) ([]byte, string, bool) {
	c.avatarMu.Lock()
	if e, ok := c.avatars[jidStr]; ok {
		c.avatarMu.Unlock()
		if e.none {
			return nil, "", false
		}
		return e.data, e.mime, true
	}
	c.avatarMu.Unlock()

	jid, err := types.ParseJID(jidStr)
	if err != nil {
		return nil, "", false
	}
	info, err := c.WA.GetProfilePictureInfo(ctx, jid, nil)
	if err != nil || info == nil || info.URL == "" {
		c.avatarMu.Lock()
		c.avatars[jidStr] = avatarEntry{none: true}
		c.avatarMu.Unlock()
		return nil, "", false
	}

	// fetch the image bytes ourselves so the phone hits our endpoint, not the
	// WhatsApp CDN, and so we can cache.
	data, mime, ferr := fetchURL(ctx, info.URL)
	if ferr != nil {
		c.avatarMu.Lock()
		c.avatars[jidStr] = avatarEntry{none: true}
		c.avatarMu.Unlock()
		return nil, "", false
	}
	c.avatarMu.Lock()
	c.avatars[jidStr] = avatarEntry{data: data, mime: mime}
	c.avatarMu.Unlock()
	return data, mime, true
}

func fetchURL(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("avatar fetch status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "image/jpeg"
	}
	return data, mime, nil
}

// mmsTypeFor maps a media type to the MMS type string WhatsApp's CDN expects.
// whatsmeow keeps this mapping unexported, so it's repeated here — the values
// are part of the wire protocol and don't drift.
var mmsTypeFor = map[whatsmeow.MediaType]string{
	whatsmeow.MediaImage:    "image",
	whatsmeow.MediaVideo:    "video",
	whatsmeow.MediaAudio:    "audio",
	whatsmeow.MediaDocument: "document",
}

// downloadMedia fetches and decrypts the bytes for a media message.
//
// The in-memory cache holds the original protobuf and is the fast path, but it
// is bounded and dies with the process — which is why every photo used to break
// after a restart or a browser refresh. The keys needed to re-fetch from the CDN
// are persisted per message, so failing over to those makes media durable.
// errNoKeys is a permanent answer, not a transient one: a message stored
// before keys were kept can never be fetched, by anyone, ever. It is a distinct
// error so the HTTP layer can say "gone" rather than "not found" — and so the
// app can stop asking.
var errNoKeys = errors.New("no stored keys (predates key storage, or never had media)")

func (c *Client) downloadMedia(ctx context.Context, id string) ([]byte, error) {
	if dl, ok := c.media.get(id); ok {
		data, err := c.WA.Download(ctx, dl)
		if err == nil {
			return data, nil
		}
		// Fall through: a stale cached entry can still fail, and the stored keys
		// may well work.
	}

	ref, ok := c.hist.mediaRefFor(id)
	if !ok {
		return nil, errNoKeys
	}
	mediaType := whatsmeow.MediaType(ref.mediaType)
	mms, known := mmsTypeFor[mediaType]
	if !known {
		return nil, fmt.Errorf("media %s has unsupported type %q", id, ref.mediaType)
	}
	return c.WA.DownloadMediaWithPath(ctx, ref.directPath,
		ref.encSHA256, ref.sha256, ref.mediaKey, mediaType, mms, false)
}

// IsPermanentlyGone reports whether this media can never be fetched, as
// opposed to having failed this time.
func IsPermanentlyGone(err error) bool { return errors.Is(err, errNoKeys) }
