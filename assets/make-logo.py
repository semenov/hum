"""Regenerate the hum logo.

    pip install fonttools cairosvg
    python assets/make-logo.py

Text is converted to outlines, so the resulting SVG needs no font installed.
Space Grotesk is downloaded on demand rather than vendored (SIL OFL 1.1).
"""

import math, os, urllib.request
from fontTools.ttLib import TTFont
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.boundsPen import BoundsPen
import cairosvg
os.chdir("/Users/semenov/Dev/mlx/hum")

FONT = "assets/fonts/Space_Grotesk.ttf"
if not os.path.exists(FONT):
    os.makedirs(os.path.dirname(FONT), exist_ok=True)
    css = urllib.request.urlopen(urllib.request.Request(
        "https://fonts.googleapis.com/css2?family=Space+Grotesk:wght@700",
        headers={"User-Agent": "Mozilla/5.0"})).read().decode()
    url = css.split("url(")[1].split(")")[0]
    urllib.request.urlretrieve(url, FONT)
    print("downloaded Space Grotesk")
INK, PAPER   = "#0B0D0E", "#FFFFFF"
INK_D        = "#F4F4F5"
PAPER_D      = "#0B0D0E"
G1, G2       = "#FFB020", "#FF5F45"      # amber -> coral: the machine warming up

f = TTFont(FONT); GS = f.getGlyphSet(); CMAP = f.getBestCmap()
UPM = f["head"].unitsPerEm; HMTX = f["hmtx"]

def typeset(text, size):
    """Return (svg paths, advance width, scale, ink top, ink bottom) in px."""
    s = size/UPM
    parts, x = [], 0
    top, bot = -1e9, 1e9
    for ch in text:
        gn = CMAP[ord(ch)]
        pen = SVGPathPen(GS); GS[gn].draw(pen)
        d = pen.getCommands()
        if d: parts.append(f'<path d="{d}" transform="translate({x},0)"/>')
        bp = BoundsPen(GS); GS[gn].draw(bp)
        if bp.bounds:
            top = max(top, bp.bounds[3]); bot = min(bot, bp.bounds[1])
        x += HMTX[gn][0]
    return "".join(parts), x*s, s, top*s, bot*s

def wave(x0, x1, y, amp, wl, steps=460):
    """Flat -> swell -> flat: a tone that starts, sustains and settles."""
    pts = []
    for i in range(steps+1):
        t = i/steps
        x = x0 + (x1-x0)*t
        env = math.sin(math.pi*t)**0.55
        pts.append((x, y - amp*env*math.sin(2*math.pi*(x-x0)/wl)))
    return "M " + " L ".join(f"{x:.2f} {yy:.2f}" for x, yy in pts)

GRAD = f'<defs><linearGradient id="w" x1="0" y1="0" x2="1" y2="0">' \
       f'<stop offset="0" stop-color="{G1}"/><stop offset="1" stop-color="{G2}"/></linearGradient></defs>'

# ---- horizontal lockup -----------------------------------------------------
SIZE, PAD, GAP, SW = 110, 38, 30, 11
glyphs, adv, sc, ink_top, ink_bot = typeset("hum", SIZE)
baseline = PAD + ink_top
wave_y   = baseline + GAP
W = adv + PAD*2
H = wave_y + SW/2 + PAD*0.75

def lockup(ink, paper):
    bg = f'<rect width="{W:.0f}" height="{H:.0f}" fill="{paper}"/>' if paper else ""
    return f'''<svg xmlns="http://www.w3.org/2000/svg" width="{W:.0f}" height="{H:.0f}" viewBox="0 0 {W:.0f} {H:.0f}" role="img" aria-label="hum">
{GRAD}
  {bg}
  <g fill="{ink}" transform="translate({PAD},{baseline:.1f}) scale({sc},{-sc})">{glyphs}</g>
  <path d="{wave(PAD, W-PAD, wave_y, 10, 104)}" fill="none" stroke="url(#w)" stroke-width="{SW}" stroke-linecap="round"/>
</svg>'''

# ---- square mark: the h, with the tone running under it --------------------
M, HS, MARG = 512, 300, 92
hg, hadv, hsc, htop, hbot = typeset("h", HS)
WAMP, WSW, WGAP = 15, 28, 52
group_h = htop + WGAP + WAMP + WSW/2        # ink top of h -> bottom of wave
top     = (M - group_h)/2
hbase   = top + htop                         # baseline of the h
wy      = hbase + WGAP
hx      = (M - hadv)/2

def mark(ink, paper):
    return f'''<svg xmlns="http://www.w3.org/2000/svg" width="{M}" height="{M}" viewBox="0 0 {M} {M}" role="img" aria-label="hum">
{GRAD}
  <rect width="{M}" height="{M}" rx="112" fill="{paper}"/>
  <g fill="{ink}" transform="translate({hx:.1f},{hbase:.1f}) scale({hsc},{-hsc})">{hg}</g>
  <path d="{wave(MARG, M-MARG, wy, WAMP, 168)}" fill="none" stroke="url(#w)" stroke-width="{WSW}" stroke-linecap="round"/>
</svg>'''

os.makedirs("assets", exist_ok=True)
open("assets/logo.svg","w").write(lockup(INK, PAPER))
open("assets/logo-dark.svg","w").write(lockup(INK_D, PAPER_D))
open("assets/mark.svg","w").write(mark(INK_D, PAPER_D))
for n in ("logo","logo-dark","mark"):
    cairosvg.svg2png(url=f"assets/{n}.svg", write_to=f"assets/{n}.png", scale=2)
print(f"lockup {W:.0f}x{H:.0f}   mark {M}x{M}")
