package calls

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// WABackend is as much of a call backend as whatsmeow alone can provide.
//
// Which is exactly one thing: declining. RejectCall is first-party — it sends a
// reject node over the connection we already hold, the same way the official
// client does — so it carries no more risk than sending a message, and needs no
// reimplementation of anything.
//
// Answering is a different matter. It means media: MLow, SRTP, a relay mesh,
// none of which whatsmeow touches. That is what meowcaller is for, and it is a
// third-party reimplementation of a proprietary voice protocol, which is a
// decision about the account rather than a piece of work. See CALLS.md.
//
// So this backend refuses to answer, and says why. A phone that rings, names
// the caller and declines is most of what call handling is used for, and it is
// available today without deciding anything.
type WABackend struct {
	cli *whatsmeow.Client
}

func NewWABackend(cli *whatsmeow.Client) *WABackend { return &WABackend{cli: cli} }

var errNoMedia = fmt.Errorf("answering needs a media backend; this build can only decline")

func (b *WABackend) Answer(context.Context, string, PCMSink) (PCMSource, error) {
	return nil, errNoMedia
}

func (b *WABackend) Dial(context.Context, string, PCMSink) (PCMSource, string, error) {
	return nil, "", errNoMedia
}

func (b *WABackend) Reject(ctx context.Context, callID, fromJID string) error {
	if b.cli == nil || callID == "" {
		return nil
	}
	from, err := types.ParseJID(fromJID)
	if err != nil {
		return fmt.Errorf("reject %s: %w", callID, err)
	}
	return b.cli.RejectCall(ctx, from, callID)
}

// Hangup on a call that was never answered is a decline — there is no media
// session to tear down, only an offer to refuse. Once answering exists this
// will need to tell the two apart.
func (b *WABackend) Hangup(ctx context.Context, callID, fromJID string) error {
	return b.Reject(ctx, callID, fromJID)
}
