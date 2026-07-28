package calls

import (
	"go.mau.fi/whatsmeow/types/events"
)

// WACallHook returns a function suitable for wa.Client.SetCallHook. It does the
// whatsmeow type switch here (so the core Manager stays whatsmeow-agnostic) and
// translates into Manager calls.
//
// Note on the offer types:
//   - events.CallOffer      -> 1:1 call. No Media field; treat as audio.
//   - events.CallOfferNotice-> group / notice; has .Media ("audio"|"video")
//     and .Type ("group" when group). We ring for these too.
//   - events.CallTerminate  -> ended; .Reason is "timeout"/"rejected_elsewhere"/
//     "accepted_elsewhere"/etc.
func WACallHook(m *Manager) func(any) {
	return func(evt any) {
		switch v := evt.(type) {

		case *events.CallOffer:
			m.NotifyIncoming(
				v.CallID,
				v.From.String(),
				v.From.User, // name lookup can be added via contacts store
				false,       // 1:1 offer: assume audio; upgrade if video negotiated
				v.Timestamp.Unix(),
			)

		case *events.CallOfferNotice:
			m.NotifyIncoming(
				v.CallID,
				v.From.String(),
				v.From.User,
				v.Media == "video",
				v.Timestamp.Unix(),
			)

		case *events.CallTerminate:
			m.NotifyEnded(v.CallID, v.Reason)

		// CallAccept / CallPreAccept / CallRelayLatency: informational for
		// now; useful later for accurate call-state UI.
		default:
			_ = v
		}
	}
}
