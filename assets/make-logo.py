"""Regenerate the hum logo.

    pip install fonttools cairosvg
    python assets/make-logo.py

The wordmark is set in Orbitron and split into cyan/magenta channels — the
mark reads as vibration, which is what a hum is. Text is converted to outlines,
so the SVGs need no font installed. Orbitron is fetched on demand (SIL OFL 1.1)
rather than vendored.
"""
import os, urllib.request
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

INK_L, PAPER_L = "#0B0D0E", "#FFFFFF"
INK_D, PAPER_D = "#F2F4F8", "#08090C"
CY, MG = "#22D3EE", "#F0509A"

f = TTFont(FONT); GS = f.getGlyphSet(); CMAP = f.getBestCmap()
UPM = f["head"].unitsPerEm; HMTX = f["hmtx"]

def typeset(text, size, tracking=0):
    s = size/UPM
    parts, x = [], 0
    top, bot = -1e9, 1e9
    for ch in text:
        gn = CMAP[ord(ch)]
        pen = SVGPathPen(GS); GS[gn].draw(pen); d = pen.getCommands()
        if d: parts.append(f'<path d="{d}" transform="translate({x},0)"/>')
        bp = BoundsPen(GS); GS[gn].draw(bp)
        if bp.bounds:
            top = max(top, bp.bounds[3]); bot = min(bot, bp.bounds[1])
        x += HMTX[gn][0] + tracking/s
    return "".join(parts), x*s, s, top*s, bot*s

def split(glyphs, x, base, s, ink, d):
    """Three stacked passes: cyan behind left, magenta behind right, ink on top.
    No blend modes — those render inconsistently outside a browser."""
    lay = lambda dx, fill: (f'<g fill="{fill}" transform="translate({x+dx:.1f},{base:.1f}) '
                            f'scale({s},{-s})">{glyphs}</g>')
    return lay(-d, CY) + lay(d, MG) + lay(0, ink)

# ---- horizontal lockup -----------------------------------------------------
SIZE, PAD, D = 96, 46, 6
glyphs, adv, sc, top, bot = typeset("HUM", SIZE, tracking=7)
W, H = adv + PAD*2, top + PAD*2
base = PAD + top

def lockup(ink, paper):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{W:.0f}" height="{H:.0f}" '
            f'viewBox="0 0 {W:.0f} {H:.0f}" role="img" aria-label="hum">'
            f'<rect width="{W:.0f}" height="{H:.0f}" fill="{paper}"/>'
            f'{split(glyphs, PAD, base, sc, ink, D)}</svg>')

# ---- square mark: H, same vibration ---------------------------------------
M, HS, DM = 512, 300, 16
hg, hadv, hsc, htop, hbot = typeset("H", HS)
hx, hbase = (M - hadv)/2, (M + htop)/2

def mark(ink, paper):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{M}" height="{M}" '
            f'viewBox="0 0 {M} {M}" role="img" aria-label="hum">'
            f'<rect width="{M}" height="{M}" rx="112" fill="{paper}"/>'
            f'{split(hg, hx, hbase, hsc, ink, DM)}</svg>')

open("assets/logo.svg","w").write(lockup(INK_L, PAPER_L))
open("assets/logo-dark.svg","w").write(lockup(INK_D, PAPER_D))
open("assets/mark.svg","w").write(mark(INK_D, PAPER_D))
for n in ("logo","logo-dark","mark"):
    cairosvg.svg2png(url=f"assets/{n}.svg", write_to=f"assets/{n}.png", scale=2)
print(f"lockup {W:.0f}x{H:.0f}   mark {M}x{M}")
