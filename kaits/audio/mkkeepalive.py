#!/usr/bin/env python3
"""Generate the near-silent loop used to keep the app alive in the background.

Why a file and not a data: URI: a privileged package is served under a CSP
whose default-src does not reliably cover data:, so a data: URI would work in
every browser you'd test in and be blocked on the phone. A real file in the
package is covered by 'self' and works under both app types.

Why not silence, and why not zero volume: the platform decides an app is
"perceptibly doing something" from whether it is AUDIBLE, and Gecko computes
that from the stream rather than taking playback as proof. Digital silence, a
muted element, or volume 0 are the three ways to be counted as inaudible — and
an inaudible app is exactly the one the priority manager ignores. Turning the
volume to zero would not make this politer, it would make it pointless.

So the signal is loud enough to be unambiguous to a detector and quiet enough
to be inaudible in the room, and those are two different knobs:

  - decoded amplitude 1% of full scale (-40 dBFS), so nothing analysing the
    samples mistakes it for silence;
  - element volume 0.01 on top (see KEEPALIVE_VOLUME), putting -80 dBFS at the
    speaker, which is far below the noise floor of any phone.

The frequency does the rest of the work, and it is chosen for HEADPHONES, not
for the phone's own speaker. A phone speaker cannot reproduce anything below a
few hundred hertz, so any low tone is safe through it — but headphones
reproduce low frequencies perfectly well, which is exactly what that argument
forgets. What protects the headphone case is the ear, not the driver.

At -80 dBFS the tone lands around 20 dB SPL on headphones at full volume. The
absolute threshold of hearing is strongly frequency dependent, so that number
alone decides nothing:

    110 Hz  threshold ~27 dB SPL   margin  +7 dB    thin
     63 Hz  threshold ~38 dB SPL   margin +18 dB
     20 Hz  threshold ~78 dB SPL   margin +58 dB    decisive

Hence 20 Hz: at the bottom edge of human hearing, where the ear is some 50 dB
less sensitive than it is at a couple of hundred hertz, the margin stops being
a calculation you have to trust and becomes obvious. It is also an exact number
of cycles in a one-second loop, so the seam stays continuous.

0 Hz, meanwhile, is not "no sound" — it is a constant DC offset. It is
inaudible, but the output path blocks DC (there is a capacitor in the way), so
it may well reach the hardware as nothing at all while also being the kind of
constant signal an audibility check can dismiss. It risks the one failure that
matters: silent AND not counted. It also displaces the speaker cone while it
runs and pops when it stops. A tone below hearing gets the same inaudibility
without either gamble.

8 kHz mono 16-bit, THIRTY seconds: 480 KB. Length is the point. This file is
only the fallback for an engine with no Web Audio — the normal path synthesises
the tone continuously — but a looping element clicks at every loop point, and a
one-second loop clicked 3600 times an hour. Thirty seconds cuts that by thirty,
and the file is still small enough not to think about.

    python3 kaits/audio/mkkeepalive.py
"""

import math
import os
import struct

RATE = 8000
SECONDS = 30
FREQ = 20.0           # bottom edge of hearing; see the note above
AMPLITUDE = 328       # out of 32767 — 1% of full scale, -40 dBFS

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "keepalive.wav")


def main():
    frames = RATE * SECONDS
    samples = bytearray()
    for i in range(frames):
        # A whole number of cycles across the loop, so the end meets the start
        # and looping introduces no discontinuity.
        cycles = round(FREQ * SECONDS)
        v = int(AMPLITUDE * math.sin(2 * math.pi * cycles * i / frames))
        samples += struct.pack("<h", v)

    data = bytes(samples)
    header = b"RIFF" + struct.pack("<I", 36 + len(data)) + b"WAVE"
    header += b"fmt " + struct.pack("<IHHIIHH", 16, 1, 1, RATE, RATE * 2, 2, 16)
    header += b"data" + struct.pack("<I", len(data))

    with open(OUT, "wb") as f:
        f.write(header + data)
    print("wrote %s (%d bytes)" % (OUT, len(header) + len(data)))


if __name__ == "__main__":
    main()
