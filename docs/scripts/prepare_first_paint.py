#!/usr/bin/env python3
"""Keep every built page from painting before its stylesheets and fonts.

Embeds the woff2 fonts that the given stylesheets reference as data URLs, so
each font arrives with the render-blocking stylesheet, and inlines
hold-first-paint.js directly after each page's <meta charset>, so Chromium
does not paint a frame before it has decoded those fonts.
"""

from __future__ import annotations

import argparse
import base64
import pathlib
import re

from public_markdown_sources import fail

ROOT = pathlib.Path(__file__).resolve().parents[1]
HOLD_SCRIPT = pathlib.Path(__file__).resolve().parent / "hold-first-paint.js"
FONT_URL = re.compile(r"""url\(\s*(["']?)([^"')]+\.woff2)\1\s*\)""")
CHARSET = re.compile(rb"""<meta\s+charset=["']?utf-8["']?\s*/?>""", re.IGNORECASE)
# Browsers look for the charset declaration only in the first 1024 bytes.
CHARSET_SCAN_BYTES = 1024


def resolve_font(
    site_dir: pathlib.Path, stylesheet: pathlib.Path, reference: str
) -> pathlib.Path:
    if reference.startswith("/"):
        font = (site_dir / reference.lstrip("/")).resolve()
    else:
        font = (stylesheet.parent / reference).resolve()
    if not font.is_relative_to(site_dir) or not font.is_file():
        fail(
            f"{stylesheet.relative_to(site_dir)}: font {reference} is not a file in the site"
        )
    return font


def embed_fonts(site_dir: pathlib.Path, stylesheet: pathlib.Path) -> None:
    css = stylesheet.read_text(encoding="utf-8")

    def data_url(match: re.Match[str]) -> str:
        font = resolve_font(site_dir, stylesheet, match.group(2))
        encoded = base64.b64encode(font.read_bytes()).decode("ascii")
        return f'url("data:font/woff2;base64,{encoded}")'

    embedded, count = FONT_URL.subn(data_url, css)
    if count == 0:
        fail(f"{stylesheet.relative_to(site_dir)} references no woff2 font to embed")
    stylesheet.write_text(embedded, encoding="utf-8")


def hold_first_paint(site_dir: pathlib.Path, page: pathlib.Path, tag: bytes) -> None:
    html = page.read_bytes()
    match = CHARSET.search(html, 0, CHARSET_SCAN_BYTES)
    if match is None:
        fail(
            f"{page.relative_to(site_dir)} has no <meta charset> in its first {CHARSET_SCAN_BYTES} bytes"
        )
    page.write_bytes(html[: match.end()] + tag + html[match.end() :])


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Embed fonts and hold first paint in the generated site.",
    )
    parser.add_argument(
        "--site-dir",
        default="site",
        help="generated site directory relative to docs root",
    )
    parser.add_argument(
        "--stylesheet",
        action="append",
        required=True,
        help="render-blocking stylesheet relative to the site directory; repeatable",
    )
    args = parser.parse_args()

    site_dir = (ROOT / args.site_dir).resolve()
    if not site_dir.is_dir():
        fail(f"missing site directory: {args.site_dir}")
    for name in args.stylesheet:
        stylesheet = site_dir / name
        if not stylesheet.is_file():
            fail(f"missing stylesheet: {name}")
        embed_fonts(site_dir, stylesheet)

    script = HOLD_SCRIPT.read_text(encoding="utf-8")
    if "</script" in script.lower():
        fail(f"{HOLD_SCRIPT.name} cannot be inlined: it contains </script")
    tag = f"<script>{script}</script>".encode()
    pages = sorted(site_dir.rglob("*.html"))
    if not pages:
        fail(f"no HTML pages under {args.site_dir}")
    for page in pages:
        hold_first_paint(site_dir, page, tag)
    print(f"held first paint on {len(pages)} pages")


if __name__ == "__main__":
    main()
