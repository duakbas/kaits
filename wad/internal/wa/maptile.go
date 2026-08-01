package wa

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"
)

// Map previews for locations that arrive without one.
//
// WhatsApp ships a rendered JPEG inside a location message it sent, and those
// are stored and served as-is. A location WE send has no such thumbnail, and
// neither does one from a client that omitted it — so the bubble was a bare
// pin and a pair of coordinates, which tells you nothing about where the place
// actually is.
//
// The tile is fetched by the DAEMON, not the phone. One machine asking, with a
// cache and an honest User-Agent, is a fraction of the requests a phone would
// make and is far easier to keep within OpenStreetMap's tile policy. It also
// keeps the phone talking only to the daemon, which is the whole shape of this
// project.
//
// NOTE ON POLICY: tile.openstreetmap.org is run on donated resources. This is
// a personal client fetching an occasional tile and caching it; that is within
// what the policy contemplates. Anything that starts fetching tiles in bulk —
// prefetching a map, scrolling — would not be, and should move to a paid tile
// provider instead.

const (
	tileZoom     = 15 // street level: a tile covers roughly a square kilometre
	tileCacheMax = 200
	tileTimeout  = 8 * time.Second
)

type tileCache struct {
	mu    sync.Mutex
	m     map[string][]byte
	order []string
}

func newTileCache() *tileCache { return &tileCache{m: map[string][]byte{}} }

func (t *tileCache) get(k string) ([]byte, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, ok := t.m[k]
	return b, ok
}

func (t *tileCache) put(k string, b []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, seen := t.m[k]; !seen {
		t.order = append(t.order, k)
		for len(t.order) > tileCacheMax {
			delete(t.m, t.order[0])
			t.order = t.order[1:]
		}
	}
	t.m[k] = b
}

// tileXY converts coordinates to a slippy-map tile index, and also reports
// where inside that tile the point falls (0..1 on each axis) so the caller can
// draw a marker in the right place rather than assuming the centre.
func tileXY(lat, lon float64, zoom int) (x, y int, fx, fy float64) {
	n := math.Exp2(float64(zoom))
	latRad := lat * math.Pi / 180
	fxAll := (lon + 180) / 360 * n
	fyAll := (1 - math.Log(math.Tan(latRad)+1/math.Cos(latRad))/math.Pi) / 2 * n
	x, y = int(math.Floor(fxAll)), int(math.Floor(fyAll))
	return x, y, fxAll - math.Floor(fxAll), fyAll - math.Floor(fyAll)
}

// MapTile returns a PNG map tile containing the point, and the fractional
// position of the point within it.
func (c *Client) MapTile(lat, lon float64) ([]byte, float64, float64, error) {
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return nil, 0, 0, fmt.Errorf("coordinates out of range")
	}
	x, y, fx, fy := tileXY(lat, lon, tileZoom)
	key := fmt.Sprintf("%d/%d/%d", tileZoom, x, y)
	if b, ok := c.tiles.get(key); ok {
		return b, fx, fy, nil
	}

	url := fmt.Sprintf("https://tile.openstreetmap.org/%s.png", key)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	// An identifying User-Agent is required by the tile policy, and a generic
	// one is the fastest way to get blocked.
	req.Header.Set("User-Agent", "kaits/1.0 (self-hosted personal chat client)")

	resp, err := (&http.Client{Timeout: tileTimeout}).Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, 0, fmt.Errorf("tile server said %s", resp.Status)
	}
	// A tile is ~20 KB. Anything wildly larger is not a tile.
	b, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return nil, 0, 0, err
	}
	c.tiles.put(key, b)
	return b, fx, fy, nil
}
