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

**This is now what's built.** The daemon offers, so it does not have to hope
PCMU is in the phone's list — it presents a one-codec offer and the phone can
take it or refuse it. If Gecko 48 refuses a PCMU-only offer outright, the tone
test fails at `setRemoteDescription` and we find out in one command rather than
after building the whole media path on the assumption.

The transcode this leaves is `g711.go`, which is the whole of it: a lookup
table with tests against the published µ-law values.

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

**3. pion to the phone, no WhatsApp.** BUILT — `internal/calls/phoneleg.go`
and `kaits/js/call.js`. Ring the app, answer, hear a warbling tone; the daemon
logs the level of whatever the phone's microphone sends back, so one test
exercises both directions.

```bash
curl "https://your.host/debug/call?token=$WAD_TOKEN"
```

Nothing about this touches WhatsApp — no account, no relay, nothing that can
get a number flagged — so it can be run as often as it takes. That is the
point: the account risk is the only part of this feature that cannot be
undone, so the risky half stays off until this half works.

What to expect in the daemon log:

```
calls: test call test-dli258nn ringing
calls: phone leg test-dli258nn: connecting
calls: phone leg test-dli258nn: connected
calls: test test-dli258nn — 98 packets from the phone, peak 4210 (-17 dBFS)
```

`connected` proves ICE and DTLS-SRTP. The tone in your ear proves the daemon →
phone direction and G.711. The packet count and level prove phone → daemon,
which is the half more likely to fail on this hardware: `getUserMedia` in a
packaged app, a permission that can be refused silently, an echo canceller
enthusiastic enough to mute everything.

If it says `connected` and you hear nothing, the transport is fine and the
fault is codec or audio routing. If it never says `connected`, it is ICE or
DTLS and the SDP is where to look.

**4. Join them.** BUILT — `internal/calls/join.go` and `resample.go`. It was a
buffer and a loop, and the buffer was the interesting half.

**UNVERIFIED against a live call.** Every piece has tests; what nothing here
can test is the three running together with a real account at one end, and
that is deliberately the last thing to do.

Two things were worth getting right rather than hacking:

*The resampler has an actual filter.* 16 kHz to 8 kHz by taking every other
sample is the obvious implementation and it is wrong in a way you can hear:
anything above 4 kHz does not vanish, it FOLDS. A 6 kHz component comes back
at 2 kHz, in the middle of speech, as a metallic warble that follows the voice
and cannot be removed afterwards. `resample_test.go` feeds a 6 kHz tone
through and requires 30 dB of rejection; the naive version fails it by 30 dB
exactly, which is the measure of how audible it would have been.

*The pump runs on its own clock.* meowcaller delivers 60 ms; RTP wants 20 ms.
Forwarding each arrival straight through sends three packets and then nothing,
and the two sides are clocked by different things and drift. A bounded queue
drained on a local ticker absorbs both. It fills with silence when starved —
a gap should sound like a gap, not like the call seizing up — and drops the
OLDEST audio when it overflows, because a second-old packet is worth nothing
to a conversation and an unbounded queue turns a brief stall into a call that
is minutes behind by the end.

The daemon logs queue depth every five seconds during a call. Steadily growing
means the clocks disagree; permanently zero means nothing is arriving from
WhatsApp.

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

**Gecko 48 WebRTC.** Downgraded from "would not assume" to "probably fine",
on evidence gathered before writing the code:

- **DTLS version was the real worry, and it is not a problem.** pion/dtls
  implements 1.2 and nothing older. Firefox has sent a DTLS 1.2 ClientHello
  since Firefox **37** — Mozilla bug 1153702 is titled "Firefox 37 uses DTLS
  1.2 client, breaking any WebRTC implementations using DTLS 1.0" — so Gecko 48
  is comfortably above pion's floor rather than just under it. (DTLS 1.0 was
  not removed from Firefox until 86, long after this engine.)
- **Cipher suites overlap.** Firefox has offered
  `TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256` and
  `TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256` since 34; pion supports both.
- **Unified plan does not bite for one audio track.** A single `m=audio`
  section is identical under either set of semantics. It would matter for a
  second track, and there will never be one.
- **The offer is deliberately minimal**, which is the mitigation for everything
  not yet known: one codec (PCMU), no header extensions, no interceptors. See
  the header comment in `phoneleg.go` for why each was dropped.

`phoneleg_test.go` asserts that shape and runs a whole session against a second
pion peer — offer, answer, ICE, DTLS-SRTP, µ-law, audio both ways. That cannot
prove Gecko interoperates, and nothing here can. What it proves is that when
the phone fails, our side is not the reason, which is the question that would
otherwise take an evening to answer.

The genuinely untested part is now narrow: whether Gecko 48 accepts pion's SDP,
and whether `getUserMedia` works in a packaged app on this handset.

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
