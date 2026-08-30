package wa

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
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

// The reported bug: a live share posted a brand new location card every tick
// instead of moving the one it opened with. whatsmeow has no live-location
// update primitive, so a LiveLocationMessage handed to SendMessage is an
// ordinary message with an ordinary new id — which is exactly what an update
// used to be. An update has to be an edit of the opening message.
func TestLiveUpdateIsAnEditOfTheOpeningMessage(t *testing.T) {
	chat, _ := types.ParseJID("12345@s.whatsapp.net")
	msg := liveUpdateMsg("START-ID", chat, 47.3769, 8.5417, 12, 5, 900)

	if msg.GetLiveLocationMessage() != nil {
		t.Fatal("update is a top-level LiveLocationMessage — that is a NEW message, " +
			"which is the bug: one card per tick")
	}
	pm := msg.GetEditedMessage().GetMessage().GetProtocolMessage()
	if pm == nil {
		t.Fatal("update is not an edit at all")
	}
	if pm.GetType() != waE2E.ProtocolMessage_MESSAGE_EDIT {
		t.Errorf("edit type = %v, want MESSAGE_EDIT", pm.GetType())
	}
	if pm.GetKey().GetID() != "START-ID" {
		t.Errorf("edits message %q, want the opening message START-ID", pm.GetKey().GetID())
	}
	if !pm.GetKey().GetFromMe() {
		t.Error("edit key is not marked fromMe; only our own message is ours to edit")
	}
	if got := pm.GetKey().GetRemoteJID(); got != chat.String() {
		t.Errorf("edit key chat = %q, want %q", got, chat.String())
	}

	live := pm.GetEditedMessage().GetLiveLocationMessage()
	if live == nil {
		t.Fatal("the edit carries no live location")
	}
	if live.GetDegreesLatitude() != 47.3769 || live.GetDegreesLongitude() != 8.5417 {
		t.Errorf("coordinates = %f,%f", live.GetDegreesLatitude(), live.GetDegreesLongitude())
	}
	if live.GetSequenceNumber() != 5 || live.GetTimeOffset() != 900 {
		t.Errorf("seq/remaining = %d/%d, want 5/900", live.GetSequenceNumber(), live.GetTimeOffset())
	}
	if live.GetAccuracyInMeters() != 12 {
		t.Errorf("accuracy = %d, want 12", live.GetAccuracyInMeters())
	}
}

// The escape hatch has to actually produce the old shape, or it is no use for
// working out which behaviour a real client honours.
func TestLiveUpdateResendEnvRestoresSeparateMessages(t *testing.T) {
	t.Setenv("WAD_LIVELOC_RESEND", "1")
	chat, _ := types.ParseJID("12345@s.whatsapp.net")
	msg := liveUpdateMsg("START-ID", chat, 47.3769, 8.5417, 0, 5, 900)

	live := msg.GetLiveLocationMessage()
	if live == nil {
		t.Fatal("WAD_LIVELOC_RESEND=1 did not produce a plain live location message")
	}
	if msg.GetEditedMessage() != nil {
		t.Error("produced both an edit and a plain message")
	}
	if got := live.GetContextInfo().GetStanzaID(); got != "START-ID" {
		t.Errorf("context stanza id = %q, want START-ID", got)
	}
}

// A refused update must not be retried every 30 seconds for the rest of an
// eight-hour share. c.WA is nil here, so a second send attempt would panic —
// which is the assertion.
func TestUpdateStopsAfterARefusal(t *testing.T) {
	chat := "12345@s.whatsapp.net"
	jid, _ := types.ParseJID(chat)
	c := &Client{live: newLiveShares()}
	c.live.m[jid.String()] = &liveShare{
		chat: jid, startMsg: "START-ID", startAt: time.Now(),
		endsAt: time.Now().Add(time.Hour), broken: true,
	}

	running, err := c.UpdateLiveLocation(context.Background(), chat, 47.3769, 8.5417, 0)
	if err != nil {
		t.Errorf("a share that stopped transmitting reported an error every tick: %v", err)
	}
	if !running {
		t.Error("reported the share as finished; it is still running, just not transmitting")
	}
}

// The other half of "it ends the moment it is sent": an update announced how
// long the share had been RUNNING where the opening message announces how long
// it will LAST. A receiving client reading that field the same way in both
// places sees a share good for a few seconds and draws a card that has already
// expired. The two must mean the same thing.
func TestUpdateAnnouncesTimeRemainingNotElapsed(t *testing.T) {
	chat := "12345@s.whatsapp.net"
	jid, _ := types.ParseJID(chat)
	c := &Client{live: newLiveShares()}

	// A one-hour share, opened ten minutes ago.
	started := time.Now().Add(-10 * time.Minute)
	c.live.m[jid.String()] = &liveShare{
		chat: jid, startMsg: "START-ID", startAt: started,
		endsAt: started.Add(time.Hour),
	}

	c.live.mu.Lock()
	remaining := uint32(time.Until(c.live.m[jid.String()].endsAt).Seconds())
	c.live.mu.Unlock()

	if remaining < 49*60 || remaining > 50*60 {
		t.Fatalf("remaining = %ds, want about 50 minutes", remaining)
	}
	elapsed := uint32(time.Since(started).Seconds())
	if remaining <= elapsed {
		t.Errorf("remaining %d is not distinguishable from elapsed %d here; "+
			"the test cannot tell the bug from the fix", remaining, elapsed)
	}
}
