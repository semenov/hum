"""Regenerate the hum logo.

    pip install fonttools cairosvg
    python assets/make-logo.py

A stylised GPU cooler sits left of the wordmark — the thing that actually hums.
The wordmark is Orbitron split into cyan and magenta channels, which reads as
vibration. Text is converted to outlines, so the SVGs need no font installed;
Orbitron is fetched on demand (SIL OFL 1.1) rather than vendored.

Outputs:
    logo.svg       transparent, ink follows the viewer's colour scheme
    logo-dark.svg  transparent, fixed light ink, for the README's <picture>
    mark.svg       opaque rounded square, the cooler alone — app icon / avatar
"""
import math, os, urllib.request
from fontTools.ttLib import TTFont
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.boundsPen import BoundsPen
import cairosvg

os.chdir(os.path.dirname(os.path.abspath(__file__)) + "/..")
FONT = "assets/fonts/Orbitron.ttf"
if not os.path.exists(FONT):
    os.makedirs(os.path.dirname(FONT), exist_ok=True)
    css = urllib.request.urlopen(urllib.request.Request(
        "https://fonts.googleapis.com/css2?family=Orbitron:wght@800",
        headers={"User-Agent": "Mozilla/5.0"})).read().decode()
    urllib.request.urlretrieve(css.split("url(")[1].split(")")[0], FONT)
    print("downloaded Orbitron")

INK_L, INK_D = "#0B0D0E", "#F2F4F8"
# One ink cannot be dark enough for white and light enough for #0d1117; the best
# any single value manages on both is about 4.3:1, and this is that value. Used
# where a renderer supports neither <picture> nor a colour scheme.
INK_AUTO = "#787888"
PAPER_MARK   = "#08090C"
CY, MG       = "#22D3EE", "#F0509A"

f = TTFont(FONT); GS = f.getGlyphSet(); CMAP = f.getBestCmap()
UPM = f["head"].unitsPerEm; HMTX = f["hmtx"]

GRAD = (f'<linearGradient id="g" x1="0" y1="0" x2="1" y2="1">'
        f'<stop offset="0" stop-color="{CY}"/><stop offset="1" stop-color="{MG}"/></linearGradient>')


def typeset(text, size, tracking=0):
    s = size / UPM
    parts, x, top, bot = [], 0, -1e9, 1e9
    for ch in text:
        gn = CMAP[ord(ch)]
        pen = SVGPathPen(GS); GS[gn].draw(pen); d = pen.getCommands()
        if d: parts.append(f'<path d="{d}" transform="translate({x},0)"/>')
        bp = BoundsPen(GS); GS[gn].draw(bp)
        if bp.bounds:
            top = max(top, bp.bounds[3]); bot = min(bot, bp.bounds[1])
        x += HMTX[gn][0] + tracking / s
    return "".join(parts), x * s, s, top * s, bot * s


def _pt(cx, cy, r, a):
    return cx + r * math.cos(a), cy + r * math.sin(a)


def _circle(cx, cy, r):
    """A full circle as two half arcs. One 360-degree arc is degenerate and
    breaks bounding-box maths in some renderers."""
    return (f'M {cx-r:.2f} {cy:.2f} A {r:.2f} {r:.2f} 0 1 0 {cx+r:.2f} {cy:.2f} '
            f'A {r:.2f} {r:.2f} 0 1 0 {cx-r:.2f} {cy:.2f} Z')


def _blades(cx, cy, R, n=7, sweep=1.0, hub=.22, rim=.86, win=.30, wout=.16):
    """Swept blades as filled shapes: narrow at the hub, wide at the rim."""
    out = []
    for i in range(n):
        a = i * 2 * math.pi / n
        a2 = a + sweep
        p1 = _pt(cx, cy, R*hub, a - win);  p2 = _pt(cx, cy, R*rim, a2 - wout)
        p3 = _pt(cx, cy, R*rim, a2 + wout); p4 = _pt(cx, cy, R*hub, a + win)
        c1 = _pt(cx, cy, R*(hub+rim)/2*1.10, (a+a2)/2 - win*.7)
        c2 = _pt(cx, cy, R*(hub+rim)/2*1.02, (a+a2)/2 + win*.9)
        out.append(f'M {p1[0]:.1f} {p1[1]:.1f} Q {c1[0]:.1f} {c1[1]:.1f} {p2[0]:.1f} {p2[1]:.1f} '
                   f'A {R*rim:.1f} {R*rim:.1f} 0 0 1 {p3[0]:.1f} {p3[1]:.1f} '
                   f'Q {c2[0]:.1f} {c2[1]:.1f} {p4[0]:.1f} {p4[1]:.1f} Z')
    return " ".join(out)


def _impeller(cx, cy, R):
    """A solid disc with the blades knocked out of it."""
    return _circle(cx, cy, R) + " " + _blades(cx, cy, R)


def _frame(cx, cy, R):
    """A case-fan frame: rounded square, round opening, four mounting holes."""
    s = R * 2.05
    x, y = cx - s/2, cy - s/2
    rr, hole, ins = s*.15, s*.072, s*.16
    outer = (f'M {x:.1f} {y+rr:.1f} A {rr:.1f} {rr:.1f} 0 0 1 {x+rr:.1f} {y:.1f} '
             f'L {x+s-rr:.1f} {y:.1f} A {rr:.1f} {rr:.1f} 0 0 1 {x+s:.1f} {y+rr:.1f} '
             f'L {x+s:.1f} {y+s-rr:.1f} A {rr:.1f} {rr:.1f} 0 0 1 {x+s-rr:.1f} {y+s:.1f} '
             f'L {x+rr:.1f} {y+s:.1f} A {rr:.1f} {rr:.1f} 0 0 1 {x:.1f} {y+s-rr:.1f} Z')
    screws = " ".join(_circle(x+dx, y+dy, hole) for dx, dy in
                      ((ins, ins), (s-ins, ins), (ins, s-ins), (s-ins, s-ins)))
    return f'{outer} {_circle(cx, cy, R*.93)} {screws}'


def cooler(cx, cy, R, ink_attr):
    """The frame carries the gradient — a lit fan ring — and the impeller is cut
    from the ink colour, so it flips cleanly between light and dark themes."""
    return (f'<path fill="url(#g)" fill-rule="evenodd" d="{_frame(cx, cy, R*0.95)}"/>'
            f'<path fill-rule="evenodd" d="{_impeller(cx, cy, R*0.70)}" '
            f'{ink_attr.replace("stroke=", "fill=").replace("class=\'ink-s\'", "class=\'ink\'")}/>')


# ---- horizontal lockup -----------------------------------------------------
SIZE, PAD, D = 96, 44, 6
glyphs, adv, sc, top, bot = typeset("HUM", SIZE, tracking=7)
FR, GAP = top * 0.68, top * 0.42
fx = PAD + FR
tx = PAD + FR * 2 + GAP
W, H = tx + adv + PAD, top + PAD * 2
base, fy = PAD + top, PAD + top / 2


def lockup(ink=None):
    """ink=None emits a self-adapting file: the ink colour follows the viewer's
    colour scheme, so the logo is still right if used without <picture>."""
    if ink is None:
        # Default to the mid tone, then sharpen for each scheme. A renderer
        # that ignores the media queries still gets something legible either
        # way, instead of black text on a black page.
        style = ('<style>.ink{fill:%s}'
                 '@media(prefers-color-scheme:light){.ink{fill:%s}}'
                 '@media(prefers-color-scheme:dark){.ink{fill:%s}}</style>'
                 % (INK_AUTO, INK_L, INK_D))
        word = "".join(f'<g class="ink" transform="translate({tx+dx:.1f},{base:.1f}) '
                       f'scale({sc},{-sc})">{glyphs}</g>' if c is None else
                       f'<g fill="{c}" transform="translate({tx+dx:.1f},{base:.1f}) '
                       f'scale({sc},{-sc})">{glyphs}</g>'
                       for c, dx in ((CY, -D), (MG, D), (None, 0)))
        fan = cooler(fx, fy, FR, 'class="ink"')
    else:
        style = ""
        word = "".join(f'<g fill="{c}" transform="translate({tx+dx:.1f},{base:.1f}) '
                       f'scale({sc},{-sc})">{glyphs}</g>' for c, dx in ((CY, -D), (MG, D), (ink, 0)))
        fan = cooler(fx, fy, FR, f'fill="{ink}"')
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{W:.0f}" height="{H:.0f}" '
            f'viewBox="0 0 {W:.0f} {H:.0f}" fill="none" role="img" aria-label="hum">'
            f'<defs>{GRAD}</defs>{style}{fan}{word}</svg>')


# ---- square mark: the cooler alone -----------------------------------------
M = 512
def mark():
    """The fan frame *is* the icon shape — no rounded square inside a rounded
    square. Gradient body, dark opening and mounting holes, light impeller."""
    c, R = M / 2, M * 0.40
    s_ = R * 2.05
    ins, hole = s_ * .16, s_ * .072
    x0, y0 = c - s_/2, c - s_/2
    screws = " ".join(_circle(x0+dx, y0+dy, hole) for dx, dy in
                      ((ins, ins), (s_-ins, ins), (ins, s_-ins), (s_-ins, s_-ins)))
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{M}" height="{M}" '
            f'viewBox="0 0 {M} {M}" role="img" aria-label="hum">'
            f'<defs>{GRAD}</defs>'
            f'<rect width="{M}" height="{M}" rx="{M*0.22:.0f}" fill="url(#g)"/>'
            f'<path fill="{PAPER_MARK}" fill-rule="evenodd" '
            f'd="{_circle(c, c, R*0.93)} {screws}"/>'
            f'<path fill="{INK_D}" fill-rule="evenodd" d="{_impeller(c, c, R*0.72)}"/></svg>')


open("assets/logo.svg", "w").write(lockup())
open("assets/logo-dark.svg", "w").write(lockup(INK_D))
open("assets/logo-auto.svg", "w").write(lockup(INK_AUTO))
open("assets/mark.svg", "w").write(mark())
for n in ("logo", "logo-dark", "logo-auto", "mark"):
    cairosvg.svg2png(url=f"assets/{n}.svg", write_to=f"assets/{n}.png", scale=2)
print(f"lockup {W:.0f}x{H:.0f}   mark {M}x{M}")
