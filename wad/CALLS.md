# Calls: how they could actually work

Written while the rest of the project waits. Nothing here is built yet; this is
the plan and, more usefully, the list of things that could sink it.

## What already exists

More than you'd think. `internal/calls` has the shape of it:

- `Backend` — the four things the daemon needs from a call library: `Answer`,
  `Reject`, `Dial`, `Hangup`, with audio as `[]float32` PCM in and out.
- `Manager` — one call at a time, which is all a feature phone needs.
- `Noop` — so the daemon builds and runs message-only today.
- Wire frames both directions: `calloffer`, `callstate`, `callsignal`,
  `callanswer`, `callreject`, `calldial`, `callhangup`.
- A call screen in the app, and `showIncomingCall` wired to `calloffer`.
- `audio-capture` in the privileged manifest — already granted on the test
  device.

So the plumbing is drawn. What's missing is media.

## The blocker has moved

The comment in `calls.go` says meowcaller "has no tagged release and its
live-media API isn't documented in anything I could verify." That is no longer
true. `github.com/purpshell/meowcaller` is MIT-licensed, actively developed,
and does exactly the hard part:

- inbound and outbound calls, accept/reject/cancel;
- audio through Meta's **MLow** codec, implemented in pure Go;
- injection into WhatsApp's SRTP relay mesh;
- audio out via a sink — `SinkFunc(func(pcm []float32))` — and audio in via a
  source passed to `call.Play`;
- 16 kHz mono PCM.

Its own README lists Opus fallback as in progress. So: the WhatsApp leg is
someone else's solved problem, and our job is the other leg and the join.

## The shape

    WhatsApp  <--MLow/SRTP-->  meowcaller  <--16k PCM-->  wad  <--WebRTC-->  KaiOS

The daemon sits in the middle doing format conversion. Nothing else is
plausible: the phone cannot speak WhatsApp's call protocol, and the daemon
cannot speak into the phone's speaker.

### Why WebRTC for the phone leg, and not the socket we already have

Tempting to send PCM over the existing WebSocket — no ICE, no SDP, no new
moving parts. It doesn't survive contact with the numbers. 16 kHz 16-bit mono
is 256 kbit/s each way, which is fine on Wi-Fi and not fine on mobile data, and
the phone has no way to compress it: Gecko 48 has no WebCodecs, and
`MediaRecorder` produces containerised chunks about a second behind, which is
not a phone call.

WebRTC gets us codec, jitter buffer, packet loss concealment and echo
cancellation from the browser, all of which we would otherwise be writing in
JavaScript on a 2016 engine. `pion` is the Go side.

### The simplification worth taking: G.711, not Opus

The obvious pairing is Opus on the WebRTC leg. It's also the expensive one:
there is no pure-Go Opus **encoder**, so the daemon would need cgo and
libopus — another system dependency in `install.sh`, another thing to break a
build on a box we can't see.

**Negotiate PCMU (G.711 µ-law) with the phone instead.** Then the daemon's
entire codec responsibility is a 256-entry lookup table and a resampler:

    WA 16k PCM  ->  downsample 8k  ->  µ-law  ->  RTP  ->  phone
    phone  ->  µ-law  ->  16-bit  ->  upsample 16k  ->  meowcaller

G.711 is 64 kbit/s and telephone-grade, which is precisely what this is — a
phone call on a feature phone. Firefox has supported PCMU since long before 48.

**Verify before building on it:** get the SDP offer out of the KaiOS app and
confirm PCMU is in the m=audio line. If it isn't, we're back to Opus and cgo,
and I'd rather know that on day one than on day five.

## Order of work, so each step is worth having alone

**0. Reject, properly.** `Manager.HandleAppFrame` calls `m.be.Reject`, which
is `Noop`. whatsmeow has `RejectCall` today. Wiring that gives a phone that
rings, shows who's calling, and can decline — no media anywhere. Half an hour,
and it's the half of calling that's most used.

**1. Missed calls in the log.** `CallTerminate` and `CallOffer` are already
delivered. Store them as messages of kind `call`. Independent of everything
below.

**2. meowcaller, server-side only.** BUILT — `internal/calls/meow.go`, behind
`WAD_CALLS=1`. Set `WAD_CALL_RECORD=/some/dir` as well and every answered call
writes the peer's audio to a WAV. No phone involved: answer, record, listen to
the file. That proves the whole WhatsApp leg — MLow, SRTP, relay — in isolation,
and if meowcaller does not work against a current account we find out before
writing any of the rest.

**3. pion to the phone, no WhatsApp.** Ring the app, answer, and send a
generated tone. This proves Gecko 48 ↔ pion interop, which is the part I'd bet
on going wrong: SDP semantics, DTLS-SRTP, ICE against a VPS with no NAT.

**4. Join them.** Resample and forward in both directions. If 2 and 3 both
work, this is a buffer and a loop.

**5. Outgoing.** `calldial` exists in the protocol. Same media path, reversed.

Steps 2 and 3 are independent and either can fail without wasting the other.

## What could sink it

**The account.** This is the one that matters. meowcaller is a third-party
reimplementation of a proprietary voice protocol, and call traffic that doesn't
look like a real client is exactly the sort of thing that gets an account
flagged. This account is the one you use. Worth deciding in advance whether
calls are worth that risk, and worth testing against a second number if you can
get one.

**Incoming calls only ring while the app is alive.** KaiOS's push service
accepts pushes and discards them — established, and independently confirmed in
the BananaHackers thread. So there is no way to wake a closed app for a call.
The app has to be running and connected, which on this hardware means the
foreground or a recently-backgrounded state. That is a real ceiling on how
useful incoming calls can be, and it does not apply to outgoing ones — which
argues for doing step 5 earlier than its number suggests.

**Gecko 48 WebRTC.** Firefox 48 predates unified plan. pion can be configured
for either, and a single audio track is the case most likely to interoperate,
but this is 2016 WebRTC talking to a 2026 library and I would not assume.

**Latency.** Phone → VPS → WhatsApp relay → the other party. The VPS hop is new
and it is in Zurich; if the other party is far away this may be audibly worse
than a normal call.

**Battery and heat.** An MT6739 doing WebRTC with a 1400 mAh battery. Expect
minutes, not hours, and expect the phone to be warm.

**Echo.** The KaiOS speaker and microphone are centimetres apart. Ask
`getUserMedia` for `echoCancellation`, and be ready for it to be ignored.

## Decisions to make before starting

1. Is the account risk acceptable? If not, stop here — everything else is moot.
2. Outgoing-only first? It sidesteps the push problem entirely and is arguably
   the more useful half on a phone that gets killed in the background.
3. Audio only. Video is possible — meowcaller supports it — and is not worth a
   sentence of effort on a 240×320 screen.
