package wa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// GIF search, through GIPHY.
//
// This was written against Tenor first, which is where WhatsApp's own GIF
// button goes — and Tenor's API was decommissioned on 30 June 2026. Google
// stopped issuing keys in January and shut the service down in June, so there
// is no key to get and nothing at the other end. GIPHY is the remaining
// provider with a free developer tier.
//
// Three things about this are deliberate.
//
// It needs a key you supply, in WAD_GIPHY_KEY. There is no anonymous tier, and
// an app-embedded key would be a key in a zip on a submission portal. Without
// one the picker says so rather than sitting there empty.
//
// The phone never talks to GIPHY. Previews are proxied through the daemon, so
// the app makes requests to exactly one host — which matters on a 2016 browser
// whose root certificates and TLS support are whatever they were in 2016, and
// which keeps the phone's browsing of a GIF search off a third party's logs.
//
// And what gets SENT is the MP4, not the GIF. WhatsApp has no GIF message type:
// its "GIFs" are H.264 video with a gifPlayback flag, and an actual GIF file
// arrives as a document or a still. GIPHY publishes both, so we take the one
// WhatsApp wants.

const (
	giphyTimeout = 15 * time.Second
	// giphyMaxBytes caps a proxied preview. A 100px-wide GIF is tens of
	// kilobytes; anything past this is not a preview.
	giphyMaxBytes = 4 << 20
)

// GIFSearchEnabled reports whether GIF search can work at all.
func GIFSearchEnabled() bool { return giphyKey() != "" }

func giphyKey() string { return os.Getenv("WAD_GIPHY_KEY") }

// GIFResult is one row of a search, in the shape the picker consumes.
type GIFResult struct {
	ID string `json:"id"`
	// Preview is a daemon URL, not a GIPHY one — see the note above.
	Preview string `json:"preview"`
	// Send is the MP4 url, handed back verbatim when the user picks one.
	Send string `json:"send"`
	Desc string `json:"desc,omitempty"`
}

var gifHTTP = &http.Client{Timeout: giphyTimeout}

// giphyImage is one rendition. GIPHY gives a GIF and often an MP4 for the same
// size under one key, which is exactly the pair this needs.
type giphyImage struct {
	URL string `json:"url"`
	MP4 string `json:"mp4"`
}

// SearchGIFs runs a search, or returns what is trending when the query is empty
// — opening a picker onto nothing is a worse first impression than opening it
// onto whatever everyone else is sending.
func (c *Client) SearchGIFs(ctx context.Context, query string, limit int) ([]GIFResult, error) {
	key := giphyKey()
	if key == "" {
		return nil, fmt.Errorf("GIF search is off: set WAD_GIPHY_KEY on the daemon")
	}
	if limit <= 0 || limit > 50 {
		limit = 24
	}

	endpoint := "https://api.giphy.com/v1/gifs/trending"
	q := url.Values{}
	if strings.TrimSpace(query) != "" {
		endpoint = "https://api.giphy.com/v1/gifs/search"
		q.Set("q", query)
	}
	q.Set("api_key", key)
	q.Set("limit", strconv.Itoa(limit))
	q.Set("rating", "pg-13")
	q.Set("bundle", "messaging_non_clips")

	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := gifHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("giphy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("giphy returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Data []struct {
			ID     string                `json:"id"`
			Title  string                `json:"title"`
			Images map[string]giphyImage `json:"images"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("giphy: %w", err)
	}

	out := make([]GIFResult, 0, len(parsed.Data))
	for _, g := range parsed.Data {
		preview := pickFirst(g.Images, false, "fixed_width_small", "fixed_width_downsampled", "fixed_width")
		send := pickFirst(g.Images, true, "downsized_small", "fixed_width", "original")
		// Both halves are needed: one to show it, one to send it. A row missing
		// either is a cell that cannot be used for anything.
		if preview == "" || send == "" {
			continue
		}
		out = append(out, GIFResult{
			ID:      g.ID,
			Preview: "/gifproxy?u=" + url.QueryEscape(preview),
			Send:    send,
			Desc:    g.Title,
		})
	}
	return out, nil
}

// pickFirst returns the first rendition that has the field we need, in
// preference order. Renditions come and go and a missing one is normal, so the
// list is a fallback chain rather than a lookup.
func pickFirst(images map[string]giphyImage, wantMP4 bool, names ...string) string {
	for _, n := range names {
		img := images[n]
		if wantMP4 && img.MP4 != "" {
			return img.MP4
		}
		if !wantMP4 && img.URL != "" {
			return img.URL
		}
	}
	return ""
}

// gifHost reports whether a URL may be fetched on the app's behalf.
//
// This is the check that keeps /gifproxy from being an open proxy: without it,
// anyone holding the token could ask the daemon to fetch anything at all, from
// inside whatever network the daemon is on.
//
// The leading dot in the suffix matters. "giphy.com" alone would also accept
// "notgiphy.com", and matching without anchoring the end would accept
// "giphy.com.example.net".
func gifHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "giphy.com" || strings.HasSuffix(h, ".giphy.com")
}

// FetchGIFPreview proxies one preview image.
func FetchGIFPreview(ctx context.Context, raw string) ([]byte, string, error) {
	if !gifHost(raw) {
		return nil, "", fmt.Errorf("refusing to fetch %q: not a GIPHY url", raw)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := gifHTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("preview returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, giphyMaxBytes))
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/gif"
	}
	return data, ct, nil
}

// SendGIFByURL fetches the MP4 and sends it as WhatsApp sends a GIF: video with
// the gifPlayback flag, which is what makes it loop silently rather than arrive
// as a one-second film.
func (c *Client) SendGIFByURL(ctx context.Context, chatJID, raw, quotedID string) (string, error) {
	if !gifHost(raw) {
		return "", fmt.Errorf("that is not a GIPHY url")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return "", err
	}
	resp, err := gifHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch gif: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch gif: returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxUploadBytes))
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("that GIF is empty")
	}
	return c.SendMedia(ctx, chatJID, "gif",
		base64.StdEncoding.EncodeToString(data), "video/mp4", "", "", quotedID)
}
