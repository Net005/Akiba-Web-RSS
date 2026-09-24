import tempfile
import unittest
from pathlib import Path
from threading import Lock
from unittest.mock import Mock, patch

from akiba_rss_server import RSSServer


class PushoverTests(unittest.TestCase):
    def make_server(self):
        server = RSSServer.__new__(RSSServer)
        server.config = {
            "pushover_destinations": [
                {"api_token": "token-1", "user_key": "user-1"},
                {"api_token": "token-2", "user_key": "user-2"},
            ]
        }
        server.state = {
            "found_links": {},
            "notified_releases": {},
            "pending_notifications": {"SPSF-1": True},
            "pushover_deliveries": {},
        }
        server.state_file = Path(tempfile.mkdtemp()) / "state.json"
        server.state_lock = Lock()
        return server

    @patch("akiba_rss_server.requests.post")
    def test_sends_exact_message_once_to_each_destination(self, post):
        post.return_value = Mock(
            raise_for_status=Mock(), json=Mock(return_value={"status": 1})
        )
        server = self.make_server()

        self.assertTrue(server.send_pushover_notification("SPSF-1"))
        self.assertTrue(server.send_pushover_notification("SPSF-1"))

        self.assertEqual(post.call_count, 2)
        for call in post.call_args_list:
            self.assertEqual(call.kwargs["data"]["message"], "New AW release found")
        self.assertIn("SPSF-1", server.state["notified_releases"])
        self.assertNotIn("SPSF-1", server.state["pending_notifications"])

    @patch("akiba_rss_server.requests.post")
    def test_retry_only_resends_failed_destination(self, post):
        success = Mock(
            raise_for_status=Mock(), json=Mock(return_value={"status": 1})
        )
        failure = Mock(raise_for_status=Mock(side_effect=RuntimeError("failed")))
        post.side_effect = [success, failure, success]
        server = self.make_server()

        self.assertFalse(server.send_pushover_notification("SPSF-1"))
        self.assertTrue(server.send_pushover_notification("SPSF-1"))

        self.assertEqual(post.call_count, 3)
        self.assertEqual(post.call_args_list[-1].kwargs["data"]["user"], "user-2")

    @patch("akiba_rss_server.requests.get")
    @patch("akiba_rss_server.requests.post")
    def test_sensitive_destination_uses_rss_content_and_cover(self, post, get):
        server = self.make_server()
        server.config["pushover_destinations"] = [{
            "api_token": "token-1",
            "user_key": "user-1",
            "include_sensitive_data": True,
        }]
        get.return_value = Mock(
            content=b"cover bytes",
            headers={"Content-Type": "image/jpeg"},
            raise_for_status=Mock(),
        )
        post.return_value = Mock(
            raise_for_status=Mock(), json=Mock(return_value={"status": 1})
        )

        self.assertTrue(server.send_pushover_notification(
            "SPSF-1",
            rss_title="[SPSF-1] Release title",
            release_story="The release story.",
            cover_url="https://example.test/cover.jpg",
            release_url="https://www.akiba-web.com/product/1",
        ))

        request = post.call_args.kwargs
        self.assertEqual(request["data"]["title"], "[SPSF-1] Release title")
        self.assertEqual(request["data"]["message"], "The release story.")
        self.assertEqual(
            request["data"]["url"], "https://www.akiba-web.com/product/1"
        )
        self.assertEqual(request["data"]["url_title"], "View on Akiba-Web")
        self.assertEqual(request["files"]["attachment"][1], b"cover bytes")

    def test_myjd_duplicate_error_detection(self):
        self.assertTrue(RSSServer._is_myjd_duplicate("DUPLICATE_LINK"))
        self.assertTrue(RSSServer._is_myjd_duplicate("link already exists"))
        self.assertFalse(RSSServer._is_myjd_duplicate("connection failed"))


if __name__ == "__main__":
    unittest.main()
