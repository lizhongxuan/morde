import importlib.util
import unittest
from datetime import datetime, timezone
from types import SimpleNamespace
from urllib.parse import parse_qs, urlparse


ENGINE_PATH = "/tmp/morde_google_news_rss.py"
SPEC = importlib.util.spec_from_file_location("morde_google_news_rss", ENGINE_PATH)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"could not load news engine from {ENGINE_PATH}")
ENGINE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ENGINE)


class GoogleNewsRSSEngineTests(unittest.TestCase):
    def test_request_preserves_query_and_maps_time_ranges(self):
        expected = {"day": "1d", "week": "7d", "month": "30d", "year": "365d"}
        for time_range, suffix in expected.items():
            with self.subTest(time_range=time_range):
                params = {"time_range": time_range, "headers": {}}
                ENGINE.request("climate policy", params)
                query = parse_qs(urlparse(params["url"]).query)
                self.assertEqual(query["q"], [f"climate policy when:{suffix}"])
                self.assertEqual(query["hl"], ["en-US"])
                self.assertTrue(params["headers"]["Accept"].startswith("application/rss+xml"))

    def test_request_without_time_range_does_not_add_a_filter(self):
        params = {"headers": {}}
        ENGINE.request("climate policy", params)
        query = parse_qs(urlparse(params["url"]).query)
        self.assertEqual(query["q"], ["climate policy"])

    def test_response_keeps_source_and_rss_date_but_skips_insecure_links(self):
        feed = b"""<rss><channel>
          <item>
            <title>Example report - Example Publisher</title>
            <link>https://news.google.com/rss/articles/example</link>
            <source url="https://publisher.example">Example Publisher</source>
            <pubDate>Wed, 25 Nov 2026 16:11:30 GMT</pubDate>
            <description>&lt;p&gt;A useful summary.&lt;/p&gt;</description>
          </item>
          <item>
            <title>Insecure result</title>
            <link>http://publisher.example/article</link>
            <source>Untrusted</source>
          </item>
          <item>
            <title>Missing link</title>
            <source>Untrusted</source>
          </item>
        </channel></rss>"""

        results = list(ENGINE.response(SimpleNamespace(content=feed)))
        self.assertEqual(len(results), 1)
        item = results[0]
        self.assertEqual(item.title, "Example report - Example Publisher")
        self.assertEqual(item.url, "https://news.google.com/rss/articles/example")
        self.assertEqual(item.publishedDate, datetime(2026, 11, 25, 16, 11, 30, tzinfo=timezone.utc))
        self.assertIn("Source: Example Publisher", item.content)
        self.assertIn("Published: Wed, 25 Nov 2026 16:11:30 GMT", item.content)
        self.assertIn("A useful summary.", item.content)

    def test_response_keeps_malformed_date_unset_and_caps_results(self):
        items = []
        for index in range(ENGINE.max_results + 2):
            date = "not a date" if index == 0 else "Wed, 25 Nov 2026 16:11:30 GMT"
            items.append(
                f"<item><title>Story {index}</title>"
                f"<link>https://news.google.com/rss/articles/{index}</link>"
                f"<source>Publisher {index}</source><pubDate>{date}</pubDate></item>"
            )
        feed = ("<rss><channel>" + "".join(items) + "</channel></rss>").encode()

        results = list(ENGINE.response(SimpleNamespace(content=feed)))
        self.assertEqual(len(results), ENGINE.max_results)
        self.assertIsNone(results[0].publishedDate)
        self.assertIsNotNone(results[1].publishedDate)


if __name__ == "__main__":
    unittest.main(verbosity=2)
