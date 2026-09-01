#!/bin/sh
# Regenerate assets/demo.gif.
#
# Records a real install and run, then drops frames where the screen did not
# change — the waits are genuine, so speeding the whole thing up would misstate
# how long they take, but sitting on a still frame for seven seconds is no use
# to anyone either.
set -e
cd "$(dirname "$0")/.."

hum stop 2>/dev/null || true
brew uninstall hum 2>/dev/null || true
brew untap --force semenov/hum 2>/dev/null || true

HOMEBREW_NO_AUTO_UPDATE=1 vhs assets/demo.tape

ffmpeg -y -loglevel error -i assets/demo-raw.gif \
  -vf "mpdecimate=hi=64*10:lo=64*4:frac=0.02,setpts=N/12/TB,split[a][b];[a]palettegen=stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=4" \
  -loop 0 assets/demo.gif
rm -f assets/demo-raw.gif
