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

The website in `torana-site` is the visual reference. The app uses its teal
day/night palette and typography at an operational density, not marketing scale.
System / Light / Dark lives in the header and persists per control-plane origin.
The tokens below describe the dark variant; each theme-dependent token has one
`light-dark(light, dark)` definition in `internal/controlplane/dist/tokens.css`.
The browser selects its system scheme without JavaScript; an explicit choice
sets `color-scheme` without duplicating the palette. Header and page gutters
share spacing and respect the device's left/right safe-area insets.

- `--color-paper` oklch(13% 0.018 220)
- `--color-paper-2` oklch(17% 0.020 220)
- `--color-paper-3` oklch(22% 0.022 220)
- `--color-ink` oklch(95% 0.010 205)
- `--color-ink-2` oklch(76% 0.015 210)
- `--color-rule` oklch(29% 0.022 215)
- `--color-accent` oklch(78% 0.140 195)
- `--color-focus` oklch(14% 0.030 250), paired with a light outer ring so
  focus remains visible on both dark surfaces and the bright primary action.

The Torana gateway mark retains its canonical teal-to-blue fill. Accent colour
is reserved for selected state and primary action, below five percent of the
viewport.

## Typography

- Display: Bricolage Grotesque, weight 800, roman.
- Body: Geist, weight 400–650.
- Mono: JetBrains Mono, used only for digests, timestamps, and raw
  configuration.
- Display tracking: `-0.04em`.
- Type scale anchor: `--text-display = clamp(2rem, 1.5rem + 1.5vw, 3.25rem)`.

The app bundles variable Latin font subsets and licenses in `dist/fonts`, with
native fallback for other scripts. It makes no external font requests.

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
- Reorder/configuration changes retain their Save and Discard paths. Existing
  explicit approve-and-enable and disable-and-apply actions remain immediate.
- Drag reorder has keyboard Move up / Move down equivalents.

## CTA voice

- Primary: solid Torana teal with theme-specific contrasting text, compact rectangular shape.
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
