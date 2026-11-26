# SPDX-License-Identifier: AGPL-3.0-or-later
"""Bounded Google News RSS engine used by Morde's private SearXNG instance."""

from datetime import timezone
from email.utils import parsedate_to_datetime
from typing import TYPE_CHECKING
from urllib.parse import urlencode

from lxml import etree
from lxml.html import fromstring
from searx.result_types import EngineResults

if TYPE_CHECKING:
    from searx.extended_types import SXNG_Response
    from searx.search.processors import OnlineParams

about = {
    "website": "https://news.google.com/",
    "use_official_api": False,
    "require_api_key": False,
    "results": "RSS",
}

categories = ["news"]
paging = False
language_support = False
time_range_support = True
base_url = "https://news.google.com/rss/search"
time_range_map = {"day": "1d", "week": "7d", "month": "30d", "year": "365d"}
max_results = 20


def request(query: str, params: "OnlineParams") -> None:
    search_query = query.strip()
    time_range = params.get("time_range")
    if time_range:
        days = time_range_map.get(time_range)
        if days:
            search_query = f"{search_query} when:{days}"

    params["url"] = base_url + "?" + urlencode(
        {"q": search_query, "hl": "en-US", "gl": "US", "ceid": "US:en"}
    )
    params["headers"]["Accept"] = "application/rss+xml, application/xml;q=0.9, */*;q=0.8"


def _text(item, tag: str) -> str:
    node = item.find(tag)
    if node is None:
        return ""
    return " ".join(" ".join(node.itertext()).split())


def _published_date(value: str):
    if not value:
        return None
    try:
        parsed = parsedate_to_datetime(value)
    except (TypeError, ValueError, OverflowError):
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed


def response(resp: "SXNG_Response") -> EngineResults:
    results = EngineResults()
    root = etree.fromstring(resp.content)
    for item in root.findall("./channel/item")[:max_results]:
        title = _text(item, "title")
        url = _text(item, "link")
        if not title or not url.startswith("https://"):
            continue

        source = _text(item, "source")
        published = _text(item, "pubDate")
        description = _plain_html(_text(item, "description"))
        content = " | ".join(value for value in (
            f"Source: {source}" if source else "",
            f"Published: {published}" if published else "",
            description,
        ) if value)
        results.add(results.types.MainResult(
            url=url,
            title=title,
            content=content,
            publishedDate=_published_date(published),
        ))
    return results


def _plain_html(value: str) -> str:
    if not value:
        return ""
    try:
        return " ".join(fromstring(value[:4096]).text_content().split())
    except (etree.ParserError, ValueError):
        return " ".join(value.split())
