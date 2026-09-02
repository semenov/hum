"""Regenerate requirements.txt from a known-good virtualenv.

brew install should resolve nothing: every machine gets the exact set hum was
tested against, verified by sha256. The worker reaches into mlx-lm's and
transformers' internals, so an unplanned upgrade of anything in this list is a
broken `hum start`, not a minor version bump.

    PY=$(brew --prefix)/Cellar/hum/*/libexec/venv/bin/python
    $PY -m pip list --format=freeze | grep -viE '^(pip|setuptools|wheel)==' > /tmp/freeze.txt
    python3 tools/gen_requirements.py /tmp/freeze.txt > requirements.txt

Hashes cover every file of a version that could install on an Apple Silicon
Mac, so a bump of Homebrew's python@3.12 to a later minor does not invalidate
the lockfile.
"""
import json, sys, urllib.request

def files_for(name, ver):
    url = f"https://pypi.org/pypi/{name}/{ver}/json"
    with urllib.request.urlopen(url) as r:
        data = json.load(r)
    keep = []
    for f in data["urls"]:
        fn = f["filename"]
        if fn.endswith(".tar.gz") or fn.endswith(".zip"):
            keep.append(f)                                   # sdist
        elif fn.endswith(".whl"):
            # anything installable on an Apple Silicon Mac, any cpython minor
            if "py3-none-any" in fn or ("macosx" in fn and "arm64" in fn):
                keep.append(f)
    return keep

lines = ["# Generated lockfile: exact versions with checksums, so `brew install`",
         "# resolves nothing and every machine gets the set hum was tested on.",
         "# Regenerate with: python3 tools/gen_requirements.py > requirements.txt",
         ""]
missing = []
for raw in open(sys.argv[1]):
    raw = raw.strip()
    if not raw or raw.startswith("#"):
        continue
    name, ver = raw.split("==")
    fs = files_for(name, ver)
    if not fs:
        missing.append(raw)
        continue
    hashes = sorted({f["digests"]["sha256"] for f in fs})
    lines.append(f"{name}=={ver} \\")
    for i, h in enumerate(hashes):
        end = "" if i == len(hashes) - 1 else " \\"
        lines.append(f"    --hash=sha256:{h}{end}")
    print(f"  {name:22} {ver:12} {len(hashes):2} file(s)", file=sys.stderr)
if missing:
    print("NO USABLE FILES: " + ", ".join(missing), file=sys.stderr)
    sys.exit(1)
print("\n".join(lines) + "\n")
