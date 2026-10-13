# Maison's mark

The SVG sources here are the origin of everything in `web/public/icons/`. Edit a
source, run `./render-icons.sh`, commit both.

They live outside `src/` and outside `public/` on purpose: Vite copies
`public/` verbatim into the shipped bundle, and there is no reason to ship three
authoring files to every browser.

## Where the artwork came from

The **October 2026 brand pack** ("MAISON, PACK LOGO") — the nautilus shell, blue
`#27AAE1` with a single pink chamber `#EE2A7B`. The shell geometry is the pack's
own vector, never redrawn here:

| file | what it is |
|---|---|
| `favicon.svg` | the pack's `maison-symbole-favicon.svg` verbatim — the flat shell, no ribs |
| `icon.svg` | the pack's `maison-symbole-icone.svg` nested at 77.5% on a white rounded tile (the pack's `icon-512.png` placement) |
| `icon-square.svg` | the same shell at 72%, full bleed — the apple-touch source (the pack's `apple-touch-icon.png` placement) |
| `icon-maskable.svg` | the same shell at 60%, full bleed — the maskable source (the pack's `icon-maskable-512.png` placement) |

The rendered `apple-touch-icon.png` and `icon-maskable-512.png` are pixel-identical
(RMSE 0) to the pack's own PNGs.

The pack's usage rules that bind these files:

- **Below 48px, only the favicon or icone variant** — never the thin-line
  `maison-symbole-trait`. That is why `favicon-32.png` and the browser-tab SVG
  come from `favicon.svg`, not `icon.svg`.
- **One pink chamber, no other colour inside the shell.**
- **Clear space around the mark at least the pink chamber's height.**

The full-size logos (horizontal, vertical, "par Yundera", colour/mono/inverse) are
not part of the app; the two the top-level README shows are in `docs/brand/`.

## Checking the maskable safe zone

Nothing important may leave the circle:

```sh
rsvg-convert -w 512 -h 512 icon-maskable.svg \
  | convert png:- \( +clone -alpha transparent -fill white -draw 'circle 256,256 256,51' \) \
      -compose CopyOpacity -composite show:
```

## Other copies of the mark

Two places carry the mark without reading these files, and must be updated by hand
when it changes:

- **`internal/server/pwa.go`** — the offline page inlines `favicon.svg`, because it
  must render while the server that would serve `/icons/` is unreachable.
- **The Maison tile on a box** — `stacks/maison/icon.png` in
  `mesh-router-template-root`, a different repository, rendered from `icon.svg`.
