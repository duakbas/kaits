package calls

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log"
	"strings"
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// legTrack is the daemon's outgoing audio track. pion's own static tracks are
// bound to ONE codec chosen before the offer, and fail if the phone answers
// with another; this one binds to whichever of the offered codecs the phone
// accepted, best first, and packetises RTP itself.
type legTrack struct {
	leg    *PhoneLeg
	prefer []legCodec

	mu    sync.Mutex
	name  string // bound codec, "" until Bind
	pt    uint8
	ssrc  uint32
	w     webrtc.TrackLocalWriter
	seq   uint16
	ts    uint32
	first bool
}

var errNotBound = errors.New("audio track not bound yet")

func (t *legTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	got := ctx.CodecParameters()
	for _, want := range t.prefer {
		for _, c := range got {
			if strings.EqualFold(c.MimeType, want.p.MimeType) {
				t.name, t.pt, t.ssrc, t.w = want.name, uint8(c.PayloadType), uint32(ctx.SSRC()), ctx.WriteStream()
				var r [6]byte
				_, _ = rand.Read(r[:])
				t.seq = binary.BigEndian.Uint16(r[:2])
				t.ts = binary.BigEndian.Uint32(r[2:])
				t.first = true
				log.Printf("calls: phone leg %s: sending %s to the phone", t.leg.callID, want.name)
				return c, nil
			}
		}
	}
	var have []string
	for _, c := range got {
		have = append(have, c.MimeType)
	}
	log.Printf("calls: phone leg %s: phone accepted none of our codecs (it answered %v)", t.leg.callID, have)
	return webrtc.RTPCodecParameters{}, webrtc.ErrUnsupportedCodec
}

func (t *legTrack) Unbind(webrtc.TrackLocalContext) error {
	t.mu.Lock()
	t.w = nil
	t.mu.Unlock()
	return nil
}

func (t *legTrack) ID() string                { return "audio" }
func (t *legTrack) RID() string               { return "" }
func (t *legTrack) StreamID() string          { return "wad" }
func (t *legTrack) Kind() webrtc.RTPCodecType { return webrtc.RTPCodecTypeAudio }

func (t *legTrack) codecName() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.w == nil {
		return ""
	}
	return t.name
}

// write sends one frame; ticks is its duration in RTP clock units.
func (t *legTrack) write(payload []byte, ticks uint32) error {
	t.mu.Lock()
	w := t.w
	h := rtp.Header{
		Version:        2,
		PayloadType:    t.pt,
		SequenceNumber: t.seq,
		Timestamp:      t.ts,
		SSRC:           t.ssrc,
		Marker:         t.first, // start of a talkspurt (RFC 3551)
	}
	t.seq++
	t.ts += ticks
	t.first = false
	t.mu.Unlock()
	if w == nil {
		return errNotBound
	}
	_, err := w.WriteRTP(&h, payload)
	return err
}

func mimeName(m string) string {
	switch strings.ToLower(m) {
	case strings.ToLower(webrtc.MimeTypeOpus):
		return "opus"
	case strings.ToLower(webrtc.MimeTypeG722):
		return "g722"
	}
	return "pcmu"
}

// answerSummary keeps the lines of an SDP answer that say what was accepted.
func answerSummary(sdp string) string {
	var keep []string
	for _, l := range strings.Split(sdp, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "m=") || strings.HasPrefix(l, "a=rtpmap") || strings.HasPrefix(l, "a=fmtp") ||
			strings.HasPrefix(l, "a=ice-ufrag") || strings.HasPrefix(l, "a=inactive") || strings.HasPrefix(l, "a=recvonly") ||
			strings.HasPrefix(l, "a=sendonly") || strings.HasPrefix(l, "a=sendrecv") {
			keep = append(keep, "  "+l)
		}
	}
	return strings.Join(keep, "\n")
}
