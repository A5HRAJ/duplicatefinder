#!/usr/bin/env python3
"""Generate the Duplicate Finder package icons (no external deps).

Draws the app glyph — a blue gradient rounded square with two overlapping
files behind a magnifying glass — and writes PNGs at the sizes DSM needs.
"""
import math
import struct
import sys
import zlib

# design gradient: linear 150deg  #34a8ff -> #0b6fd1
C_TOP = (0x34, 0xA8, 0xFF)
C_BOT = (0x0B, 0x6F, 0xD1)


def sdf_round_rect(px, py, cx, cy, hw, hh, r):
    dx = abs(px - cx) - (hw - r)
    dy = abs(py - cy) - (hh - r)
    ax, ay = max(dx, 0.0), max(dy, 0.0)
    outside = math.hypot(ax, ay)
    inside = min(max(dx, dy), 0.0)
    return outside + inside - r


def sdf_segment(px, py, ax, ay, bx, by):
    """Distance from point to the line segment a→b."""
    pax, pay = px - ax, py - ay
    bax, bay = bx - ax, by - ay
    denom = bax * bax + bay * bay
    h = 0.0 if denom == 0 else max(0.0, min(1.0, (pax * bax + pay * bay) / denom))
    return math.hypot(pax - bax * h, pay - bay * h)


SQRT1_2 = 0.70710678118654752


def sdf_page(px, py, cx, cy, hw, hh, r, fold):
    """A sheet of paper: rounded rect with the top-right corner cut back.

    Intersection (max) of the rounded rect with the half-plane behind the 45°
    cut, so the two edges the cut creates are sharp — a dog-eared corner reads
    as a fold only if it is a crease, and the rounded body corners are what
    make the sharpness legible as intent rather than as an artefact.
    """
    d_cut = (px - (cx + hw - fold) - (py - (cy - hh))) * SQRT1_2
    return max(sdf_round_rect(px, py, cx, cy, hw, hh, r), d_cut)


# Deliberately NOT drawn: the two inner creases that would close the folded
# triangle (the shape the filled Empty Files glyph shades in). This icon is
# line art at stroke 0.072 in unit space, and the crease corner sits only
# fold/√2 from the cut — with a fold this glyph has room for, that clearance is
# under two stroke widths, so the crease and the cut merge into a solid white
# wedge. Rendered and looked at: it reads as a chunk of damage, not a fold. The
# chamfer alone carries the page, and it is what the back sheet can show too.


def smooth(edge, d):
    # 1 inside (d<0), 0 outside, ~1px falloff
    return max(0.0, min(1.0, 0.5 - d / max(edge, 1e-6)))


def render(size, ss=4):
    """Return RGBA bytes for one icon."""
    S = size * ss
    px_buf = bytearray(S * S * 4)
    # geometry in unit space (0..1)
    bg_r = 0.225           # background corner radius
    stroke = 0.072         # white line width
    w = stroke / 2
    gap = stroke           # break width where a front element crosses a back one
    # two overlapping files (upper-left), portrait sheets with a folded corner.
    # The offset is larger than a plain rounded rect would need: a page's corner
    # cut is at the TOP-RIGHT, which is exactly where the front page lands, so
    # at a 0.075 diagonal offset the front's occlusion halo would eat the back's
    # cut and the back would read as a plain square again.
    fhw, fhh, fr = 0.133, 0.163, 0.030
    fold = 0.105                       # cut corner, ~39% of the page width —
                                       # the same proportion the Duplicate
                                       # Files rail glyph folds back
    fcx, fcy = 0.418, 0.452            # front file
    bcx, bcy = 0.296, 0.330            # back file, offset up-left
    # magnifying glass (lower-right), drawn over the files
    lcx, lcy, lr = 0.600, 0.605, 0.135  # lens centre + radius
    hs = 0.70711                        # cos/sin 45°
    hx0, hy0 = lcx + lr * hs, lcy + lr * hs
    hx1, hy1 = lcx + (lr + 0.155) * hs, lcy + (lr + 0.155) * hs
    wh = w * 1.05                       # handle half-width

    for j in range(S):
        v = (j + 0.5) / S
        for i in range(S):
            u = (i + 0.5) / S
            aa = 1.0 / S
            # --- background rounded square ---
            d_bg = sdf_round_rect(u, v, 0.5, 0.5, 0.5 - 0.02, 0.5 - 0.02, bg_r)
            a_bg = smooth(aa * 1.5, d_bg)
            if a_bg <= 0.0:
                continue
            # gradient along 150deg-ish diagonal
            t = max(0.0, min(1.0, (u * 0.35 + v * 0.65)))
            r = C_TOP[0] + (C_BOT[0] - C_TOP[0]) * t
            g = C_TOP[1] + (C_BOT[1] - C_TOP[1]) * t
            b = C_TOP[2] + (C_BOT[2] - C_TOP[2]) * t

            # --- glyph: two files behind a magnifying glass (white line art) ---
            d_front = sdf_page(u, v, fcx, fcy, fhw, fhh, fr, fold)
            d_back = sdf_page(u, v, bcx, bcy, fhw, fhh, fr, fold)
            dc = math.hypot(u - lcx, v - lcy)
            d_seg = sdf_segment(u, v, hx0, hy0, hx1, hy1)

            # magnifier (lens ring + handle) sits on top of everything
            a_glass = max(smooth(aa * 1.5, abs(dc - lr) - w),
                          smooth(aa * 1.5, d_seg - wh))
            # glass footprint (+gap) breaks the file strokes behind it
            glass_occ = smooth(aa * 1.5, min(dc - (lr + gap), d_seg - (wh + gap)))
            # the front file breaks the back file where its body (+gap) sits
            front_occ = smooth(aa * 1.5, d_front - gap)

            a_front = smooth(aa * 1.5, abs(d_front) - w) * (1.0 - glass_occ)
            a_back = smooth(aa * 1.5, abs(d_back) - w) * (1.0 - front_occ) * (1.0 - glass_occ)
            a_glyph = max(a_glass, a_front, a_back)
            if a_glyph > 0:
                r = r + (255 - r) * a_glyph
                g = g + (255 - g) * a_glyph
                b = b + (255 - b) * a_glyph

            o = (j * S + i) * 4
            px_buf[o] = int(r + 0.5)
            px_buf[o + 1] = int(g + 0.5)
            px_buf[o + 2] = int(b + 0.5)
            px_buf[o + 3] = int(255 * a_bg + 0.5)

    # box downsample ss×ss
    out = bytearray(size * size * 4)
    n = ss * ss
    for j in range(size):
        for i in range(size):
            acc = [0, 0, 0, 0]
            for jj in range(ss):
                base = ((j * ss + jj) * S + i * ss) * 4
                for ii in range(ss):
                    o = base + ii * 4
                    a = px_buf[o + 3]
                    acc[0] += px_buf[o] * a
                    acc[1] += px_buf[o + 1] * a
                    acc[2] += px_buf[o + 2] * a
                    acc[3] += a
            o = (j * size + i) * 4
            if acc[3]:
                out[o] = acc[0] // acc[3]
                out[o + 1] = acc[1] // acc[3]
                out[o + 2] = acc[2] // acc[3]
            out[o + 3] = acc[3] // n
    return bytes(out)


def write_png(path, size, rgba):
    def chunk(tag, data):
        c = struct.pack('>I', len(data)) + tag + data
        return c + struct.pack('>I', zlib.crc32(tag + data) & 0xFFFFFFFF)

    raw = b''
    stride = size * 4
    for j in range(size):
        raw += b'\x00' + rgba[j * stride:(j + 1) * stride]
    png = b'\x89PNG\r\n\x1a\n'
    png += chunk(b'IHDR', struct.pack('>IIBBBBB', size, size, 8, 6, 0, 0, 0))
    png += chunk(b'IDAT', zlib.compress(raw, 9))
    png += chunk(b'IEND', b'')
    with open(path, 'wb') as f:
        f.write(png)


def main():
    outdir = sys.argv[1] if len(sys.argv) > 1 else '.'
    jobs = [
        (outdir + '/PACKAGE_ICON.PNG', 64),
        (outdir + '/PACKAGE_ICON_256.PNG', 256),
    ]
    for s in (16, 24, 32, 48, 64, 72, 256):
        jobs.append((outdir + '/icon_%d.png' % s, s))
    for path, size in jobs:
        write_png(path, size, render(size))
        print('wrote', path)


if __name__ == '__main__':
    main()
