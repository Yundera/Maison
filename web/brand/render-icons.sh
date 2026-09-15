#!/bin/sh
# Renders web/public/icons/ from the SVG sources beside this script.
#
# Run BY HAND after editing a source, and commit the output. This is deliberately
# not wired into `npm run build`: it needs librsvg and ImageMagick, neither of
# which is in the Dockerfile's node stage, and making the image build depend on
# them would trade a once-a-year manual step for a permanent build dependency.
set -eu
cd "$(dirname "$0")"
out=../public/icons
mkdir -p "$out"

cp icon.svg "$out/icon.svg"
rsvg-convert -w 192 -h 192 icon.svg          -o "$out/icon-192.png"
rsvg-convert -w 512 -h 512 icon.svg          -o "$out/icon-512.png"
rsvg-convert -w 512 -h 512 icon-maskable.svg -o "$out/icon-maskable-512.png"
rsvg-convert -w  32 -h  32 icon.svg | convert png:- -strip "$out/favicon-32.png"

# The apple-touch-icon must have no alpha channel at all: iOS composites it onto
# black, so a transparent corner becomes a black notch. Flatten onto the
# background gradient's midpoint so the seam is invisible.
rsvg-convert -w 180 -h 180 icon-square.svg \
  | convert png:- -background '#F8F6F5' -alpha remove -alpha off -strip "$out/apple-touch-icon.png"

echo "rendered:"
ls -1 "$out"
