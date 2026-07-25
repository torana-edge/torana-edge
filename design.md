# Design — Torana Control Plane

A locked design system for Torana's local operator interface. Every control
plane view uses this system; new views extend it instead of inventing a second
theme.

## Genre

Modern-minimal, with the density and directness of an operator workbench.

## Macrostructure family

- App pages: Workbench — compact command rail, page title and action paired at
  the content edge, operational lists and tables, persistent save controls.
- Plugin detail: Long Document within the same shell — identity and trust
  information first, configuration second, raw data last.
- Marketing and content pages are outside this embedded app's scope.

## Theme

- `--color-paper` oklch(17% 0.025 250)
- `--color-paper-2` oklch(21% 0.030 250)
- `--color-paper-3` oklch(25% 0.032 250)
- `--color-ink` oklch(95% 0.012 230)
- `--color-ink-2` oklch(79% 0.025 240)
- `--color-rule` oklch(37% 0.035 245)
- `--color-accent` oklch(82% 0.145 190)
- `--color-focus` oklch(14% 0.030 250), paired with a light outer ring so
  focus remains visible on both dark surfaces and the bright primary action.

The Torana gateway mark retains its canonical teal-to-blue fill. Accent colour
is reserved for selected state and primary action, below five percent of the
viewport.

## Typography

- Display: Bahnschrift SemiCondensed / DIN Condensed / Aptos Display, weight
  650, roman.
- Body: ui-sans-serif native stack, weight 400–650.
- Mono: ui-monospace native stack, used only for digests, timestamps, and raw
  configuration.
- Display tracking: `-0.025em`.
- Type scale anchor: `--text-display = clamp(1.5rem, 1.1rem + 1.2vw, 2.25rem)`.

The app remains self-contained and makes no network font requests.

## Spacing

Four-point named scale in `internal/controlplane/dist/tokens.css`. Production
styles consume named spacing tokens rather than raw spacing values.

## Motion

- `--ease-out`: cubic-bezier(0.16, 1, 0.3, 1).
- State feedback animates colour and opacity only.
- Reduced motion removes non-essential transitions.

## Microinteractions stance

- Quiet in-place success; errors use the persistent alert region.
- Focus rings are immediate and always visible.
- Reversible enable/disable changes remain staged until Save, preserving the
  existing Discard path.
- Drag reorder has keyboard Move up / Move down equivalents.

## CTA voice

- Primary: solid Torana teal, dark text, compact rectangular shape.
- Secondary: quiet ink surface with a one-pixel rule.
- Destructive: tinted text and rule, never a saturated filled button.

## Per-page allowances

- App views do not use decorative enrichment; live data and plugin state carry
  the interface.
- Tables may scroll horizontally inside their own labelled region where a
  mobile card representation would obscure operational comparison.

## What views MUST share

- Canonical gateway mark and wordmark.
- Accent placement, typography, spacing, focus treatment, buttons, inputs,
  status language, and header shell.
- A single-column mobile reading order from 320 px upward.

## What views MAY differ on

- Traffic uses metric strips before tabular detail.
- Feed prioritizes its data table and connection state.
- Settings uses grouped fieldsets.
- Plugin detail may use an isolated plugin iframe.
