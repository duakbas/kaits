#!/usr/bin/env python3
"""Generate the near-silent loop used to keep the app alive in the background.

Why a file and not a data: URI: a privileged package is served under a CSP
whose default-src does not reliably cover data:, so a data: URI would work in
every browser you'd test in and be blocked on the phone. A real file in the
package is covered by 'self' and works under both app types.

Why near-silent rather than actually silent: the platform decides an app is
"perceptibly doing something" from its audio channel being active, and an
element of pure zeroes is a plausible thing for an engine to shortcut. Three
LSBs of a low tone is inaudible on a phone speaker at any volume and leaves
nothing to optimise away.

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
AMPLITUDE = 3         # out of 32767

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
