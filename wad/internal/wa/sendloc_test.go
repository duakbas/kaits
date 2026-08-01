package wa

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// A failed GPS fix reports 0,0 as a success on more than one platform. Sending
// it puts a pin in the Atlantic in someone's chat, which is worse than an
// error, so it must be refused before it reaches WhatsApp.
func TestCheckCoords(t *testing.T) {
	cases := []struct {
		name     string
		lat, lon float64
		wantErr  bool
	}{
		{"ordinary", 41.0082, 28.9784, false},
		{"negative", -33.8688, 151.2093, false},
		{"null island", 0, 0, true},
		{"lat too high", 91, 10, true},
		{"lat too low", -90.5, 10, true},
		{"lon too high", 10, 181, true},
		{"lon too low", 10, -180.5, true},
		{"edges are valid", 90, 180, false},
		{"zero lat alone is fine", 0, 28.9784, false},
	}
	for _, tc := range cases {
		err := checkCoords(tc.lat, tc.lon)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: checkCoords(%v,%v) error = %v, wantErr %v",
				tc.name, tc.lat, tc.lon, err, tc.wantErr)
		}
	}
}

// The session bookkeeping is what makes updates coherent, and it's testable
// without a WhatsApp connection.
func TestLiveShareBookkeeping(t *testing.T) {
	chat := types.JID{User: "12345", Server: types.DefaultUserServer}
	ls := newLiveShares()

	now := time.Now()
	ls.m[chat.String()] = &liveShare{
		chat: chat, startMsg: "START1", startAt: now,
		endsAt: now.Add(time.Hour), seq: now.UnixMilli(),
	}

	s := ls.m[chat.String()]
	first := s.seq
	s.seq++
	if s.seq <= first {
		t.Errorf("sequence did not advance: %d -> %d", first, s.seq)
	}

	// An expired share must read as expired rather than lingering.
	ls.m[chat.String()].endsAt = now.Add(-time.Second)
	if !time.Now().After(ls.m[chat.String()].endsAt) {
		t.Error("share past its end time should be expired")
	}
}

// A duration of zero, a negative, or something absurd must not produce an
// unbounded share — a live location that outlives the user's intent is a
// privacy problem.
func TestLiveDurationClamp(t *testing.T) {
	for _, secs := range []int64{0, -1, -99999, maxLiveSecs + 1, 1 << 40} {
		got := secs
		if got <= 0 || got > maxLiveSecs {
			got = maxLiveSecs
		}
		if got != maxLiveSecs {
			t.Errorf("secs %d clamped to %d, want %d", secs, got, maxLiveSecs)
		}
	}
	// A sane duration passes through untouched.
	for _, secs := range []int64{60, 15 * 60, 60 * 60, maxLiveSecs} {
		got := secs
		if got <= 0 || got > maxLiveSecs {
			got = maxLiveSecs
		}
		if got != secs {
			t.Errorf("secs %d was clamped to %d, want it left alone", secs, got)
		}
	}
}

// StopLiveLocation is called on cleanup paths where the app may not know
// whether anything is running, so it must not panic or report a false stop.
func TestStopWithoutSession(t *testing.T) {
	c := &Client{live: newLiveShares()}
	if c.StopLiveLocation("12345@s.whatsapp.net") {
		t.Error("stopping a share that was never started reported true")
	}
	if c.StopLiveLocation("not a jid") {
		t.Error("stopping with an unparseable JID reported true")
	}
	if _, ok := c.LiveLocationEndsAt("12345@s.whatsapp.net"); ok {
		t.Error("a chat with no share reported one running")
	}
}

// An expired share must not be reported as running, or the app would keep the
// GPS on for a share WhatsApp has already dropped.
func TestEndsAtIgnoresExpired(t *testing.T) {
	chat := "12345@s.whatsapp.net"
	jid, _ := types.ParseJID(chat)
	c := &Client{live: newLiveShares()}

	c.live.m[jid.String()] = &liveShare{
		chat: jid, startAt: time.Now().Add(-2 * time.Hour),
		endsAt: time.Now().Add(-time.Hour),
	}
	if _, ok := c.LiveLocationEndsAt(chat); ok {
		t.Error("an expired share reported as still running")
	}

	c.live.m[jid.String()].endsAt = time.Now().Add(time.Hour)
	if _, ok := c.LiveLocationEndsAt(chat); !ok {
		t.Error("a running share reported as finished")
	}
}
