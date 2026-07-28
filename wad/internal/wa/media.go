package wa

import (
	"context"
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
	data  []byte
	mime  string
	none  bool // true = looked up, no photo (don't retry constantly)
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

// download fetches and decrypts the bytes for a stored media message.
func (c *Client) downloadMedia(ctx context.Context, id string) ([]byte, error) {
	dl, ok := c.media.get(id)
	if !ok {
		return nil, fmt.Errorf("media %s not in cache (evicted or never seen)", id)
	}
	return c.WA.Download(ctx, dl)
}

