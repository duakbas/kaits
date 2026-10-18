//go:build !cgo || nog722

package calls

import "errors"

const g722Available = false

type g722Encoder struct{}
type g722Decoder struct{}

func newG722Encoder() (*g722Encoder, error) { return nil, errors.New("built without spandsp") }
func (*g722Encoder) Encode([]int16) []byte  { return nil }
func (*g722Encoder) Close()                 {}
func newG722Decoder() (*g722Decoder, error) { return nil, errors.New("built without spandsp") }
func (*g722Decoder) Decode([]byte) []int16  { return nil }
func (*g722Decoder) Close()                 {}
