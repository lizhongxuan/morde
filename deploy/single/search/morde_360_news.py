# SPDX-License-Identifier: AGPL-3.0-or-later
"""Bounded 360 news-widget engine used by Morde's private SearXNG instance."""

from importlib import import_module
from ipaddress import ip_address
from typing import TYPE_CHECKING
from urllib.parse import urlsplit

from lxml import etree
from searx.result_types import EngineResults

if TYPE_CHECKING:
    from searx.extended_types import SXNG_Response

_upstream = import_module("searx.engines.360search")

about = {
    "website": "https://www.so.com/",
    "use_official_api": False,
    "require_api_key": False,
    "results": "HTML news widget",
}

categories = ["news"]
paging = False
language = "zh"
time_range_support = True
max_results = 8


def setup(engine_settings: dict) -> bool:
    # Reuse the pinned upstream engine's cookie cache and HTTP client.
    return _upstream.setup({"name": "morde_360_news"})


def request(query: str, params: dict) -> None:
    params.setdefault("pageno", 1)
    params.setdefault("time_range", None)
    _upstream.request(query, params)


def _text(node) -> str:
    return " ".join(" ".join(node.itertext()).split()) if node is not None else ""


def _direct_publisher_url(value: str) -> str | None:
    try:
        parsed = urlsplit(value.strip())
        hostname = parsed.hostname.lower() if parsed.hostname else ""
        port = parsed.port
    except ValueError:
        return None
    if parsed.scheme != "https" or not hostname or parsed.username or parsed.password or parsed.fragment:
        return None
    if port not in (None, 443) or hostname == "so.com" or hostname.endswith(".so.com"):
        return None
    if hostname == "360.cn" or hostname.endswith(".360.cn"):
        return None
    try:
        address = ip_address(hostname)
    except ValueError:
        address = None
    if address is not None and not address.is_global:
        return None
    return value.strip()


def response(resp: "SXNG_Response") -> EngineResults:
    results = EngineResults()
    if not resp.text or not resp.text.strip():
        return results
    try:
        root = resp.html()
    except (etree.ParserError, TypeError, ValueError):
        return results
    if root is None:
        return results

    seen: set[str] = set()
    modules = root.xpath('//*[@id="mohe-news"]')
    widget_items = modules[0].xpath('.//a[@data-mdurl]') if modules else []
    added = _append_items(results, widget_items, seen)
    if added == 0:
        # 360 may omit its dedicated news widget outside its primary region,
        # or render the module without a usable direct URL. Standard results
        # still carry the direct publisher candidate in data-mdurl; never use
        # the 360 redirect URL in href as a fallback.
        standard_items = root.xpath(
            '//li[contains(concat(" ", normalize-space(@class), " "), " res-list ")]'
            '//h3[contains(concat(" ", normalize-space(@class), " "), " res-title ")]'
            '/a[@data-mdurl]'
        )
        _append_items(results, standard_items, seen)
    return results


def _append_items(results: EngineResults, items, seen: set[str]) -> int:
    added = 0
    for item in items:
        title = (item.get("title") or _text(item)).strip()
        url = _direct_publisher_url(item.get("data-mdurl", ""))
        if not title or not url or url in seen:
            continue
        seen.add(url)
        added += 1

        origin = item.xpath(
            'ancestor::div[contains(concat(" ", normalize-space(@class), " "), " mh-info ")][1]'
            '//span[contains(concat(" ", normalize-space(@class), " "), " mh-origin ")]'
        )
        publisher = _text(origin[0]) if origin else (urlsplit(url).hostname or "")
        if publisher.startswith("www."):
            publisher = publisher[4:]
        content = f"Publisher: {publisher}" if publisher else ""
        # The widget's relative labels (for example, "1天前") are not dates.
        # Leave publishedDate unset and require the Task Agent to read the page.
        results.add(results.types.MainResult(url=url, title=title, content=content, publishedDate=None))
        if len(seen) >= max_results:
            break
    return added
