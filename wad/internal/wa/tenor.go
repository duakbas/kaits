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

// GIF search, through Tenor — which is where WhatsApp's own GIF button goes.
//
// Three things about this are deliberate.
//
// It needs a key you supply, in WAD_TENOR_KEY. Tenor's API is Google's and
// wants one; there is no anonymous tier, and an app-embedded key would be a key
// in a zip on a submission portal. Without it the GIF button is simply absent
// rather than present and broken.
//
// The phone never talks to Tenor. Previews are proxied through the daemon, so
// the app makes requests to exactly one host — which matters on a 2016 browser
// whose root certificates and TLS support are whatever they were in 2016, and
// which keeps the phone's data going to one place.
//
// And what gets SENT is the MP4, not the GIF. WhatsApp's own "GIFs" are H.264
// video with a gifPlayback flag; sending an actual GIF produces a document or a
// still, depending on the client. Tenor publishes both, so we take the one
// WhatsApp wants.

const (
	tenorTimeout = 15 * time.Second
	// tenorMaxBytes caps a proxied preview. A tinygif is tens of kilobytes;
	// anything past this is not a preview and not something to hand a phone.
	tenorMaxBytes = 4 << 20
)

// TenorEnabled reports whether GIF search can work at all.
func TenorEnabled() bool { return os.Getenv("WAD_TENOR_KEY") != "" }

// GIFResult is one row of a search, in the shape the picker consumes.
type GIFResult struct {
	ID string `json:"id"`
	// Preview is a daemon URL, not a Tenor one — see the note above.
	Preview string `json:"preview"`
	// Send is the Tenor MP4 url, handed back verbatim when the user picks one.
	Send string `json:"send"`
	Desc string `json:"desc,omitempty"`
}

var tenorHTTP = &http.Client{Timeout: tenorTimeout}

// SearchGIFs runs a Tenor search, or returns the featured list when the query
// is empty — opening a picker onto nothing is a worse first impression than
// opening it onto whatever is popular.
func (c *Client) SearchGIFs(ctx context.Context, query string, limit int) ([]GIFResult, error) {
	key := os.Getenv("WAD_TENOR_KEY")
	if key == "" {
		return nil, fmt.Errorf("GIF search is off: set WAD_TENOR_KEY on the daemon")
	}
	if limit <= 0 || limit > 50 {
		limit = 24
	}

	endpoint := "https://tenor.googleapis.com/v2/featured"
	q := url.Values{}
	if strings.TrimSpace(query) != "" {
		endpoint = "https://tenor.googleapis.com/v2/search"
		q.Set("q", query)
	}
	q.Set("key", key)
	q.Set("client_key", "wad")
	q.Set("limit", strconv.Itoa(limit))
	// tinygif is the preview a 240px screen wants; tinymp4 is what gets sent.
	// Asking for only these two keeps the response small.
	q.Set("media_filter", "tinygif,tinymp4,mp4")
	q.Set("contentfilter", "medium")

	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := tenorHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tenor: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("tenor returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Results []struct {
			ID           string `json:"id"`
			ContentDesc  string `json:"content_description"`
			MediaFormats map[string]struct {
				URL string `json:"url"`
			} `json:"media_formats"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("tenor: %w", err)
	}

	out := make([]GIFResult, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		preview := r.MediaFormats["tinygif"].URL
		send := r.MediaFormats["tinymp4"].URL
		if send == "" {
			send = r.MediaFormats["mp4"].URL
		}
		// Both halves are needed: one to show it, one to send it. A row missing
		// either is a cell that cannot be used for anything.
		if preview == "" || send == "" {
			continue
		}
		out = append(out, GIFResult{
			ID:      r.ID,
			Preview: "/gifproxy?u=" + url.QueryEscape(preview),
			Send:    send,
			Desc:    r.ContentDesc,
		})
	}
	return out, nil
}

// tenorHost reports whether a URL may be fetched on the app's behalf.
//
// This is the check that keeps /gifproxy from being an open proxy: without it,
// anyone holding the token could ask the daemon to fetch anything at all, from
// inside whatever network the daemon is on.
func tenorHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "tenor.com" ||
		strings.HasSuffix(h, ".tenor.com") ||
		strings.HasSuffix(h, ".googleapis.com")
}

// FetchGIFPreview proxies one preview image.
func FetchGIFPreview(ctx context.Context, raw string) ([]byte, string, error) {
	if !tenorHost(raw) {
		return nil, "", fmt.Errorf("refusing to fetch %q: not a Tenor url", raw)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := tenorHTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("preview returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, tenorMaxBytes))
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/gif"
	}
	return data, ct, nil
}

// SendGIFByURL fetches a Tenor MP4 and sends it as WhatsApp sends a GIF: video
// with the gifPlayback flag, which is what makes it loop silently rather than
// arrive as a one-second film.
func (c *Client) SendGIFByURL(ctx context.Context, chatJID, raw, quotedID string) (string, error) {
	if !tenorHost(raw) {
		return "", fmt.Errorf("that is not a Tenor url")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return "", err
	}
	resp, err := tenorHTTP.Do(req)
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
