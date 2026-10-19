# Study: dark strips on the desktop tiles (Chromium)

> **Status: studied, not fixed (2026-10-18).** The glitch is visually minor and the
> code is unchanged. This page keeps the diagnosis and the fix options for later.

After certain hovers, some app tiles show a darker band about 30px wide, with a sharp
edge. It comes from a Chromium rendering bug, not from Maison's CSS: Chrome redraws only
part of the screen and gets the tiles' frosted-glass blur (`backdrop-filter`) wrong there.
Chrome and Edge show it; Firefox does not. Turning off Skia Graphite does not help.

---

## Symptom

- **Trigger:** hovering certain spots, such as the right side of the Storage widget
  (vertical strips) or the gitea tile (horizontal strips).
- **Fixed screen lines:** the strips sit at fixed screen positions, not at tile edges.
  Examples: x≈597 at 1352px wide, x≈561 at 1186px wide, y≈150 when hovering gitea.
  A strip can run through the middle of a tile, and one tile can show a vertical and a
  horizontal strip at once.
- **Merging:** strips from different hovers can merge, or cover only part of a tile.
- **Repeatable:** for a given hover spot and window size, the strip is always in the
  same place. Seen on several PCS, including yunderalabs.

## Environment

| Browser | Rendering | Glitch |
|---------|-----------|--------|
| Chrome 155.0.8059.40 | Skia Graphite on Dawn/D3D11 (default) | Yes |
| Chrome 155.0.8059.40 | Graphite disabled in `chrome://flags` | Yes |
| Edge (Chromium) | default | Yes |
| Firefox | default | No |
| Chrome driven by the chrome-devtools MCP | likely software rendering | Not reproducible |

All browsers ran on the same desktop: Windows 11 (NT 10.0.26100), NVIDIA GeForce RTX
2080 Ti, driver 32.0.15.6094, ANGLE on D3D11, DirectComposition on. The exception is the
MCP-driven Chrome, a separate browser.

Not yet tested: Chrome with hardware acceleration turned off, and another GPU vendor.

## Bisection

Each override was pasted into the DevTools console, then Storage was hovered. The page
was reloaded between tests. Only removing the blur on the tiles (test 5) removed the
glitch.

| # | CSS override | Glitch |
|---|--------------|--------|
| 0 | `.widget, .tile { will-change: transform }` (permanent layers) | Yes |
| 1 | No `backdrop-filter` anywhere (`.glass`, `.views`, `.add`) | **No** |
| 2 | `.widget:hover { transform: none }` | Yes |
| 3 | `.widget:hover { box-shadow: none }` | Yes |
| 4 | No blur on widget glass only | Yes |
| 5 | No blur on tile glass only | **No** |
| 6 | `--glass-blur: blur(30px)` (no `saturate`) | Yes |
| 7 | `--glass-blur: blur(12px) saturate(180%)`, strip width unchanged | Yes |
| 8 | `.tile .glass { transform: translateZ(0) }` | Yes |
| 9 | Square, unclipped tile glass (`border-radius: 0; overflow: visible`) | Yes |
| 10 | No inset shadow or sheen on tile glass | Yes |
| 11 | `.tile::after { display: none }` (hover highlight) | Yes |
| 12 | DevTools *Paint flashing* on | **No** |
| 13 | DevTools *Layer borders* on | Yes |

## Analysis

The likely cause is a partial redraw in Chromium's compositor that recomputes the tiles'
backdrop blur only inside the area being redrawn. This fits every result, but has not
been confirmed in Chromium's code.

1. A hover changes a small area: the hovered card plus its shadow, widened by the blur
   reach of any glass it touches. Chrome redraws only that area (the "damage rect").
2. Inside that area, the tiles' blur is recomputed from the area alone, so it comes out
   darker. Outside it, the previous frame is kept. The strip's sharp edge is the edge of
   the redrawn area.

How each result fits:

- **Fixed screen lines that cut through tiles:** the strips follow the redraw area, not
  the layout.
- **Blur radius doesn't change the width (test 7):** a blur pulling in pixels from
  outside the tile would get narrower with a smaller radius. Here the edge is the redraw
  boundary.
- **Paint flashing hides it, layer borders don't:** paint flashing likely makes Chrome
  redraw the whole window every frame, so no partial redraw happens.
- **Layer and paint tweaks change nothing (tests 0, 8–11):** the bug sits in how the
  final image is put together, after layers are built.
- **Hover effects aren't the cause (tests 2–4):** any repaint near the tiles can expose
  the bug.
- **Chrome and Edge only, Graphite on or off:** the bug is in code that Chrome and Edge
  share, above the Skia backend. Firefox has its own renderer.
- **The MCP-driven Chrome doesn't reproduce it:** that browser likely draws in software,
  without the GPU path.

Open question: has this already been reported in the Chromium bug tracker? We haven't
searched or filed yet.

## Options if we fix it later

1. **Pre-blurred wallpaper (preferred).** Draw one blurred copy of the wallpaper behind
   the desktop, fixed to the window. Each tile's glass shows its slice of it, clipped to
   the tile. With no live blur on the tiles, a partial redraw has nothing to leave stale,
   and it costs less GPU than about 20 live blurs. The work is keeping each slice aligned
   through the hover lift and drag-to-reorder. Widgets and the header can keep their
   real blur, since test 4 cleared them.
2. **Drop the blur on tiles only.** One CSS change plus a stronger `--glass-color` tint so
   white text stays readable. Loses some of the frosted look.
3. **Report to Chromium.** File a bug at crbug.com with a minimal HTML reproduction: a
   grid of `backdrop-filter` tiles over a wallpaper, plus a hover that repaints a
   neighbour.

## Code

Paths relative to `web/src/`:

- `styles/tokens.css`: `--glass-blur`, `--glass-color`, `--shadow-lift`
- `styles/app.css`: `.glass`, the shared frosted surface
- `lib/components/Tile.svelte`: tile markup, hover transform, `::after` highlight
- `lib/components/Widget.svelte`: sidebar widget hover (`scale(1.01)`)
