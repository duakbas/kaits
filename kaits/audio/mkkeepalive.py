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

The frequency does the rest of the work. 110 Hz is below what a feature-phone
speaker can physically reproduce — those drivers roll off steeply under a few
hundred hertz — so even a bug that set the volume to full would produce
approximately nothing you could hear. 0 Hz, incidentally, is not "no sound": it
is a constant DC offset, which is inaudible for the same reason but pops at the
loop boundary and is the sort of thing a resampler strips out.

8 kHz mono 16-bit, one second: 16 KB, small enough to loop without seams
mattering and small enough not to care about in the package.

    python3 kaits/audio/mkkeepalive.py
"""

import math
import os
import struct

RATE = 8000
SECONDS = 1
FREQ = 110.0          # low, and utterly inaudible at this amplitude
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
