package wa

import "testing"

// The proxy takes a url from the phone and fetches it. Without a host check
// that is an open proxy into whatever network the daemon sits in — a VPS with
// a cloud metadata service on a link-local address, say, or a home network
// behind the machine running this. The check is the whole security boundary,
// so it gets tested like one.
func TestTenorHostRefusesEverythingElse(t *testing.T) {
	allowed := []string{
		"https://media.tenor.com/abc/tenor.gif",
		"https://media1.tenor.com/x/y.mp4",
		"https://tenor.com/view/thing",
		"https://tenor.googleapis.com/v2/search?q=cat",
	}
	for _, u := range allowed {
		if !tenorHost(u) {
			t.Errorf("refused a legitimate Tenor url: %s", u)
		}
	}

	refused := []string{
		// The classic targets of a server-side request forgery.
		"http://169.254.169.254/latest/meta-data/",
		"https://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:8080/ws?token=guess",
		"https://192.168.1.1/admin",
		"file:///opt/wad/wa-session.db",
		// Plain http, even to Tenor: the point of proxying is that the phone
		// need not trust the network, and neither should we.
		"http://media.tenor.com/abc/tenor.gif",
		// Hosts that merely LOOK like Tenor. Suffix matching without the dot is
		// how "nottenor.com" and "tenor.com.evil.net" get through.
		"https://nottenor.com/x.gif",
		"https://tenor.com.evil.net/x.gif",
		"https://evil.net/?x=tenor.com",
		"https://googleapis.com.evil.net/x",
		"",
		"not a url at all",
	}
	for _, u := range refused {
		if tenorHost(u) {
			t.Errorf("ALLOWED a url that is not Tenor: %s", u)
		}
	}
}

// With no key the feature has to be off rather than broken: a picker that
// opens onto an error is worse than a menu item that is not there.
func TestTenorOffWithoutAKey(t *testing.T) {
	t.Setenv("WAD_TENOR_KEY", "")
	if TenorEnabled() {
		t.Error("GIF search reported as available with no key set")
	}
	c := &Client{}
	if _, err := c.SearchGIFs(t.Context(), "cat", 5); err == nil {
		t.Error("searching with no key did not report why it cannot work")
	}
	t.Setenv("WAD_TENOR_KEY", "x")
	if !TenorEnabled() {
		t.Error("GIF search reported as unavailable with a key set")
	}
}
