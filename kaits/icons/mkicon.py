#!/usr/bin/env python3
"""Generate the Kaits app icons.

No PIL here, so the PNG is assembled by hand: RGBA rows, deflate, three chunks.
Supersampled 4x, which matters a lot at 56px.

Two overlapping bubbles — a conversation, not a message. The white one is
separated from the teal by a gap of background so the shapes stay legible when
the whole icon is 56 pixels across.
"""

import struct
import sys
import zlib

BG = (0x10, 0x14, 0x13)      # near-black, matches the app background
TEAL = (0x2e, 0xc4, 0xb6)
WHITE = (0xf2, 0xfb, 0xf9)   # very slightly cool, so it sits with the teal
SS = 4


def in_round_rect(x, y, x0, y0, x1, y1, r):
    cx = min(max(x, x0 + r), x1 - r)
    cy = min(max(y, y0 + r), y1 - r)
    if not (x0 <= x <= x1 and y0 <= y <= y1):
        return False
    dx, dy = x - cx, y - cy
    return dx * dx + dy * dy <= r * r or (x0 + r <= x <= x1 - r) or (y0 + r <= y <= y1 - r)


def in_tri(x, y, a, b, c):
    def side(p, q, r):
        return (q[0] - p[0]) * (r[1] - p[1]) - (q[1] - p[1]) * (r[0] - p[0])
    d1 = side(a, b, (x, y))
    d2 = side(b, c, (x, y))
    d3 = side(c, a, (x, y))
    neg = (d1 < 0) or (d2 < 0) or (d3 < 0)
    pos = (d1 > 0) or (d2 > 0) or (d3 > 0)
    return not (neg and pos)


def bubble(x, y, s, box, tail, grow=0.0):
    """A rounded rect plus a triangular tail, in units of the icon size."""
    x0, y0, x1, y1 = (v * s for v in box)
    x0 -= grow; y0 -= grow; x1 += grow; y1 += grow
    r = 0.155 * s + grow
    if in_round_rect(x, y, x0, y0, x1, y1, r):
        return True
    a, b, c = ((px * s, py * s) for px, py in tail)
    if grow:
        # Fatten the tail about its centroid so the gap is even all round.
        gx = (a[0] + b[0] + c[0]) / 3.0
        gy = (a[1] + b[1] + c[1]) / 3.0
        k = 1.0 + (grow / (0.06 * s))
        a = (gx + (a[0] - gx) * k, gy + (a[1] - gy) * k)
        b = (gx + (b[0] - gx) * k, gy + (b[1] - gy) * k)
        c = (gx + (c[0] - gx) * k, gy + (c[1] - gy) * k)
    return in_tri(x, y, a, b, c)


# Teal bubble sits back and left; the white one overlaps it front and right.
TEAL_BOX = (0.13, 0.15, 0.66, 0.52)
TEAL_TAIL = ((0.22, 0.45), (0.37, 0.52), (0.17, 0.62))
WHITE_BOX = (0.37, 0.44, 0.88, 0.80)
WHITE_TAIL = ((0.79, 0.73), (0.64, 0.80), (0.84, 0.89))


def colour_at(x, y, s):
    """Painter's order: teal, then a background gap, then white on top."""
    if not in_round_rect(x, y, 0, 0, s, s, 0.22 * s):
        return None
    if bubble(x, y, s, WHITE_BOX, WHITE_TAIL):
        return WHITE
    if bubble(x, y, s, WHITE_BOX, WHITE_TAIL, grow=0.038 * s):
        return BG                     # the separating gap
    if bubble(x, y, s, TEAL_BOX, TEAL_TAIL):
        return TEAL
    return BG


def render(size):
    rows = []
    n = SS * SS
    for py in range(size):
        row = bytearray()
        for px in range(size):
            r = g = b = cov = 0
            for sy in range(SS):
                for sx in range(SS):
                    c = colour_at(px + (sx + 0.5) / SS, py + (sy + 0.5) / SS, size)
                    if c is None:
                        continue
                    r += c[0]; g += c[1]; b += c[2]; cov += 1
            if cov:
                row += bytes((round(r / cov), round(g / cov), round(b / cov),
                              round(255 * cov / n)))
            else:
                row += b"\x00\x00\x00\x00"
        rows.append(bytes(row))
    return rows


def write_png(path, size):
    raw = b"".join(b"\x00" + r for r in render(size))

    def chunk(tag, data):
        c = tag + data
        return struct.pack(">I", len(data)) + c + struct.pack(">I", zlib.crc32(c))

    png = b"\x89PNG\r\n\x1a\n"
    png += chunk(b"IHDR", struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0))
    png += chunk(b"IDAT", zlib.compress(raw, 9))
    png += chunk(b"IEND", b"")
    with open(path, "wb") as f:
        f.write(png)
    print("wrote %s (%dx%d, %d bytes)" % (path, size, size, len(png)))


if __name__ == "__main__":
    out = sys.argv[1]
    for s in (56, 112):
        write_png("%s/icon-%d.png" % (out, s), s)
    write_png("%s/preview-256.png" % out, 256)   # just to eyeball it large
