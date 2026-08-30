package wa

import "testing"

// The proxy takes a url from the phone and fetches it. Without a host check
// that is an open proxy into whatever network the daemon sits in — a VPS with
// a cloud metadata service on a link-local address, say, or a home network
// behind the machine running this. The check is the whole security boundary,
// so it gets tested like one.
func TestGIFHostRefusesEverythingElse(t *testing.T) {
	allowed := []string{
		"https://api.giphy.com/v1/gifs/search?q=cat",
		"https://media0.giphy.com/media/abc/100w.gif",
		"https://media.giphy.com/media/abc/giphy.mp4",
		"https://i.giphy.com/abc.gif",
		"https://giphy.com/gifs/abc",
	}
	for _, u := range allowed {
		if !gifHost(u) {
			t.Errorf("refused a legitimate GIPHY url: %s", u)
		}
	}

	refused := []string{
		// The classic targets of a server-side request forgery.
		"http://169.254.169.254/latest/meta-data/",
		"https://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:8080/ws?token=guess",
		"https://192.168.1.1/admin",
		"file:///opt/wad/wa-session.db",
		// Plain http, even to GIPHY: the point of proxying is that the phone
		// need not trust the network, and neither should we.
		"http://media.giphy.com/media/abc/100w.gif",
		// Hosts that merely LOOK right. Suffix matching without the leading dot
		// is how "notgiphy.com" gets through; matching without anchoring the
		// end is how "giphy.com.evil.net" does.
		"https://notgiphy.com/x.gif",
		"https://giphy.com.evil.net/x.gif",
		"https://evil.net/?x=giphy.com",
		// Tenor, which is dead, and was never ours to fetch from anyway.
		"https://media.tenor.com/abc/tenor.gif",
		"",
		"not a url at all",
	}
	for _, u := range refused {
		if gifHost(u) {
			t.Errorf("ALLOWED a url that is not GIPHY: %s", u)
		}
	}
}

// With no key the feature has to be off rather than broken: a picker that
// opens onto an error is worse than a menu item that is not there.
func TestGIFSearchOffWithoutAKey(t *testing.T) {
	t.Setenv("WAD_GIPHY_KEY", "")
	if GIFSearchEnabled() {
		t.Error("GIF search reported as available with no key set")
	}
	c := &Client{}
	if _, err := c.SearchGIFs(t.Context(), "cat", 5); err == nil {
		t.Error("searching with no key did not report why it cannot work")
	}
	t.Setenv("WAD_GIPHY_KEY", "x")
	if !GIFSearchEnabled() {
		t.Error("GIF search reported as unavailable with a key set")
	}
}

// Renditions come and go, and GIPHY does not promise any particular one. A
// result is only usable when it has BOTH a preview to show and an MP4 to send,
// so the fallback chain has to find each independently — and a result with
// neither must be dropped rather than rendered as a blank cell.
func TestPickFirstWalksTheFallbackChain(t *testing.T) {
	images := map[string]giphyImage{
		// No fixed_width_small at all: the preview has to fall through.
		"fixed_width": {URL: "https://media.giphy.com/w.gif", MP4: "https://media.giphy.com/w.mp4"},
		"original":    {URL: "https://media.giphy.com/o.gif", MP4: "https://media.giphy.com/o.mp4"},
	}
	if got := pickFirst(images, false, "fixed_width_small", "fixed_width"); got != "https://media.giphy.com/w.gif" {
		t.Errorf("preview = %q, want the fixed_width gif", got)
	}
	// downsized_small is missing, and fixed_width has an mp4 — so the chain
	// must not stop at the first NAME, only at the first name that has the
	// field being asked for.
	if got := pickFirst(images, true, "downsized_small", "fixed_width", "original"); got != "https://media.giphy.com/w.mp4" {
		t.Errorf("send = %q, want the fixed_width mp4", got)
	}

	// A rendition present but with an empty field is the same as absent.
	sparse := map[string]giphyImage{
		"downsized_small": {MP4: ""},
		"original":        {MP4: "https://media.giphy.com/o.mp4"},
	}
	if got := pickFirst(sparse, true, "downsized_small", "original"); got != "https://media.giphy.com/o.mp4" {
		t.Errorf("send = %q, want the original mp4", got)
	}
	if got := pickFirst(nil, false, "fixed_width"); got != "" {
		t.Errorf("got %q from no images at all", got)
	}
}
