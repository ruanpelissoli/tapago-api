# assets/

Static brand assets for the tapago project.

## Files

| File | Use |
|---|---|
| `logo.svg` | Horizontal logo (icon + wordmark) on light backgrounds — 260×64 px viewBox |
| `logo-dark.svg` | Same layout with white wordmark for dark backgrounds |
| `logo-mark.svg` | Square icon only (64×64) — use for favicons, app icons, avatars |

## Brand

- **Primary colour**: indigo-to-violet gradient (`#6366F1` → `#8B5CF6`)
- **Light wordmark**: `#1E1B4B` (indigo-950)
- **Dark wordmark**: `#FFFFFF`
- **Icon shape**: 64×64 rounded square, `rx="16"`, stylised "T" in white

## Key decisions

SVG chosen over PNG so assets scale losslessly at any resolution. Text in
`logo.svg` / `logo-dark.svg` uses the system sans-serif stack; no custom font
embedding needed for the wordmark at this stage — avoids bundling a font file
just for branding assets.

If a custom typeface is adopted later, convert the `<text>` element to `<path>`
data so rendering is font-independent.
