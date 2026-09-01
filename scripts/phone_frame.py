#!/usr/bin/env python3
"""Wrap raw browser screenshots in a phone frame for the README.

The shoot (``make screenshots``) renders the remote at phone size into a
directory of plain PNGs. This turns each one into a picture of a phone:
a status bar and a home indicator in the page's own colours, rounded
screen corners, a dark bezel with side buttons and a soft shadow, on a
transparent background so it sits well on a light or a dark page.

    python3 scripts/phone_frame.py <raw-dir> <out-dir> [--width 480]

Needs Pillow (``pip install --user Pillow``).
"""

import argparse
import os
import sys

try:
    from PIL import Image, ImageDraw, ImageFilter, ImageFont
except ImportError:  # pragma: no cover - a missing dependency, not a bug
    sys.exit("phone_frame.py needs Pillow: pip install --user Pillow")

# The CSS-pixel viewport the shoot renders at; everything below is in
# those units and scaled by the screenshot's own device pixel ratio.
CSS_WIDTH = 390

STATUS_H = 46      # status bar strip above the page
HOME_H = 30        # home indicator strip below the page
BEZEL = 11         # dark frame around the screen
SCREEN_R = 42      # screen corner radius
BODY_R = 53        # outer corner radius
SHADOW_BLUR = 18
SHADOW_DY = 9
MARGIN = 16        # transparent room for the shadow

BODY = (26, 25, 24, 255)       # the phone's dark body
RIM = (72, 69, 66, 255)        # a lighter hairline at its edge
SHADOW = (0, 0, 0, 90)

CLOCK = "21:34"

FONTS = (
    "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
    "/usr/share/fonts/truetype/liberation/LiberationSans-Bold.ttf",
    "/usr/share/fonts/truetype/noto/NotoSans-Bold.ttf",
)


def load_font(size):
    for path in FONTS:
        if os.path.exists(path):
            return ImageFont.truetype(path, size)
    return ImageFont.load_default()


def ink_for(bg):
    """A foreground colour that reads on the sampled page background."""
    r, g, b = bg[:3]
    light = 0.299 * r + 0.587 * g + 0.114 * b > 140
    return (28, 26, 24, 255) if light else (240, 238, 234, 255)


def rounded_mask(size, radius, scale):
    mask = Image.new("L", size, 0)
    ImageDraw.Draw(mask).rounded_rectangle(
        (0, 0, size[0] - 1, size[1] - 1), radius=int(radius * scale), fill=255)
    return mask


def draw_status_bar(img, box, ink, s):
    """Clock on the left, signal / wi-fi / battery on the right."""
    d = ImageDraw.Draw(img)
    x0, y0, x1, y1 = box
    mid = (y0 + y1) / 2

    font = load_font(max(8, int(15 * s)))
    d.text((x0 + 26 * s, mid), CLOCK, font=font, fill=ink, anchor="lm")

    right = x1 - 24 * s

    # Battery: a rounded shell with a nub and a fill.
    bw, bh = 24 * s, 12 * s
    bx1, by0 = right, mid - bh / 2
    bx0 = bx1 - bw
    d.rounded_rectangle((bx0, by0, bx1, by0 + bh), radius=3 * s, outline=ink, width=max(1, int(1.4 * s)))
    d.rounded_rectangle((bx1 + 1.5 * s, mid - 3 * s, bx1 + 3.5 * s, mid + 3 * s), radius=1 * s, fill=ink)
    pad = 2.2 * s
    d.rounded_rectangle((bx0 + pad, by0 + pad, bx0 + pad + (bw - 2 * pad) * 0.72, by0 + bh - pad),
                        radius=1.4 * s, fill=ink)

    # Wi-Fi: three arcs and a dot.
    wx, wy = bx0 - 14 * s, mid + 4.5 * s
    for i, r in enumerate((11.5 * s, 7.5 * s)):
        d.arc((wx - r, wy - r, wx + r, wy + r), start=218, end=322, fill=ink, width=max(1, int(1.7 * s)))
    d.ellipse((wx - 1.7 * s, wy - 1.7 * s, wx + 1.7 * s, wy + 1.7 * s), fill=ink)

    # Cellular: four rising bars.
    sx = wx - 14 * s - 17 * s
    for i in range(4):
        h = (4 + i * 2.7) * s
        bx = sx + i * 4.6 * s
        d.rounded_rectangle((bx, mid + 5.5 * s - h, bx + 2.9 * s, mid + 5.5 * s), radius=1 * s, fill=ink)


def frame(src_path, dst_path, out_width):
    shot = Image.open(src_path).convert("RGBA")
    s = shot.width / CSS_WIDTH  # the shoot's device pixel ratio

    top_bg = shot.getpixel((shot.width // 2, 1))
    bottom_bg = shot.getpixel((shot.width // 2, shot.height - 2))

    status_h, home_h = int(STATUS_H * s), int(HOME_H * s)
    screen_w, screen_h = shot.width, shot.height + status_h + home_h

    screen = Image.new("RGBA", (screen_w, screen_h), top_bg)
    ImageDraw.Draw(screen).rectangle((0, screen_h - home_h, screen_w, screen_h), fill=bottom_bg)
    screen.paste(shot, (0, status_h))

    draw_status_bar(screen, (0, 0, screen_w, status_h), ink_for(top_bg), s)

    # Home indicator, in the page's own foreground colour.
    d = ImageDraw.Draw(screen)
    hw, hh = 134 * s, 5 * s
    hy = screen_h - home_h / 2 - hh / 2
    ink = ink_for(bottom_bg)
    d.rounded_rectangle(((screen_w - hw) / 2, hy, (screen_w + hw) / 2, hy + hh),
                        radius=hh / 2, fill=(ink[0], ink[1], ink[2], 130))

    screen.putalpha(rounded_mask((screen_w, screen_h), SCREEN_R, s))

    bezel = int(BEZEL * s)
    body_w, body_h = screen_w + 2 * bezel, screen_h + 2 * bezel
    margin = int(MARGIN * s)
    canvas = Image.new("RGBA", (body_w + 2 * margin, body_h + 2 * margin), (0, 0, 0, 0))

    # Soft shadow under the body.
    shadow = Image.new("RGBA", canvas.size, (0, 0, 0, 0))
    ImageDraw.Draw(shadow).rounded_rectangle(
        (margin, margin + SHADOW_DY * s, margin + body_w, margin + body_h + SHADOW_DY * s),
        radius=int(BODY_R * s), fill=SHADOW)
    canvas.alpha_composite(shadow.filter(ImageFilter.GaussianBlur(SHADOW_BLUR * s)))

    body = Image.new("RGBA", (body_w, body_h), (0, 0, 0, 0))
    bd = ImageDraw.Draw(body)
    bd.rounded_rectangle((0, 0, body_w - 1, body_h - 1), radius=int(BODY_R * s), fill=BODY)
    bd.rounded_rectangle((0, 0, body_w - 1, body_h - 1), radius=int(BODY_R * s),
                         outline=RIM, width=max(1, int(1.6 * s)))
    body.alpha_composite(screen, (bezel, bezel))
    canvas.alpha_composite(body, (margin, margin))

    # Side buttons, sitting just proud of the body's edge.
    cd = ImageDraw.Draw(canvas)
    bx = margin
    for top, length in ((118, 30), (170, 56), (238, 56)):  # silence, volume up, volume down
        cd.rounded_rectangle((bx - 2.5 * s, margin + top * s, bx + 1, margin + (top + length) * s),
                             radius=1.6 * s, fill=RIM)
    cd.rounded_rectangle((margin + body_w - 1, margin + 190 * s, margin + body_w + 2.5 * s,
                          margin + (190 + 84) * s), radius=1.6 * s, fill=RIM)

    if out_width and canvas.width != out_width:
        h = round(canvas.height * out_width / canvas.width)
        canvas = canvas.resize((out_width, h), Image.LANCZOS)

    canvas.save(dst_path, optimize=True)
    return canvas.size


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("src", help="directory of raw screenshots")
    ap.add_argument("dst", help="directory to write the framed pictures into")
    ap.add_argument("--width", type=int, default=840, help="output width in pixels (0 keeps full size)")
    args = ap.parse_args()

    os.makedirs(args.dst, exist_ok=True)
    names = sorted(n for n in os.listdir(args.src) if n.endswith(".png"))
    if not names:
        sys.exit(f"no screenshots in {args.src}")
    for name in names:
        size = frame(os.path.join(args.src, name), os.path.join(args.dst, name), args.width)
        print(f"{name}: {size[0]}x{size[1]}")


if __name__ == "__main__":
    main()
