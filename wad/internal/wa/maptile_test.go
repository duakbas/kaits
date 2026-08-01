package wa

import (
	"math"
	"testing"
)

// The tile maths decides where the pin lands. Getting it wrong puts the marker
// somewhere that is not the place, which is worse than no marker at all.
func TestTileXY(t *testing.T) {
	// Known slippy-map values at zoom 15.
	cases := []struct {
		name     string
		lat, lon float64
		x, y     int
	}{
		// Cross-checked against an independent implementation of the standard
		// slippy-map formula rather than worked out by hand — the hand-worked
		// versions of these were all wrong, and a wrong expectation here would
		// have "proved" a correct implementation broken.
		{"Greenwich", 51.4779, -0.0015, 16383, 10900},
		{"Istanbul", 41.0082, 28.9784, 19021, 12284},
		{"Sydney", -33.8688, 151.2093, 30147, 19663},
	}
	for _, c := range cases {
		x, y, fx, fy := tileXY(c.lat, c.lon, 15)
		if x != c.x || y != c.y {
			t.Errorf("%s: tile = %d,%d want %d,%d", c.name, x, y, c.x, c.y)
		}
		if fx < 0 || fx >= 1 || fy < 0 || fy >= 1 {
			t.Errorf("%s: fraction %f,%f outside the tile", c.name, fx, fy)
		}
	}

	// Moving east must never move the pin west.
	_, _, fxA, _ := tileXY(41.0082, 28.9784, 15)
	xA, _, _, _ := tileXY(41.0082, 28.9784, 15)
	xB, _, fxB, _ := tileXY(41.0082, 28.9790, 15)
	if xB == xA && fxB <= fxA {
		t.Errorf("moving east did not move the pin east: %f -> %f", fxA, fxB)
	}
}

// The cache must not grow without bound — this runs on a machine that may be a
// laptop, and a tile is 20 KB.
func TestTileCacheEvicts(t *testing.T) {
	c := newTileCache()
	for i := 0; i < tileCacheMax+50; i++ {
		c.put(string(rune('a'+i%26))+string(rune(i)), []byte{1})
	}
	c.mu.Lock()
	n := len(c.m)
	c.mu.Unlock()
	if n > tileCacheMax {
		t.Errorf("cache holds %d entries, cap is %d", n, tileCacheMax)
	}
	_ = math.Pi
}
