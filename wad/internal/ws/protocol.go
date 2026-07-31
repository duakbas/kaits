// Package ws defines the JSON wire protocol spoken between the daemon and the
// KaiOS app over a single WebSocket, plus the hub that fans messages out.
//
// Design notes for the phone side:
//   - Every frame is one JSON object with a "t" (type) field. Keep it flat;
//     Gecko 48's JSON.parse is fine but you don't want deep nesting in hot paths.
//   - The app never speaks WhatsApp protocol. It speaks THIS. The daemon owns
//     all the whatsmeow/meowcaller complexity.
//   - Media for a live call does NOT go over this socket. Signalling does
//     (offer/answer/ICE); the actual RTP flows app <-> pion directly. See
//     the calls package for why.
package ws

import "encoding/json"

// Envelope is the outer shape of every frame in both directions.
type Envelope struct {
	T    string          `json:"t"`              // message type, see constants below
	ID   string          `json:"id,omitempty"`   // client-generated id for req/reply correlation
	Data json.RawMessage `json:"data,omitempty"` // type-specific payload
}

// ---- daemon -> app ----
const (
	TReady        = "ready"        // daemon connected & WA session live
	TPaired       = "paired"       // pairing completed (after QR scan)
	TQR           = "qr"           // {code: "2@..."} render this as a QR on-screen
	TMessage      = "message"      // an inbound WA message (see MsgData)
	TReceipt      = "receipt"      // delivery/read receipt for a message we sent
	TChatList     = "chatlist"     // reply to "getchats": [{jid,name,ts,preview,unread}]
	THistory      = "history"      // reply to "gethistory": messages for one chat
	TPresence     = "presence"     // someone came online / typing
	TCallOffer    = "calloffer"    // incoming WA call (see CallData) -> app should ring
	TCallState    = "callstate"    // call lifecycle: ringing/accepted/ended/failed
	TCallSignal   = "callsignal"   // WebRTC signalling from pion -> app (sdp/ice)
	TError        = "error"        // {code, msg}
	TProfile      = "profile"      // reply to "getprofile"/"savecontact" (ProfileData)
	TChatUpdate   = "chatupdate"   // {chat, pinned, muted, archived, removed}
	TReaction     = "reaction"     // {chat, msgid, reactions:[...]} — the full current set
	TStatus       = "status"       // {chat, msgid, status} delivery state of a message we sent
	TTyping       = "typing"       // both ways: {chat, sender, sendername, state} composing/paused
	TSearchResult = "searchresult" // reply to "search": [{msgid,chat,chatname,text,ts}]
)

// ---- app -> daemon ----
const (
	TSend         = "send"         // send a text/image message (see SendData)
	TGetChats     = "getchats"     // request chat list
	TGetHistory   = "gethistory"   // {jid, before, limit} request history for a chat
	TMarkRead     = "markread"     // {jid, msgid} mark read
	TDelete       = "delete"       // {chat, msgid} delete (revoke) own message
	TForward      = "forward"      // {srcmsgid, dest} forward a message to another chat
	TDeleted      = "deleted"      // daemon->app: {chat, msgid} confirm a deletion
	TCallAnswer   = "callanswer"   // user pressed green key on an incoming call
	TCallReject   = "callreject"   // user pressed red key / declined
	TCallDial     = "calldial"     // {jid} place an outgoing call
	TCallHangup   = "callhangup"   // end the active call
	TCallSignalA  = "callsignal"   // WebRTC signalling from app -> pion (same type both ways)
	TChatAction   = "chataction"   // {chat, action, on} pin/mute/archive/delete a chat
	TGetProfile   = "getprofile"   // {jid} request contact or group info
	TSaveContact  = "savecontact"  // {jid, name} save a local nickname ("" clears)
	TSendReaction = "sendreaction" // {chat, msgid, emoji} react ("" removes)
	TSearch       = "search"       // {q, chat?, limit?} search stored messages
	TWatch        = "watch"        // {jid} subscribe to a contact's presence
)

// MsgData is an inbound message pushed to the app.
type MsgData struct {
	MsgID      string `json:"msgid"`
	ChatJID    string `json:"chat"`       // conversation this belongs to
	ChatName   string `json:"chatname"`   // resolved: group subject or contact name
	SenderJID  string `json:"sender"`     // who sent it (matters in groups)
	SenderName string `json:"sendername"` // resolved display name of the sender
	IsGroup    bool   `json:"group"`      // true if chat is a group (@g.us)
	Pinned     bool   `json:"pinned"`     // chat is pinned (synced from account)
	FromMe     bool   `json:"fromme"`
	Timestamp  int64  `json:"ts"`              // unix seconds
	Kind       string `json:"kind"`            // "text" | "image" | "audio" | "video" | "doc"
	Text       string `json:"text,omitempty"`  // body / caption
	MediaURL   string `json:"media,omitempty"` // http url on the daemon to fetch the blob
	Mime       string `json:"mime,omitempty"`
	QuotedID   string `json:"quoted,omitempty"`
	QuotedText string `json:"quotedtext,omitempty"` // preview of the message this replies to
	QuotedName string `json:"quotedname,omitempty"` // who sent the quoted message
	Forwarded  bool   `json:"forwarded,omitempty"`  // carries WhatsApp's forwarded marker
	// Status is the delivery state of a message we sent: "" | "sent" |
	// "delivered" | "read" | "played". Meaningless on incoming messages.
	Status    string         `json:"status,omitempty"`
	Reactions []ReactionData `json:"reactions,omitempty"`
	// Location payload, set when Kind == "location". Lat/Lon are 0,0 only when
	// genuinely unknown — the app checks Kind, not the coordinates.
	Lat        float64 `json:"lat,omitempty"`
	Lon        float64 `json:"lon,omitempty"`
	LocName    string  `json:"locname,omitempty"`
	LocAddress string  `json:"locaddress,omitempty"`
}

// ReactionData is one person's reaction to one message. The app groups these by
// emoji to show counts, and lists them by person when opened.
type ReactionData struct {
	SenderJID  string `json:"sender"`
	SenderName string `json:"sendername"`
	Emoji      string `json:"emoji"`
	Timestamp  int64  `json:"ts"`
}

// SendData is an outgoing message from the app.
type SendData struct {
	ChatJID  string `json:"chat"`
	Kind     string `json:"kind"` // "text" | "image"
	Text     string `json:"text,omitempty"`
	MediaB64 string `json:"media,omitempty"` // base64 image bytes for kind=image
	Mime     string `json:"mime,omitempty"`
	QuotedID string `json:"quoted,omitempty"`
	// Private turns a quoted reply into a "reply privately": the message goes
	// to the quoted author's DM instead of ChatJID, which the daemon resolves
	// from QuotedID (the app can't — group senders are per-group LIDs).
	Private bool `json:"private,omitempty"`
	// FileName labels a document; ignored for other kinds.
	FileName string `json:"filename,omitempty"`
}

// ProfileData is everything the contact / group info screen renders.
//
// Name is the resolved display name; the other name fields are kept separate so
// the screen can show, for example, "Peder" as the heading with the WhatsApp
// push name underneath, and can tell "saved by me" from "named themselves".
type ProfileData struct {
	JID           string       `json:"jid"`
	Name          string       `json:"name"`                    // resolved display name
	SavedName     string       `json:"savedname,omitempty"`     // nickname saved in this app
	PushName      string       `json:"pushname,omitempty"`      // name the contact set on WhatsApp
	Phone         string       `json:"phone,omitempty"`         // +<E.164>, when the number is known
	RedactedPhone string       `json:"redactedphone,omitempty"` // "+90 ∙∙∙∙∙∙∙∙00" fallback
	Status        string       `json:"status,omitempty"`        // "about" text, or a group's topic
	AvatarURL     string       `json:"avatar,omitempty"`
	IsGroup       bool         `json:"group"`
	IsBusiness    bool         `json:"business,omitempty"`
	Saved         bool         `json:"saved"`  // known under a name the user chose
	InAddressBook bool         `json:"inbook"` // specifically from the synced address book
	Pinned        bool         `json:"pinned"`
	Muted         bool         `json:"muted"`
	Archived      bool         `json:"archived"`
	MemberCount   int          `json:"members,omitempty"`
	CreatedAt     int64        `json:"created,omitempty"`
	Members       []MemberData `json:"memberlist,omitempty"`
}

// MemberData is one participant row on a group's info screen.
type MemberData struct {
	JID     string `json:"jid"`
	Name    string `json:"name"`
	IsAdmin bool   `json:"admin,omitempty"`
}

// ChatActionData asks the daemon to change a chat's state on the account.
// Action is "pin" | "mute" | "archive" | "delete"; On is the desired state and
// is ignored for "delete". Duration is optional mute seconds (0 = indefinite).
type ChatActionData struct {
	ChatJID  string `json:"chat"`
	Action   string `json:"action"`
	On       bool   `json:"on"`
	Duration int64  `json:"duration,omitempty"`
}

// CallData describes an incoming call for the ring screen.
type CallData struct {
	CallID    string `json:"callid"`
	FromJID   string `json:"from"`
	FromName  string `json:"name"`
	Video     bool   `json:"video"`
	Timestamp int64  `json:"ts"`
}

// Signal carries opaque WebRTC signalling (SDP offer/answer or an ICE
// candidate) in whichever direction. The app and pion each just forward
// these to their local RTCPeerConnection.
type Signal struct {
	CallID    string          `json:"callid"`
	Kind      string          `json:"kind"` // "offer" | "answer" | "ice"
	SDP       string          `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
}

// Frame is a small helper to build an outbound envelope.
func Frame(t string, v any) (Envelope, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{T: t, Data: b}, nil
}
