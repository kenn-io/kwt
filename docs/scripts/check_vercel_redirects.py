#!/usr/bin/env python3
"""Check docs/vercel.json against the Zensical nav.

The docs tier moved under /docs/ when the website tier took over the site
root, so every nav page's legacy HTML route and Markdown twin must redirect
permanently. The root index is the exception: the website owns / and
/index.md now. Any other redirect is rejected so the table cannot drift.
"""

from __future__ import annotations

import json
import pathlib
import sys
from typing import Any

ROOT = pathlib.Path(__file__).resolve().parents[1]
VERCEL = ROOT / "vercel.json"

sys.path.insert(0, str(ROOT / "scripts"))
from public_markdown_sources import fail, public_markdown_sources  # noqa: E402

ASSET_REDIRECTS = {
    "/assets/:path*": "/docs/assets/:path*",
}


def load_vercel() -> dict[str, Any]:
    try:
        data = json.loads(VERCEL.read_text(encoding="utf-8"))
    except FileNotFoundError:
        fail("missing docs/vercel.json")
    except json.JSONDecodeError as error:
        fail(f"invalid docs/vercel.json: {error}")
    if not isinstance(data, dict):
        fail("docs/vercel.json must contain an object")
    return data


def collect_redirects(data: dict[str, Any]) -> dict[str, dict[str, Any]]:
    raw_redirects = data.get("redirects", [])
    if not isinstance(raw_redirects, list):
        fail("vercel redirects must be a list")

    redirects: dict[str, dict[str, Any]] = {}
    for index, item in enumerate(raw_redirects):
        if not isinstance(item, dict):
            fail(f"redirect entry {index} must be an object")
        if set(item) != {"source", "destination", "permanent"}:
            fail(f"redirect entry {index} must contain source, destination, and permanent only")
        source = item.get("source")
        if not isinstance(source, str) or not source:
            fail(f"redirect entry {index} missing source")
        if not isinstance(item.get("destination"), str) or not item["destination"]:
            fail(f"redirect entry {index} missing destination")
        if not isinstance(item.get("permanent"), bool):
            fail(f"redirect entry {index} permanent must be boolean")
        if source in redirects:
            fail(f"duplicate redirect source {source}")
        redirects[source] = item
    return redirects


def expected_legacy_redirects() -> dict[str, str]:
    expected: dict[str, str] = {}
    for path in public_markdown_sources(ROOT / "zensical.toml"):
        if path == "index.md":
            continue
        stem = path.removesuffix(".md")
        route = stem.removesuffix("/index")
        expected[f"/{route}/"] = f"/docs/{route}/"
        expected[f"/{path}"] = f"/docs/{path}"
    expected.update(ASSET_REDIRECTS)
    return expected


def main() -> None:
    data = load_vercel()
    if "framework" not in data or data["framework"] is not None:
        fail("vercel framework must be null")
    if data.get("trailingSlash") is not True:
        fail("vercel trailingSlash must be true so /docs/<page> resolves like Zensical links")

    redirects = collect_redirects(data)
    expected = expected_legacy_redirects()
    for source, destination in expected.items():
        item = redirects.get(source)
        if not item:
            fail(f"missing legacy redirect {source}")
        if item["destination"] != destination or item["permanent"] is not True:
            fail(f"incorrect legacy redirect {source}")
    for source in redirects:
        if source not in expected:
            fail(f"unexpected redirect source {source}")

    print("vercel redirect checks passed")


if __name__ == "__main__":
    main()
