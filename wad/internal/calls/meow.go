package calls

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/purpshell/meowcaller"
	"go.mau.fi/whatsmeow"
)

// MeowBackend answers and places calls, through meowcaller.
//
// Read this before enabling it. meowcaller is a third-party reimplementation of
// WhatsApp's voice protocol — MLow, SRTP, the relay mesh — none of which is
// documented or sanctioned. It works, and it is also exactly the kind of
// traffic that gets an account flagged. That is why this is behind WAD_CALLS=1
// rather than simply being the backend: turning it off is an environment
// variable and a restart, not a rebuild, which matters if the account starts
// behaving oddly at two in the morning.
//
// Audio is 16 kHz mono throughout, in 960-sample frames (60 ms), because that
// is what meowcaller hands us and what it wants back. Everything about
// resampling and codecs belongs on the phone side of the daemon, not here.

// MeowBackend implements Backend using meowcaller for media.
type MeowBackend struct {
	cli *meowcaller.Client

	mu sync.Mutex
	// Calls meowcaller has told us about, by its id, so an answer arriving from
	// the phone can find the call object it belongs to.
	calls map[string]*meowcaller.Call

	// recordDir, when set, writes each call's incoming audio to a WAV. This is
	// how the WhatsApp leg gets tested without a phone in the loop at all:
	// answer, record, listen to the file. See CALLS.md, step 2.
	recordDir string
}

// NewMeowBackend wires meowcaller to an existing whatsmeow client and starts
// watching for incoming calls.
func NewMeowBackend(wa *whatsmeow.Client) *MeowBackend {
	b := &MeowBackend{
		cli:       meowcaller.NewClient(wa),
		calls:     map[string]*meowcaller.Call{},
		recordDir: os.Getenv("WAD_CALL_RECORD"),
	}
	// Remember the call, but do NOT answer here. Whether to answer is the
	// phone's decision, and it arrives later as a callanswer frame.
	b.cli.OnIncomingCall(func(call *meowcaller.Call) {
		if call == nil {
			return
		}
		b.mu.Lock()
		b.calls[call.ID()] = call
		b.mu.Unlock()
		call.OnEnd(func(reason string) { b.forget(call.ID()) })
	})
	return b
}

func (b *MeowBackend) forget(id string) {
	b.mu.Lock()
	delete(b.calls, id)
	b.mu.Unlock()
}

func (b *MeowBackend) lookup(id string) (*meowcaller.Call, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.calls[id]
	return c, ok
}

// Answer accepts the call and starts audio in both directions.
func (b *MeowBackend) Answer(ctx context.Context, callID string, sink PCMSink) (PCMSource, error) {
	call, ok := b.lookup(callID)
	if !ok {
		return nil, fmt.Errorf("call %s is not one we know about", callID)
	}
	return b.start(call, sink)
}

// Dial places an outgoing call. dest is a phone number or JID.
//
// Outgoing is worth more than incoming on this hardware, and not for protocol
// reasons: KaiOS discards pushes, so an incoming call can only ring while the
// app happens to be alive. An outgoing one is always possible, because the
// person making it is by definition looking at the phone.
func (b *MeowBackend) Dial(ctx context.Context, dest string, sink PCMSink) (PCMSource, string, error) {
	call, err := b.cli.Call(ctx, dest)
	if err != nil {
		return nil, "", fmt.Errorf("dial %s: %w", dest, err)
	}
	b.mu.Lock()
	b.calls[call.ID()] = call
	b.mu.Unlock()
	call.OnEnd(func(reason string) { b.forget(call.ID()) })

	src, err := b.attach(call, sink)
	if err != nil {
		return nil, "", err
	}
	return src, call.ID(), nil
}

func (b *MeowBackend) start(call *meowcaller.Call, sink PCMSink) (PCMSource, error) {
	src, err := b.attach(call, sink)
	if err != nil {
		return nil, err
	}
	if err := call.Answer(); err != nil {
		src.Close()
		return nil, fmt.Errorf("answer %s: %w", call.ID(), err)
	}
	return src, nil
}

// attach sets up both directions of audio. Done BEFORE answering, so the first
// frames after the peer connects are not dropped on the floor while we are
// still building the plumbing.
func (b *MeowBackend) attach(call *meowcaller.Call, sink PCMSink) (PCMSource, error) {
	var sinks []meowcaller.AudioSink

	if sink != nil {
		sinks = append(sinks, meowcaller.SinkFunc(func(frame []float32) { sink(frame) }))
	}
	if b.recordDir != "" {
		path := filepath.Join(b.recordDir,
			fmt.Sprintf("call-%s-%s.wav", time.Now().UTC().Format("20060102-150405"), call.ID()))
		if rec, err := meowcaller.WAVRecorder(path); err == nil {
			sinks = append(sinks, rec)
			log.Printf("calls: recording %s", path)
		} else {
			// Not fatal: a call that happens without a recording is better than
			// no call because a directory was not writable.
			log.Printf("calls: cannot record to %s: %v", path, err)
		}
	}

	switch len(sinks) {
	case 0:
		// Nowhere for the peer's audio to go. Legal — one-way is still a call —
		// but almost certainly a mistake, so say so.
		log.Printf("calls: no audio sink for %s; the peer will not be heard", call.ID())
	case 1:
		call.Receive(sinks[0])
	default:
		call.Receive(multiSink(sinks))
	}

	bridge := newPCMBridge()
	call.Play(bridge)
	return bridge, nil
}

func (b *MeowBackend) Reject(ctx context.Context, callID, fromJID string) error {
	call, ok := b.lookup(callID)
	if !ok {
		return nil // already gone; declining twice is not an error
	}
	defer b.forget(callID)
	return call.Reject()
}

func (b *MeowBackend) Hangup(ctx context.Context, callID, fromJID string) error {
	call, ok := b.lookup(callID)
	if !ok {
		return nil
	}
	defer b.forget(callID)
	return call.Hangup()
}

// ---- audio plumbing ----

// multiSink fans the peer's audio out to several destinations — typically the
// phone and a WAV file at the same time, which is what makes a failure
// diagnosable after the fact.
type multiSink []meowcaller.AudioSink

func (m multiSink) WriteFrame(frame []float32) error {
	var firstErr error
	for _, s := range m {
		if err := s.WriteFrame(frame); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m multiSink) Close() error {
	var firstErr error
	for _, s := range m {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// pcmBridge is our audio going the other way: whatever the phone sends is
// written in, and meowcaller pulls it out one 960-sample frame at a time.
//
// The two sides have different ideas about size and timing — a WebRTC track
// delivers what it delivers, when it arrives — so this buffers and re-cuts into
// the frames meowcaller asks for.
type pcmBridge struct {
	mu     sync.Mutex
	buf    []float32
	closed bool
	// wake is signalled on every write so a starved reader does not spin.
	wake chan struct{}
}

func newPCMBridge() *pcmBridge {
	return &pcmBridge{wake: make(chan struct{}, 1)}
}

// Write accepts audio of any length from the phone side.
func (p *pcmBridge) Write(pcm []float32) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return io.ErrClosedPipe
	}
	// Bound the buffer. Audio that is a second late is not worth sending: on a
	// live call the far end wants the present, and an unbounded queue turns a
	// slow phone into ever-growing delay rather than a brief gap.
	const maxBuffered = meowcaller.SampleRate // one second
	p.buf = append(p.buf, pcm...)
	if len(p.buf) > maxBuffered {
		p.buf = p.buf[len(p.buf)-maxBuffered:]
	}
	p.mu.Unlock()

	select {
	case p.wake <- struct{}{}:
	default:
	}
	return nil
}

// ReadFrame gives meowcaller exactly one frame, waiting briefly for one and
// then filling with silence.
//
// Silence rather than blocking: the far end needs a steady stream, and a
// momentary gap in what the phone sends should be heard as a gap, not as the
// call seizing up.
func (p *pcmBridge) ReadFrame() ([]float32, error) {
	deadline := time.NewTimer(80 * time.Millisecond)
	defer deadline.Stop()

	for {
		p.mu.Lock()
		if p.closed && len(p.buf) == 0 {
			p.mu.Unlock()
			return nil, io.EOF
		}
		if len(p.buf) >= meowcaller.FrameSamples {
			frame := make([]float32, meowcaller.FrameSamples)
			copy(frame, p.buf[:meowcaller.FrameSamples])
			p.buf = p.buf[meowcaller.FrameSamples:]
			p.mu.Unlock()
			return frame, nil
		}
		p.mu.Unlock()

		select {
		case <-p.wake:
		case <-deadline.C:
			return make([]float32, meowcaller.FrameSamples), nil
		}
	}
}

func (p *pcmBridge) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return nil
}
