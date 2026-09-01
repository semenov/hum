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


def blades(cx, cy, R, n=7, sweep=1.05, hub=0.20, rim=0.92):
    """Swept impeller blades, drawn as curved strokes from hub to rim."""
    d = []
    for i in range(n):
        a, a2 = i * 2 * math.pi / n, i * 2 * math.pi / n + sweep
        p1 = (cx + R*hub*math.cos(a),  cy + R*hub*math.sin(a))
        p2 = (cx + R*rim*math.cos(a2), cy + R*rim*math.sin(a2))
        am, rm = (a + a2) / 2, R * (hub + rim) / 2 * 1.06
        c = (cx + rm*math.cos(am - 0.16), cy + rm*math.sin(am - 0.16))
        d.append(f'M {p1[0]:.1f} {p1[1]:.1f} Q {c[0]:.1f} {c[1]:.1f} {p2[0]:.1f} {p2[1]:.1f}')
    return "".join(f'<path d="{x}"/>' for x in d)


def cooler(cx, cy, R, ink_attr):
    """Blades and hub in the ink colour; the shroud picks up the gradient,
    the way a lit cooler ring does."""
    return (f'<g fill="none" stroke-width="{R*0.86*0.20:.1f}" stroke-linecap="round" {ink_attr}>'
            f'{blades(cx, cy, R*0.86)}</g>'
            f'<circle cx="{cx:.1f}" cy="{cy:.1f}" r="{R:.1f}" fill="none" '
            f'stroke="url(#g)" stroke-width="{R*0.13:.1f}"/>'
            f'<circle cx="{cx:.1f}" cy="{cy:.1f}" r="{R*0.16:.1f}" {ink_attr.replace("stroke=","fill=")}/>')


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
        style = ('<style>.ink{fill:%s}.ink-s{stroke:%s}'
                 '@media(prefers-color-scheme:dark){.ink{fill:%s}.ink-s{stroke:%s}}</style>'
                 % (INK_L, INK_L, INK_D, INK_D))
        word = "".join(f'<g class="ink" transform="translate({tx+dx:.1f},{base:.1f}) '
                       f'scale({sc},{-sc})">{glyphs}</g>' if c is None else
                       f'<g fill="{c}" transform="translate({tx+dx:.1f},{base:.1f}) '
                       f'scale({sc},{-sc})">{glyphs}</g>'
                       for c, dx in ((CY, -D), (MG, D), (None, 0)))
        fan = cooler(fx, fy, FR, 'class="ink-s"').replace('class="ink-s"/>', 'class="ink"/>')
    else:
        style = ""
        word = "".join(f'<g fill="{c}" transform="translate({tx+dx:.1f},{base:.1f}) '
                       f'scale({sc},{-sc})">{glyphs}</g>' for c, dx in ((CY, -D), (MG, D), (ink, 0)))
        fan = cooler(fx, fy, FR, f'stroke="{ink}"')
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{W:.0f}" height="{H:.0f}" '
            f'viewBox="0 0 {W:.0f} {H:.0f}" fill="none" role="img" aria-label="hum">'
            f'<defs>{GRAD}</defs>{style}{fan}{word}</svg>')


# ---- square mark: the cooler alone -----------------------------------------
M = 512
def mark():
    fan = cooler(M / 2, M / 2, M * 0.33, f'stroke="{INK_D}"')
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{M}" height="{M}" '
            f'viewBox="0 0 {M} {M}" role="img" aria-label="hum">'
            f'<defs>{GRAD}</defs>'
            f'<rect width="{M}" height="{M}" rx="112" fill="{PAPER_MARK}"/>'
            f'{fan}</svg>')


open("assets/logo.svg", "w").write(lockup())
open("assets/logo-dark.svg", "w").write(lockup(INK_D))
open("assets/mark.svg", "w").write(mark())
for n in ("logo", "logo-dark", "mark"):
    cairosvg.svg2png(url=f"assets/{n}.svg", write_to=f"assets/{n}.png", scale=2)
print(f"lockup {W:.0f}x{H:.0f}   mark {M}x{M}")
