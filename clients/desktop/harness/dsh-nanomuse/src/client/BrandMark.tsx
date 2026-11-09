/**
 * The app's own mark (docs/brand.md): the N stroke on its white squircle tile with a hairline
 * edge, drawn inline from `assets/brand/nanomuse-icon.svg` so the bundle fetches nothing. It
 * stands for the app where the app speaks for itself — the sign-in pages, the permissions
 * page — never for the Muse: the face is the Muse, the mark is the app, and the mark never
 * goes in the chat. The tile stays white in the dark theme too (the brand's rule).
 */
import { createElement as h, useId, type ReactNode } from 'react'

/** The stroke, in the tile's 100 × 100 coordinates (one filled path, the brand's gradient). */
const STROKE = 'M50.02 79.14C45.88 78.24 43.14 74.48 41.94 68.06C41.21 64.11 41.07 57.03 41.57 48.4C42.01 40.73 41.95 38.95 41.25 38.95C40.84 38.95 38.78 40.99 37.31 42.85C35.79 44.78 32.24 49.66 29.36 53.81C24.1 61.36 23.23 62.5 21.73 63.62C20.32 64.69 19.77 64.88 18.1 64.89C16.81 64.9 16.55 64.85 15.8 64.51C14.63 63.95 13.5 62.79 13.09 61.7C12.38 59.79 12.36 58.4 13.03 56.4C15.13 50.07 30.84 29.92 37.95 24.43C40.06 22.8 42.52 21.8 44.44 21.8C47.3 21.8 50.22 23.43 51.98 26.02C54.35 29.48 55.41 36.71 55.18 47.82C54.87 62.95 54.87 63.03 55.36 63.2C55.47 63.23 56.02 62.78 56.6 62.19C58.17 60.58 59.77 58.46 67.56 47.75C74.06 38.79 76.77 35.7 79.17 34.51C80.29 33.94 80.32 33.94 81.98 33.94C83.54 33.95 83.71 33.97 84.59 34.39C86.34 35.22 87.54 36.65 88.0 38.44C88.3 39.61 88.28 40.72 87.94 42.08C87.45 43.98 86.62 45.31 82.58 50.7C81.37 52.32 78.99 55.53 77.3 57.82C71.35 65.91 68.99 68.81 65.78 72.03C63.02 74.79 60.71 76.55 58.18 77.81C55.42 79.19 52.47 79.67 50.02 79.14Z'

export function BrandMark({ size = 72, className }: { size?: number; className?: string }): ReactNode {
  // one gradient per instance: two marks on one page must not share an id
  const id = `nm-mark-${useId().replace(/[^a-zA-Z0-9_-]/g, '')}`
  return h('svg', {
    width: size,
    height: size,
    viewBox: '0 0 100 100',
    className: `nm-brand-mark${className ? ` ${className}` : ''}`,
    role: 'img',
    'aria-label': 'nanoMuse',
    focusable: false,
  },
    h('defs', null,
      h('linearGradient', { id, gradientUnits: 'userSpaceOnUse', x1: 12.6, y1: 50, x2: 87.8, y2: 50 },
        h('stop', { offset: 0, stopColor: '#015CFB' }),
        h('stop', { offset: 1, stopColor: '#0186FB' }))),
    // the tile with its hairline edge, so it keeps its shape on a white page
    h('rect', { x: 0.3, y: 0.3, width: 99.4, height: 99.4, rx: 27, ry: 27, fill: '#FFFFFF', stroke: '#D9D9DE', strokeWidth: 0.6 }),
    h('path', { d: STROKE, fill: `url(#${id})` }))
}
