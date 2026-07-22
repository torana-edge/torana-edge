# Torana logo

The Torana mark — a stylized *torana* (gateway): a pointed finial, layered eave
beams, and two posts.

## Neon raster (primary brand art)
Full-detail cyan→purple neon render, on black. Use for hero art, README banners,
splash screens, and anywhere the mark appears large.

| File | Use |
| --- | --- |
| `torana-color.png` | Primary neon (cyan→purple) on black. `-512.png` is a downscale. |
| `torana-white.png` | White neon on black, for mono / colored backgrounds. `-512.png` downscale. |

The neon glow is baked into the pixels and needs a dark background; it does not
read below ~64px (the glow swamps the strokes). For small UI sizes use the vectors.

## Vector line-art (small sizes / print / light backgrounds)

| File | Use |
| --- | --- |
| `torana-color.svg` | Cyan→purple neon-tint gradient on dark backgrounds. Crisp at any size. |
| `torana-white.svg` | Monochrome white, for photos / colored backgrounds. |
| `torana-dark.svg` | Monochrome navy (`#0b1a2f`), for light backgrounds. |

The three vectors share identical geometry (`viewBox="0 0 64 64"`) — only the
stroke differs. To recolor, swap the `stroke` value; for a theme-inheriting mark,
set `stroke="currentColor"`.

**Gradient note:** the color variant uses `gradientUnits="userSpaceOnUse"`. This
is required — the mark contains perfectly horizontal/vertical strokes, whose
`objectBoundingBox` (the SVG default) is zero-area, and browsers drop the
gradient fill on those strokes. `userSpaceOnUse` also gives one cohesive
diagonal sweep across the whole mark instead of a per-stroke gradient.

The header logo and favicon in the Control Plane SPA
(`internal/controlplane/dist/index.html`) are inlined copies of the color
variant.
