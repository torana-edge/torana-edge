# Torana logo

The Torana mark — a stylized *torana* (gateway): a pointed finial, layered eave
beams, and two posts.

| File | Use |
| --- | --- |
| `torana-color.svg` | Primary. Cyan→indigo neon gradient on dark backgrounds. |
| `torana-white.svg` | Monochrome white, for photos / colored backgrounds. |
| `torana-dark.svg` | Monochrome navy (`#0b1a2f`), for light backgrounds. |

All three share identical geometry (`viewBox="0 0 64 64"`) — only the stroke
differs. To recolor, swap the `stroke` value; for a theme-inheriting mark, set
`stroke="currentColor"`.

**Gradient note:** the color variant uses `gradientUnits="userSpaceOnUse"`. This
is required — the mark contains perfectly horizontal/vertical strokes, whose
`objectBoundingBox` (the SVG default) is zero-area, and browsers drop the
gradient fill on those strokes. `userSpaceOnUse` also gives one cohesive
diagonal sweep across the whole mark instead of a per-stroke gradient.

The header logo and favicon in the Control Plane SPA
(`internal/controlplane/dist/index.html`) are inlined copies of the color
variant.
