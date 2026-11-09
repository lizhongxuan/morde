#!/usr/bin/env python3
"""Generate the nanoMuse iOS app icons from assets/brand/.

Idempotent; rerun after changing the mark or icon colours. Writes 1024x1024 PNGs:

- Assets.xcassets/AppIcon.appiconset/Icon-1024.png       the one-stroke N in the brand gradient
                                                          on a white tile (the default icon)
- Assets.xcassets/AppIcon.appiconset/Icon-1024-Dark.png  the same mark on the dark tile (the
                                                          automatic dark appearance)
- Resources/AlternateIcons/AppIcon-{Light,Dark,LegacyLight,LegacyDark}.png  the four skins the
  in-app icon picker offers (Info.plist CFBundleAlternateIcons): light / dark with the gradient
  mark, "legacy" light with the flat brand blue, "legacy" dark with a white mark.

iOS applies its own corner mask, so the tiles are full squares. The mark is drawn on a 100-unit
tile from assets/brand/nanomuse-mark.svg.
Needs Playwright with Chromium (`pip install playwright && playwright install chromium`).
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
IOS = ROOT / "android" / "src" / "ios"
ICONSET = IOS / "Assets.xcassets" / "AppIcon.appiconset"
ALTERNATE = IOS / "Resources" / "AlternateIcons"
BRAND_MARK = ROOT / "assets" / "brand" / "nanomuse-mark.svg"
SIZE = 1024
STROKE_START = "#015CFB"
STROKE_END = "#0186FB"
TILE_LIGHT = "#FFFFFF"
TILE_DARK = "#0F1B33"


def mark_path() -> str:
    svg = BRAND_MARK.read_text(encoding="utf-8")
    match = re.search(r'<path[^>]*\sd="([^"]+)"', svg)
    if not match:
        sys.exit(f"{BRAND_MARK}: no <path d=...>")
    return match.group(1)


def tile(d: str, background: str, stroke_start: str, stroke_end: str | None = None) -> str:
    """A full-square tile with the mark; a gradient when two stroke colours are given."""
    grad = (
        f'<linearGradient id="g" gradientUnits="userSpaceOnUse" x1="12.6" y1="50" x2="87.8" y2="50">'
        f'<stop offset="0" stop-color="{stroke_start}"/><stop offset="1" stop-color="{stroke_end}"/></linearGradient>'
        if stroke_end
        else ""
    )
    fill = "url(#g)" if stroke_end else stroke_start
    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><defs>{grad}</defs>'
        f'<rect width="100" height="100" fill="{background}"/><path d="{d}" fill="{fill}"/></svg>'
    )


def main(argv: list[str] | None = None) -> int:
    args = sys.argv[1:] if argv is None else argv
    if args in (["-h"], ["--help"]):
        print(__doc__.strip())
        return 0
    if args:
        print(
            f"usage: {sys.argv[0]}  (takes no arguments; unknown: {' '.join(args)})",
            file=sys.stderr,
        )
        return 2
    if not ICONSET.is_dir():
        sys.exit(f"{ICONSET} not found (restore the iOS tree first)")
    try:
        from playwright.sync_api import sync_playwright
    except ImportError:
        sys.exit("playwright not installed: pip install playwright && playwright install chromium")
    d = mark_path()
    light = tile(d, TILE_LIGHT, STROKE_START, STROKE_END)
    dark = tile(d, TILE_DARK, STROKE_START, STROKE_END)
    jobs = {
        ICONSET / "Icon-1024.png": light,
        ICONSET / "Icon-1024-Dark.png": dark,
        ALTERNATE / "AppIcon-Light.png": light,
        ALTERNATE / "AppIcon-Dark.png": dark,
        ALTERNATE / "AppIcon-LegacyLight.png": tile(d, TILE_LIGHT, STROKE_START),
        ALTERNATE / "AppIcon-LegacyDark.png": tile(d, TILE_DARK, "#FFFFFF"),
    }
    with sync_playwright() as p:
        browser = p.chromium.launch()
        page = browser.new_page(viewport={"width": SIZE, "height": SIZE}, device_scale_factor=1)
        for target, svg in jobs.items():
            if not target.parent.is_dir():
                continue
            page.set_content(f'<body style="margin:0">{svg}</body>')
            page.locator("svg").evaluate(
                "(e, s) => { e.setAttribute('width', s); e.setAttribute('height', s); }", str(SIZE)
            )
            page.screenshot(path=str(target), clip={"x": 0, "y": 0, "width": SIZE, "height": SIZE})
            print(f"  wrote {target.relative_to(ROOT)}")
        browser.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
