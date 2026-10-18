//go:build !cgo || noopus

package calls

// Built without libopus: the phone leg falls back to G.711 (narrowband).

import "errors"

const opusAvailable = false

var errNoOpus = errors.New("built without libopus")

type opusEncoder struct{}
type opusDecoder struct{}

func newOpusEncoder(rate, bitrate int) (*opusEncoder, error) { return nil, errNoOpus }
func (*opusEncoder) Encode([]int16) ([]byte, error)          { return nil, errNoOpus }
func (*opusEncoder) Close()                                  {}
func newOpusDecoder(rate int) (*opusDecoder, error)          { return nil, errNoOpus }
func (*opusDecoder) Decode([]byte, int, bool) ([]int16, error) {
	return nil, errNoOpus
}
func (*opusDecoder) Close() {}
