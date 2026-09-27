"""Draws the peerly icon: a world handed round a cycle of two arrows.

Writes icon.ico (embedded into peerly.exe, see winres.json) and
../../ui/web/favicon.svg from the same geometry. Needs Pillow:

    python client/app/winres/make_icon.py

Small sizes get thicker strokes instead of being scaled down, so the
16 px icon in the taskbar and the Start menu stays readable.
"""
import math
import os
from PIL import Image, ImageDraw

HERE = os.path.dirname(os.path.abspath(__file__))
TOP, BOTTOM = (0x3A, 0x85, 0x60), (0x27, 0x5E, 0x42)  # around --accent #2f6f4f
WHITE = (255, 255, 255, 255)
SIZES = [256, 64, 48, 40, 32, 24, 20, 16]


def geometry(size):
    """Everything in units of a 100-wide canvas, tuned per output size."""
    small = size <= 24
    return {
        "margin": 3 if size >= 32 else 1,
        "radius": 22,
        "ring": 30 if small else 29,
        "stroke": 11 if small else 8.5,
        "gap": 30 if small else 34,  # degrees of empty ring left for each arrowhead
        "head": 0 if size <= 16 else (13 if small else 15),
        "dot": 13 if small else 12,
    }


def arcs(g):
    # Two arcs travelling clockwise; each ends in an arrowhead that points at
    # the start of the other, so the world is handed round and round.
    half = 180 - g["gap"]
    return [(-90 + g["gap"] / 2, -90 + g["gap"] / 2 + half),
            (90 + g["gap"] / 2, 90 + g["gap"] / 2 + half)]


def arrowhead(cx, cy, r, angle_deg, length, width):
    t = math.radians(angle_deg)
    px, py = cx + r * math.cos(t), cy + r * math.sin(t)
    tx, ty = -math.sin(t), math.cos(t)   # clockwise tangent (y points down)
    rx, ry = math.cos(t), math.sin(t)    # radial
    return [(px + tx * length, py + ty * length),
            (px + rx * width, py + ry * width),
            (px - rx * width, py - ry * width)]


def render(size):
    g = geometry(size)
    ss = 8
    n = size * ss
    k = n / 100.0
    img = Image.new("RGBA", (n, n), (0, 0, 0, 0))

    grad = Image.new("RGBA", (n, n))
    gd = ImageDraw.Draw(grad)
    for y in range(n):
        f = y / (n - 1)
        gd.line([(0, y), (n, y)], fill=tuple(round(a + (b - a) * f) for a, b in zip(TOP, BOTTOM)) + (255,))
    mask = Image.new("L", (n, n), 0)
    m = g["margin"] * k
    ImageDraw.Draw(mask).rounded_rectangle([m, m, n - m, n - m], radius=g["radius"] * k, fill=255)
    img.paste(grad, (0, 0), mask)

    d = ImageDraw.Draw(img)
    c = n / 2
    r = g["ring"] * k
    w = g["stroke"] * k
    for a0, a1 in arcs(g):
        # the head covers the last part of the arc, so stop the stroke short of it
        end = a1 - (math.degrees(g["head"] * 0.35 / g["ring"]) if g["head"] else 0)
        d.arc([c - r - w / 2, c - r - w / 2, c + r + w / 2, c + r + w / 2], a0, end, fill=WHITE, width=round(w))
        for a in (a0,):
            t = math.radians(a)
            x, y = c + r * math.cos(t), c + r * math.sin(t)
            d.ellipse([x - w / 2, y - w / 2, x + w / 2, y + w / 2], fill=WHITE)
        if g["head"]:
            d.polygon(arrowhead(c, c, r, a1 - math.degrees(g["head"] * 0.35 / g["ring"]),
                                g["head"] * k, g["head"] * 0.62 * k), fill=WHITE)
        else:
            t = math.radians(a1)
            x, y = c + r * math.cos(t), c + r * math.sin(t)
            d.ellipse([x - w / 2, y - w / 2, x + w / 2, y + w / 2], fill=WHITE)
    dr = g["dot"] * k
    d.ellipse([c - dr, c - dr, c + dr, c + dr], fill=WHITE)
    return img.resize((size, size), Image.LANCZOS)


def svg():
    g = geometry(256)
    c, r, w = 50, g["ring"], g["stroke"]
    parts = []
    for a0, a1 in arcs(g):
        end = a1 - math.degrees(g["head"] * 0.35 / g["ring"])
        p0 = (c + r * math.cos(math.radians(a0)), c + r * math.sin(math.radians(a0)))
        p1 = (c + r * math.cos(math.radians(end)), c + r * math.sin(math.radians(end)))
        large = 1 if end - a0 > 180 else 0
        parts.append(f'<path d="M{p0[0]:.2f} {p0[1]:.2f} A{r} {r} 0 {large} 1 {p1[0]:.2f} {p1[1]:.2f}" '
                     f'fill="none" stroke="#fff" stroke-width="{w}" stroke-linecap="round"/>')
        pts = arrowhead(c, c, r, end, g["head"], g["head"] * 0.62)
        parts.append('<path d="M' + " L".join(f"{x:.2f} {y:.2f}" for x, y in pts) + 'Z" fill="#fff"/>')
    m = g["margin"]
    return (
        '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">'
        '<defs><linearGradient id="g" x1="0" y1="0" x2="0" y2="1">'
        f'<stop offset="0" stop-color="#{TOP[0]:02x}{TOP[1]:02x}{TOP[2]:02x}"/>'
        f'<stop offset="1" stop-color="#{BOTTOM[0]:02x}{BOTTOM[1]:02x}{BOTTOM[2]:02x}"/>'
        '</linearGradient></defs>'
        f'<rect x="{m}" y="{m}" width="{100 - 2 * m}" height="{100 - 2 * m}" rx="{g["radius"]}" fill="url(#g)"/>'
        + "".join(parts)
        + f'<circle cx="{c}" cy="{c}" r="{g["dot"]}" fill="#fff"/></svg>\n'
    )


if __name__ == "__main__":
    images = [render(s) for s in SIZES]
    images[0].save(os.path.join(HERE, "icon.ico"), sizes=[(s, s) for s in SIZES], append_images=images[1:])
    with open(os.path.join(HERE, "..", "..", "ui", "web", "favicon.svg"), "w", newline="\n") as f:
        f.write(svg())
    print("wrote icon.ico", SIZES, "and favicon.svg")
