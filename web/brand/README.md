# Maison's mark

The SVG sources here are the origin of everything in `web/public/icons/`. Edit a
source, run `./render-icons.sh`, commit both.

They live outside `src/` and outside `public/` on purpose: Vite copies
`public/` verbatim into the shipped bundle, and there is no reason to ship three
authoring files to every browser.

## Where the artwork came from

`icon.svg` was traced from **`template-root/root/stacks/maison/icon.png`** — the
192px raster already used for Maison's own tile on a PCS — at template-root
`a6bcc61`. The circle centres and radii were fitted to that file's
silhouette rather than eyeballed, and the redraw reproduces it to **RMSE 0.0119**
(1.2%), which is antialiasing noise rather than shape error.

It was redrawn rather than upscaled because 192px is below the 512px a web app
manifest needs, and enlarging the raster to 512 is visibly soft on any modern
phone. A vector source also rasterises to whatever size a future platform asks
for.

Three things in the source look like mistakes and are not, all recorded in the
file's own comment: the background is a **gradient**, not the flat `#F8F6F5` a
colour-picker reports; there are **two** brand colours drawn at 95% opacity, not
the four you can pick out of the original; and the **z-order is asymmetric** —
the left amber circle is under the red, the right one over it.

## Checking it still matches

```sh
compare -metric RMSE \
  <(rsvg-convert -w 192 -h 192 icon.svg | convert png:- -background white -alpha remove -alpha off png:-) \
  <(convert ../../../template-root/root/stacks/maison/icon.png -background white -alpha remove -alpha off png:-) \
  null:
# expect the parenthesised figure <= 0.015
```

And the maskable safe zone — nothing important may leave the circle:

```sh
rsvg-convert -w 512 -h 512 icon-maskable.svg \
  | convert png:- \( +clone -alpha transparent -fill white -draw 'circle 256,256 256,51' \) \
      -compose CopyOpacity -composite show:
```

## Drift

The upstream `icon.png` is in a **different repository** (the `template-root`
submodule) and can change without anything here noticing — the `compare` above is
the only thing that would catch it.

The right long-term fix is the other direction: regenerate
`template-root/root/stacks/maison/icon.png` **from** `icon.svg` so this becomes
the single source for the tile and the PWA alike. That needs a change in
template-root's repo, so it has not been done here.
