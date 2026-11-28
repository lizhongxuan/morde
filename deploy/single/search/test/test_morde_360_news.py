import importlib.util
import unittest
from types import SimpleNamespace
from unittest.mock import patch
from urllib.parse import parse_qs, urlparse

from lxml.html import fromstring


ENGINE_PATH = "/tmp/morde_360_news.py"
SPEC = importlib.util.spec_from_file_location("morde_360_news", ENGINE_PATH)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"could not load news engine from {ENGINE_PATH}")
ENGINE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ENGINE)


class FakeResponse:
    def __init__(self, text):
        self.text = text

    def html(self):
        return fromstring(self.text)


class Morde360NewsEngineTests(unittest.TestCase):
    def test_request_reuses_upstream_cookie_and_time_range_mapping(self):
        expected = {"day": "d", "week": "w", "month": "m", "year": "y"}
        for time_range, provider_range in expected.items():
            with self.subTest(time_range=time_range), patch.object(ENGINE._upstream, "get_cookie", return_value="so=fixture"):
                params = {"pageno": 1, "time_range": time_range, "headers": {}}
                ENGINE.request("climate policy", params)
                query = parse_qs(urlparse(params["url"]).query)
                self.assertEqual(query["q"], ["climate policy"])
                self.assertEqual(query["adv_t"], [provider_range])
                self.assertEqual(params["headers"]["Cookie"], "so=fixture")
                self.assertTrue(params["url"].startswith("https://www.so.com/s?"))

    def test_request_without_time_range_does_not_add_filter(self):
        with patch.object(ENGINE._upstream, "get_cookie", return_value="so=fixture"):
            params = {"pageno": 1, "time_range": None, "headers": {}}
            ENGINE.request("climate policy", params)
        query = parse_qs(urlparse(params["url"]).query)
        self.assertNotIn("adv_t", query)

    def test_response_returns_direct_https_publishers_without_relative_dates(self):
        page = """<html><body>
          <div id="mohe-news"><ul><li><div class="g-figure-caption">
            <span class="mh-time">1天前</span>
            <div class="mh-info"><a title="Example policy update" href="https://www.so.com/link?x=1"
              data-mdurl="https://publisher.example/article">Example policy update</a>
              <span class="mh-origin">Example Publisher</span></div>
            <div class="mh-info"><a title="Insecure story" data-mdurl="http://publisher.example/story">Insecure story</a></div>
            <div class="mh-info"><a title="Tracking result" href="https://www.so.com/link?x=2">Tracking result</a></div>
            <div class="mh-info"><a title="Aggregator" data-mdurl="https://news.so.com/ns?q=policy">Aggregator</a></div>
            <div class="mh-info"><a title="Private result" data-mdurl="https://127.0.0.1/article">Private result</a></div>
          </div></li></ul></div>
          <div><a title="Outside news widget" data-mdurl="https://publisher.example/other">Outside</a></div>
        </body></html>"""
        results = list(ENGINE.response(FakeResponse(page)))
        self.assertEqual(len(results), 1)
        self.assertEqual(results[0].title, "Example policy update")
        self.assertEqual(results[0].url, "https://publisher.example/article")
        self.assertEqual(results[0].content, "Publisher: Example Publisher")
        self.assertIsNone(results[0].publishedDate)
        self.assertNotIn("1天前", results[0].content)
        self.assertNotIn("so.com", results[0].url)

    def test_response_falls_back_to_standard_results_without_news_widget(self):
        page = """<html><body>
          <li class="res-list" data-result-type="normal">
            <h3 class="res-title"><a href="https://www.so.com/link?m=redirect"
              data-mdurl="https://publisher.example/current-story">Current story</a></h3>
            <p class="res-desc"><span>5小时前 - </span><span data-result-summary>News summary.</span></p>
            <p class="g-linkinfo"><cite>publisher.example</cite></p>
          </li>
          <li class="res-list"><h3 class="res-title">
            <a href="https://www.so.com/link?m=only-redirect">Redirect only</a>
          </h3></li>
          <li class="res-list"><h3 class="res-title">
            <a data-mdurl="http://publisher.example/insecure">Insecure</a>
          </h3></li>
        </body></html>"""
        results = list(ENGINE.response(FakeResponse(page)))
        self.assertEqual(len(results), 1)
        self.assertEqual(results[0].title, "Current story")
        self.assertEqual(results[0].url, "https://publisher.example/current-story")
        self.assertEqual(results[0].content, "Publisher: publisher.example")
        self.assertNotIn("5小时前", results[0].content)

    def test_response_falls_back_when_news_widget_has_no_usable_candidate(self):
        page = """<html><body>
          <div id="mohe-news">
            <a title="Redirect only" href="https://www.so.com/link?m=redirect">Redirect only</a>
            <a title="Insecure widget result" data-mdurl="http://publisher.example/story">Insecure</a>
          </div>
          <li class="res-list"><h3 class="res-title">
            <a href="https://www.so.com/link?m=current"
              data-mdurl="https://publisher.example/current-story">Current story</a>
          </h3></li>
          <li class="res-list"><h3 class="res-title">
            <a href="https://www.so.com/link?m=aggregator"
              data-mdurl="https://news.so.com/article">Aggregator</a>
          </h3></li>
        </body></html>"""
        results = list(ENGINE.response(FakeResponse(page)))
        self.assertEqual(len(results), 1)
        self.assertEqual(results[0].title, "Current story")
        self.assertEqual(results[0].url, "https://publisher.example/current-story")
        self.assertNotIn("so.com", results[0].url)

    def test_response_deduplicates_and_caps_results(self):
        links = [
            f'<a title="Story {index}" data-mdurl="https://publisher.example/{index}">Story</a>'
            for index in range(ENGINE.max_results + 3)
        ]
        links.insert(1, links[0])
        html = f'<div id="mohe-news">{"".join(links)}</div>'
        results = list(ENGINE.response(FakeResponse(html)))
        self.assertEqual(len(results), ENGINE.max_results)
        self.assertEqual(len({result.url for result in results}), ENGINE.max_results)

    def test_response_handles_parser_returning_no_document(self):
        self.assertEqual(list(ENGINE.response(SimpleNamespace(text="not empty", html=lambda: None))), [])

    def test_response_handles_empty_or_missing_news_module(self):
        self.assertEqual(list(ENGINE.response(SimpleNamespace(text="", html=lambda: None))), [])
        self.assertEqual(list(ENGINE.response(FakeResponse("<html><body>no news</body></html>"))), [])


if __name__ == "__main__":
    unittest.main(verbosity=2)
