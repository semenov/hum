"""Generates assets/demo.cast, the source for the README's demo.gif.

    python3 assets/gen_cast.py
    agg --theme github-dark --font-size 26 --cols 96 --rows 30 \
        --last-frame-duration 4 assets/demo.cast assets/demo.gif

--last-frame-duration holds on the answer before the loop restarts; without it
the result flashes past in the time it takes to read one line.

The cast is written rather than recorded, so the pacing is chosen instead of
waited out — a real recording spends twenty seconds compiling and nine more
loading weights, which is honest and unwatchable. Every line of output below is
copied verbatim from an actual run; only the timing is authored.

(agg: `brew install agg`. To capture a real session instead, `brew install
asciinema` then `asciinema rec assets/demo.cast -c zsh`.)
"""
import json
import os

E = "\x1b"


def c(*codes):
    return E + "[" + ";".join(map(str, codes)) + "m"


def rgb(r, g, b):
    return f"{E}[38;2;{r};{g};{b}m"


BLUE = rgb(78, 168, 255)      # the CLI's blue, used for values
GREEN = rgb(61, 220, 132)     # the CLI's green, used for the running dot
DIM, BOLD, RST = c(2), c(1), c(0)
PROMPT = GREEN + "❯" + RST + " "

READY = (
    f"\r\n  {GREEN}●{RST} {BOLD}Hum is ready.{RST}\r\n\r\n"
    f"    {DIM}The server is listening on http://127.0.0.1:4242 and speaks the{RST}\r\n"
    f"    {DIM}OpenAI chat completions API. Point OpenCode, your editor, or any{RST}\r\n"
    f"    {DIM}OpenAI SDK at it — no API key is required.{RST}\r\n\r\n"
    f"    {DIM}Model{RST}       {BLUE}Qwen3.6 35B-A3B{RST}\r\n"
    f"    {DIM}Logs{RST}        {BLUE}~/.hum/hum.log{RST}\r\n\r\n"
    f"    {DIM}Stop it again with{RST} {BLUE}hum stop{RST}\r\n\r\n"
)

events = [
    (0.3, PROMPT),
    (0.9, "brew tap semenov/hum"),
    (1.5, "\r\n"),
    (1.9, "==> Tapping semenov/hum\r\nTapped 1 formula (14 files, 9.7KB).\r\n"),
    (2.2, PROMPT),
    (2.7, "brew trust semenov/hum"),
    (3.1, "\r\n"),
    (3.4, "Trusted tap: semenov/hum\r\n"),
    (3.7, PROMPT),
    (4.2, "brew install --HEAD hum"),
    (4.7, "\r\n"),
    (5.0, "==> Installing hum from semenov/hum\r\n"),
    (5.4, f"==> {DIM}python3.12 -m venv .../libexec/venv{RST}\r\n"),
    (5.9, f"==> {DIM}pip install --quiet --require-hashes -r requirements.txt{RST}\r\n"),
    (6.6, f"==> {DIM}go build -ldflags ...{RST}\r\n"),
    (7.2, "🍺  /opt/homebrew/Cellar/hum/HEAD: 431.8MB, built in 16 seconds\r\n"),
    (7.7, PROMPT),
    (8.3, "hum start"),
    (8.8, "\r\n"),
    (9.2, f"  {BLUE}⠹{RST} Loading Qwen3.6 35B-A3B {DIM}2s{RST}\r"),
    (9.7, f"  {BLUE}⠼{RST} Loading Qwen3.6 35B-A3B {DIM}5s{RST}\r"),
    (10.2, f"{E}[2K\r" + READY),
    (10.6, PROMPT),
    (11.3, "hum ask 'name three roman emperors, one line each'"),
    (11.9, "\r\n"),
    (12.6, "Augustus\r\n"),
    (12.9, "Trajan\r\n"),
    (13.2, "Marcus Aurelius\r\n"),
    (13.6, PROMPT),
]

out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "demo.cast")
with open(out, "w") as f:
    f.write(json.dumps({"version": 2, "width": 96, "height": 30,
                        "title": "hum", "env": {"TERM": "xterm-256color"}}) + "\n")
    for t, d in events:
        f.write(json.dumps([t, "o", d]) + "\n")
print("wrote", out)
