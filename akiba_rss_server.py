#!/usr/bin/env python3
"""
Akiba-Web RSS Server with MyJDownloader Integration
Scrapes GIGA superheroine releases, matches forum download links, and serves RSS.
All configuration is read from config.json.
Author: Rick
"""

import os
import sys
import json
import time
import hashlib
import logging
import re
import random
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import parse_qsl, urlencode, urljoin, urlsplit, urlunsplit
from threading import Thread, Lock
from collections import defaultdict
from flask import Flask, Response
from bs4 import BeautifulSoup
import requests

# Third-party imports
try:
    from feedgen.feed import FeedGenerator
except ImportError:
    os.system(f"{sys.executable} -m pip install feedgen --break-system-packages -q")
    from feedgen.feed import FeedGenerator

try:
    from myjdapi import Myjdapi
except ImportError:
    os.system(f"{sys.executable} -m pip install myjdapi --break-system-packages -q")
    from myjdapi import Myjdapi

SCRIPT_DIR = Path(__file__).parent.resolve()
CONFIG_PATH = SCRIPT_DIR / "config.json"

def load_config(config_path):
    """Load configuration from JSON file with defaults."""
    defaults = {
        "myjd_email": "",
        "myjd_password": "",
        "pushover_destinations": [],
        "akiba_base": "https://www.akiba-web.com",
        "akiba_releases_path": "/search/?narrow=2&sort=1",
        "forum_base": "https://vipergirls.to",
        "forum_thread": "threads/5913172-Heroine-superheroine-JAV-movie-collection",
        "forum_pages": 5,
        "port": 5000,
        "schedule_interval": 3600,
        "rss_file": str(SCRIPT_DIR / "feed.rss"),
        "state_file": str(SCRIPT_DIR / ".scraper_state.json"),
        "log_file": str(SCRIPT_DIR / "scraper.log"),
    }

    if not os.path.exists(config_path):
        print(f"Config not found at {config_path}, using defaults")
        return defaults

    try:
        with open(config_path) as f:
            config = json.load(f)
        for key, value in defaults.items():
            if key not in config:
                config[key] = value
        # Storage paths in config are relative to the application, not the
        # shell's current working directory. Absolute paths remain supported.
        for key in ("rss_file", "state_file", "log_file"):
            path = Path(config[key]).expanduser()
            if not path.is_absolute():
                path = SCRIPT_DIR / path
            config[key] = str(path.resolve())
        return config
    except Exception as e:
        raise RuntimeError(
            f"Config load failed for {config_path}: {e}. "
            "Refusing to start with blank fallback credentials."
        ) from e

# Load config early for logging setup
_config_early = load_config(CONFIG_PATH)

from logging.handlers import RotatingFileHandler

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
    handlers=[
        logging.StreamHandler(sys.stdout),
        RotatingFileHandler(_config_early["log_file"], maxBytes=10*1024*1024, backupCount=1),
    ],
)

logger = logging.getLogger(__name__)

class AkibaScraper:
    """Scrapes akiba-web.com for GIGA superheroine releases."""

    def __init__(self, base_url, timeout=30):
        self.base_url = base_url.rstrip("/")
        self.session = requests.Session()
        self.timeout = timeout
        self._prime_session()

    def _prime_session(self):
        """Enter the age gate and establish the session required by deep links."""
        try:
            headers = {"User-Agent": "Mozilla/5.0"}
            self.session.get(
                self.base_url,
                headers=headers,
                timeout=self.timeout,
            )
            # The redesigned site checks both cookies and session state.  Visiting
            # this endpoint sets old_check/layout; top.php then primes deep links.
            self.session.get(
                urljoin(self.base_url + "/", "cookie_set.php"),
                headers=headers,
                timeout=self.timeout,
                allow_redirects=False,
            )
            resp = self.session.get(
                urljoin(self.base_url + "/", "top.php"),
                headers=headers,
                timeout=self.timeout,
            )
            resp.raise_for_status()
            logger.info(f"Akiba session primed: {resp.status_code}")
        except Exception as e:
            logger.error(f"Akiba session prime failed: {e}")

    def fetch_releases(self, releases_path="/search/?narrow=2"):
        """Fetch the redesigned release listing and extract its product cards."""
        try:
            listing_url = urljoin(self.base_url + "/", releases_path.lstrip("/"))
            # sort=1 is the site's "By Realease Day: [ New ]" mode.  Rebuild
            # the query instead of appending blindly so custom config remains safe.
            parts = urlsplit(listing_url)
            query = dict(parse_qsl(parts.query, keep_blank_values=True))
            query["sort"] = "1"
            listing_url = urlunsplit(
                (parts.scheme, parts.netloc, parts.path, urlencode(query), parts.fragment)
            )
            resp = self.session.get(
                listing_url,
                headers={
                    "Referer": self.base_url,
                    "User-Agent": "Mozilla/5.0",
                },
                timeout=self.timeout,
            )
            resp.raise_for_status()

            soup = BeautifulSoup(resp.content, "html.parser")
            cards = soup.select(".search_sam_box")
            if not cards:
                logger.error("Failed to locate .search_sam_box on release listing")
                return []

            releases = []
            seen_urls = set()
            for card in cards:
                item = card.select_one("a[href*='product_id']")
                title_tag = card.select_one("a[href*='product_id'] span")
                img = card.select_one(".pac_thum_box img") or card.select_one("img")
                if not item:
                    continue
                href = item.get("href", "")
                full_url = urljoin(self.base_url + "/", href)
                if full_url in seen_urls:
                    continue

                # Product numbers now live as card text, e.g. （SPSF-52）.
                match = re.search(
                    r"\b([A-Z]{2,8})\s*[-‐‑‒–—]\s*(\d{1,4})\b",
                    card.get_text(" ", strip=True),
                    re.IGNORECASE,
                )
                if not match:
                    continue
                release_id = f"{match.group(1).upper()}-{match.group(2)}"
                title = title_tag.get_text(" ", strip=True) if title_tag else ""
                # The theme misspells the sort heading as "Realease". Accept
                # both spellings and both common date separators.
                date_match = re.search(
                    r"(?:realease|release)\s*(?:day|date)?[^0-9]*"
                    r"(\d{4}[-/]\d{1,2}[-/]\d{1,2})",
                    card.get_text(" ", strip=True),
                    re.IGNORECASE,
                )
                listing_release_date = (
                    date_match.group(1).replace("-", "/") if date_match else ""
                )
                releases.append({
                    "id": release_id,
                    "title": title,
                    "url": full_url,
                    "thumbnail": urljoin(self.base_url + "/", img.get("src", "")) if img else "",
                    "listing_release_date": listing_release_date,
                })
                seen_urls.add(full_url)

            # Do not trust the theme to retain its selected order: enforce New
            # first locally, while Python's stable sort preserves undated cards.
            releases.sort(
                key=lambda release: release["listing_release_date"], reverse=True
            )

            logger.info(f"Total releases found: {len(releases)}")
            return releases
        except Exception as e:
            logger.error(f"Homepage fetch failed: {e}")
            return []

    def fetch_homepage(self):
        """Backward-compatible alias for callers using the old method name."""
        return self.fetch_releases()

    def fetch_product_detail(self, product_url):
        """Fetch product page metadata, screenshots, story, and cover."""
        try:
            resp = self.session.get(
                product_url,
                headers={
                    "Referer": self.base_url,
                    "User-Agent": "Mozilla/5.0",
                },
                timeout=self.timeout,
            )
            resp.raise_for_status()

            soup = BeautifulSoup(resp.content, "html.parser")

            # Title
            title_tag = (
                soup.select_one("#works_pic h5")
                or soup.select_one("#works_txt b")
                or soup.select_one("h1")
            )
            title = title_tag.get_text(strip=True) if title_tag else ""

            # Actress
            actress_list = []
            actress_dd = soup.select_one("#works_txt dd.yaku")
            if actress_dd:
                actress_list = [a.get_text(strip=True) for a in actress_dd.select("a")]
            if not actress_list:
                actress_span = soup.select_one("#works_txt span.yaku")
                if actress_span:
                    actress_list = [actress_span.get_text(strip=True)]

            # Director
            director = ""
            director_dt = soup.select_one("#works_txt dt:-soup-contains('Director')")
            if director_dt:
                dd = director_dt.find_next_sibling("dd")
                if dd:
                    a = dd.select_one("a")
                    director = a.get_text(strip=True) if a else dd.get_text(strip=True)

            # Duration
            duration = ""
            time_dt = soup.select_one("#works_txt dt:-soup-contains('Time')")
            if time_dt:
                dd = time_dt.find_next_sibling("dd")
                if dd:
                    duration = dd.get_text(strip=True)

            # Release date
            release_date = ""
            date_dt = soup.select_one("#works_txt dt:-soup-contains('Release Date')")
            if date_dt:
                dd = date_dt.find_next_sibling("dd")
                if dd:
                    release_date = dd.get_text(strip=True)

            # Cover image
            cover_url = ""
            works_pic = soup.select_one("#works_pic img")
            if works_pic:
                cover_url = works_pic.get("src", "")
                if cover_url and not cover_url.startswith("http"):
                    cover_url = urljoin(self.base_url + "/", cover_url)

            # Screenshots from #sample_list (full-size _l.jpg URLs)
            screenshots = []
            sample_links = soup.select("#sample_list a[href], .gasatsu_images_pc a[href]")
            for a in sample_links:
                href = a.get("href", "")
                if href and "_l.jpg" in href:
                    href = urljoin(self.base_url + "/", href)
                    if href not in screenshots:
                        screenshots.append(href)
            # Story from #story_list2 (expanded full-text version)
            story = ""
            story_div = soup.select_one("#story_list2 .story_window")
            if story_div:
                story = story_div.get_text(strip=True)
            elif soup.select_one("#story_list1 .story_window"):
                story = soup.select_one("#story_list1 .story_window").get_text(strip=True)

            logger.info(
                f"Product detail: {title[:30]}... — {len(screenshots)} screenshots, "
                f"actress={', '.join(actress_list) if actress_list else 'Unknown'}"
            )

            return {
                "title": title,
                "actress": ", ".join(actress_list) if actress_list else "Unknown",
                "director": director if director else "Unknown",
                "duration": duration if duration else "Unknown",
                "release_date": release_date if release_date else "Unknown",
                "cover": cover_url,
                "screenshots": screenshots,
                "story": story,
            }
        except Exception as e:
            logger.error(f"Product detail fetch failed for {product_url}: {e}")
            return {}

    def close(self):
        self.session.close()

class ForumScraper:
    """Scrapes vipergirls.to forum thread for k2s.cc download links."""

    def __init__(self, forum_base, forum_thread, max_pages=5, timeout=30):
        self.forum_base = forum_base.rstrip("/")
        self.forum_thread = forum_thread.strip("/")
        self.max_pages = max_pages
        self.timeout = timeout
        self.session = requests.Session()
        self.session.headers.update({
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
                          "(KHTML, like Gecko) Chrome/120.0 Safari/537.36",
            "Accept-Language": "en-US,en;q=0.9",
        })

    def _build_url(self, page_num):
        """Build forum thread URL for given page number (FIXED: page{n} not page-{n})."""
        return f"{self.forum_base}/{self.forum_thread}/page{page_num}"

    def _discover_max_page(self):
        """Try to find last page from thread pagination (FIXED regex from page- to page)."""
        try:
            url = f"{self.forum_base}/{self.forum_thread}"
            resp = self.session.get(url, timeout=self.timeout)
            soup = BeautifulSoup(resp.content, "html.parser")

            max_page = 1

            # Check selected/current page spans
            for span in soup.select("span.selected, span.curr, a.selected"):
                txt = span.get_text(strip=True)
                if txt.isdigit():
                    max_page = max(max_page, int(txt))

            # Check pagination links - FIXED: vBulletin uses page{n} not page-{n}
            for a in soup.select("a[href]"):
                href = a.get("href", "")
                match = re.search(r'/page(\d+)', href)  # FIXED: removed hyphen
                if match:
                    num = int(match.group(1))
                    if num > max_page:
                        max_page = num

            # Check rel='last' links - FIXED: removed hyphen
            for a in soup.select("a[rel='last'], a.lastpage"):
                href = a.get("href", "")
                match = re.search(r'/page(\d+)', href)  # FIXED: removed hyphen
                if match:
                    max_page = max(max_page, int(match.group(1)))

            return max_page
        except Exception as e:
            logger.error(f"Forum max page discovery failed: {e}")
            return 1

    def fetch_forum_links(self):
        """Fetch k2s.cc links from the last N pages of the forum thread."""
        links_by_release = defaultdict(list)

        max_page = self._discover_max_page()
        pages_to_scrape = list(
            range(max(max_page - self.max_pages + 1, 1), max_page + 1)
        )
        logger.info(
            f"Forum thread max page={max_page}, "
            f"scraping pages {pages_to_scrape[0]}-{pages_to_scrape[-1]}"
        )

        for page_num in pages_to_scrape:
            try:
                url = self._build_url(page_num)
                resp = self.session.get(url, timeout=self.timeout)
                resp.raise_for_status()

                soup = BeautifulSoup(resp.content, "html.parser")
                found_ids = set()

                for link in soup.select("a[href*='k2s.cc'], a[href*='k2s.to']"):
                    href = link.get("href", "")
                    text = link.get_text(strip=True)

                    # Combine link text + href to search for release ID
                    combined = f"{text} {href}"

                    # Use re.search (not match) to find ID embedded in filename
                    match = re.search(r'([A-Z]{2,8})-(\d{2,4})', combined, re.IGNORECASE)
                    if match:
                        rid = f"{match.group(1).upper()}-{match.group(2)}"
                        found_ids.add(rid)
                        if href not in links_by_release[rid]:
                            links_by_release[rid].append(href)

                logger.info(
                    f"Forum page {page_num}: found links for "
                    f"{len(found_ids)} release IDs"
                )
            except Exception as e:
                logger.error(f"Forum page {page_num} failed: {e}")

        total = sum(len(v) > 0 for v in links_by_release.values())
        logger.info(f"Forum: found k2s links for {total} release IDs")
        return dict(links_by_release)

def escape_html(text):
    """Basic HTML escaping for text content."""
    if not text:
        return ""
    return (
        text.replace("&", "&amp;")
        .replace("<", "&lt;")
        .replace(">", "&gt;")
        .replace('"', "&quot;")
    )

def build_description(detail, release_id, download_links):
    """
    Build HTML description for RSS item.
    Layout: Cover → Metadata → Story → Screenshots (5x bigger) → Downloads
    """
    html_parts = []
    cover_url = detail.get("cover", "")
    story = detail.get("story", "")
    screenshots = detail.get("screenshots", [])

    # 1. Cover image
    if cover_url:
        html_parts.append(
            f'<div style="text-align:center;margin-bottom:10px;">'
            f'<img src="{cover_url}" style="max-width:300px;'
            f'border:1px solid #444;border-radius:4px;" /></div>'
        )

    # 2. Title + metadata table
    title = detail.get("title", release_id)
    html_parts.append(
        f'<h3 style="margin:0;">{release_id} — {escape_html(title)}</h3>'
    )
    html_parts.append('<hr style="border-color:#444;" />')
    html_parts.append(
        '<table style="border-collapse:collapse;width:100%;font-size:14px;">'
    )
    for label, value in [
        ("Actress", detail.get("actress", "")),
        ("Director", detail.get("director", "")),
        ("Duration", detail.get("duration", "")),
        ("Release Date", detail.get("release_date", "")),
        ("Product No.", release_id),
    ]:
        ev = escape_html(str(value)) if value else "Unknown"
        html_parts.append(
            f'<tr><td style="padding:4px 8px;font-weight:bold;'
            f'width:120px;color:#aaa;">{label}</td>'
            f'<td style="padding:4px 8px;">{ev}</td></tr>'
        )
    html_parts.append("</table>")

    # 3. Story text
    if story:
        html_parts.append(
            f'<div style="margin:10px 0;padding:8px;background:#1a1a1a;'
            f'border-radius:4px;font-size:13px;line-height:1.6;color:#ddd;">'
            f"{escape_html(story)}</div>"
        )

    # 4. Inline screenshots (always _l.jpg full-size)
    if screenshots:
        html_parts.append('<hr style="border-color:#444;margin-top:10px;" />')
        html_parts.append(
            '<div style="display:flex;flex-wrap:wrap;gap:8px;margin:8px 0;">'
        )
        for ss_url in screenshots:
            # Always use _l.jpg for both link and inline image
            html_parts.append(
                f'<a href="{ss_url}"><img src="{ss_url}" '
                f'style="width:600px;height:auto;border:2px solid #444;'
                f'border-radius:4px;display:block;margin:4px 0;" /></a>'
            )
        html_parts.append("</div>")

    # 5. Download links
    if download_links:
        html_parts.append('<hr style="border-color:#444;margin-top:10px;" />')
        html_parts.append(
            '<h4 style="margin:5px 0;">Download Links (keep2share)</h4>'
        )
        for link in download_links:
            fname = link.split("/")[-1].split("?")[0]
            html_parts.append(
                f'<p style="margin:4px 0;"><a href="{link}">{fname}</a></p>')

    return "\n".join(html_parts)

class RSSServer:
    """Flask server that scrapes, caches, and serves an RSS feed."""

    def __init__(self):
        self.config = load_config(CONFIG_PATH)

        self.port = self.config["port"]
        self.schedule_interval = self.config["schedule_interval"]
        self.rss_file = Path(self.config["rss_file"])
        self.state_file = Path(self.config["state_file"])

        self.app = Flask(__name__)
        self.cache_lock = Lock()
        self.pipeline_lock = Lock()
        self.state_lock = Lock()
        self.last_cache_time = 0
        self.cache_ttl_seconds = 7200

        self.akiba_scraper = None
        self.forum_scraper = None
        self.jd = None
        self.jd_devices = []
        self.jd_round_robin = 0
        self.state = {
            "found_links": {},
            "queued_releases": {},
            "notified_releases": {},
            "pending_notifications": {},
            "pushover_deliveries": {},
            "round_robin_index": 0,
        }

        self._setup_routes()

    def _setup_routes(self):
        @self.app.route("/")
        def home():
            return Response(
                "<h1>Access Denied</h1><p>This endpoint requires authentication.</p>",
                status=403,
                mimetype="text/html"
            )

        @self.app.route("/giga/feed", strict_slashes=False)
        def rss():
            # Check cache WITHOUT triggering scrape
            with self.cache_lock:
                now = time.time()
                cache_age = now - self.last_cache_time

                if cache_age < self.cache_ttl_seconds:
                    response = Response(
                        self.rss_file.read_text(encoding='utf-8'),
                        mimetype="application/rss+xml; charset=utf-8"
                    )
                    response.headers['Content-Type'] = 'application/rss+xml; charset=utf-8'
                    return response

            Thread(target=self.run_pipeline, daemon=True).start()

            response = Response(
                self.rss_file.read_text(encoding='utf-8'),
                mimetype="application/rss+xml; charset=utf-8"
            )
            response.headers['Content-Type'] = 'application/rss+xml; charset=utf-8'
            return response

        @self.app.route("/giga/feed/raw", strict_slashes=False)
        def raw_xml():
            response = Response(
                self.rss_file.read_text(encoding='utf-8'),
                mimetype="application/xml; charset=utf-8"
            )
            response.headers['Content-Type'] = 'application/xml; charset=utf-8'
            return response

        @self.app.route("/giga/feed/realtime")
        def realtime():
            # FORCE scrape - always blocks until done
            self.run_pipeline()
            return {"status": "scraped", "path": str(self.rss_file)}

        @self.app.route("/giga/feed/refresh")
        def refresh_html():
            # FORCE scrape - always blocks until done
            self.run_pipeline()
            return "<h1>Feed refreshed!</h1>"

        @self.app.route("/health")
        def health():
            return {
                "status": "ok",
                "jd_connected": bool(self.jd),
                "jd_devices": len(self.jd_devices),
                "state_found_links": len(self.state["found_links"]),
                "state_queued_releases": len(
                    self.state.get("queued_releases", {})
                ),
                "rss_file": str(self.rss_file),
                "uptime": int(time.time() - start_time),
                "cache_age_seconds": time.time() - self.last_cache_time,
            }

    # ── Device Number Parsing ──────────────────────────────────────────

    def _parse_device_number(self, device_name):
        """Extract trailing number from device name (e.g., 'Device1' → 1). Returns None if no number."""
        match = re.search(r'(\d+)$', device_name)
        return int(match.group(1)) if match else None

    # ── MyJDownloader ──────────────────────────────────────────────

    def init_myjd(self):
        """Connect to MyJDownloader account and enumerate devices."""
        email = self.config.get("myjd_email", "")
        password = self.config.get("myjd_password", "")
        if not email or not password:
            logger.warning("No MyJD credentials in config, MyJD disabled")
            return False

        try:
            # Build the replacement connection locally so it is published only
            # after both authentication and device enumeration succeed.
            jd = Myjdapi()
            jd.connect(email, password)
            jd_devices = jd.list_devices()

            # Sort devices by trailing number (1, 2, 3...) or alphabetical if no numbers
            def sort_key(dev):
                num = self._parse_device_number(dev['name'])
                return (0, num) if num is not None else (1, dev['name'])

            jd_devices = sorted(jd_devices, key=sort_key)
            self.jd = jd
            self.jd_devices = jd_devices

            logger.info(
                f"MyJD: Connected, {len(self.jd_devices)} devices found (ordered by trailing number)"
            )
            for idx, dev in enumerate(self.jd_devices):
                num = self._parse_device_number(dev['name'])
                logger.info(f"  - [{idx+1}] {dev['name']} (#{num if num is not None else 'N/A'})")
            return True
        except Exception as e:
            logger.error(f"MyJD connection failed: {e}")
            self.jd = None
            self.jd_devices = []
            return False

    @staticmethod
    def _is_invalid_myjd_token(error):
        """Return True when MyJDownloader rejected the shared session token."""
        return "TOKEN_INVALID" in str(error).upper()

    @staticmethod
    def _is_myjd_duplicate(error):
        """Return True when JD says the submitted link is already present."""
        message = str(error).upper()
        return any(marker in message for marker in (
            "DUPLICATE",
            "DUPE",
            "ALREADY EXISTS",
            "ALREADY_EXISTS",
        ))

    def _add_link_to_device(self, device_name, link, release_id):
        """Submit one link using the current MyJDownloader connection."""
        dev_obj = self.jd.get_device(device_name)
        dev_obj.linkgrabber.add_links([{
            "autostart": True,
            "links": link,
            "packageName": release_id,
        }])

    def add_links_to_jd(self, links, release_id):
        """Add EACH link to a different device in numerical order (1→2→3→1...). Tracks SUCCESS per link."""
        if not self.jd or not self.jd_devices:
            logger.warning(
                f"❌ {release_id} — no JD devices, storing links but not forwarding"
            )
            return False

        has_numbers = any(self._parse_device_number(d['name']) for d in self.jd_devices)
        success_count = 0

        for idx, link in enumerate(links):
            if link in self.state.get("found_links", {}):
                logger.info(f"⏭️ {release_id} — link {idx+1} already queued, skipping")
                continue

            if has_numbers:
                device_index = idx % len(self.jd_devices)
                target_device = self.jd_devices[device_index]
            else:
                device_index = self.jd_round_robin % len(self.jd_devices)
                target_device = self.jd_devices[device_index]
                self.jd_round_robin += 1

            if idx > 0:
                sleep_time = random.uniform(3, 5)
                logger.debug(f"⏳ Throttling: waiting {sleep_time:.1f}s before next link")
                time.sleep(sleep_time)

            try:
                try:
                    self._add_link_to_device(
                        target_device["name"], link, release_id
                    )
                except Exception as e:
                    if not self._is_invalid_myjd_token(e):
                        raise

                    logger.warning(
                        "MyJD session expired; reconnecting before retrying "
                        f"{release_id} link {idx+1}/{len(links)}"
                    )
                    if not self.init_myjd():
                        raise RuntimeError(
                            "MyJD session expired and reconnect failed"
                        ) from e

                    # Retry exactly once. Any second failure is handled by the
                    # outer exception block and the link remains unrecorded.
                    self._add_link_to_device(
                        target_device["name"], link, release_id
                    )

                if has_numbers:
                    num = self._parse_device_number(target_device['name'])
                    logger.info(
                        f"✅ {release_id} — link {idx+1}/{len(links)} → {target_device['name']} (#{num})"
                    )
                else:
                    logger.info(
                        f"✅ {release_id} — link {idx+1}/{len(links)} → {target_device['name']} (round-robin)"
                    )

                self.state.setdefault("found_links", {})[link] = True
                success_count += 1

            except Exception as e:
                if self._is_myjd_duplicate(e):
                    self.state.setdefault("found_links", {})[link] = True
                    success_count += 1
                    logger.info(
                        f"✅ {release_id} — link {idx+1}/{len(links)} already "
                        f"exists on {target_device['name']}; marked successful"
                    )
                else:
                    logger.error(
                        f"❌ {release_id} — link {idx+1}/{len(links)} failed on "
                        f"{target_device['name']}: {e}"
                    )

        self.save_state()
        logger.info(f"ℹ️ {release_id} — {success_count}/{len(links)} links successfully queued")
        if success_count == len(links):
            self.state.setdefault("queued_releases", {})[release_id] = (
                datetime.now(timezone.utc).isoformat()
            )
            self.save_state()
        return success_count > 0

    # ── Pushover ─────────────────────────────────────────────────

    @staticmethod
    def _truncate_utf8(value, max_bytes):
        """Truncate text to an API byte limit without splitting a character."""
        encoded = value.encode("utf-8")
        if len(encoded) <= max_bytes:
            return value
        return encoded[:max_bytes].decode("utf-8", errors="ignore")

    def send_pushover_notification(
        self, release_id, rss_title="", release_story="", cover_url="",
        release_url=""
    ):
        """Notify every configured destination once per successfully queued release."""
        notified = self.state.setdefault("notified_releases", {})
        if release_id in notified:
            logger.info(f"⏭️ {release_id} — Pushover notification already sent")
            return True

        destinations = self.config.get("pushover_destinations", [])
        if not destinations:
            logger.info(f"ℹ️ {release_id} — no Pushover destinations configured")
            return False

        deliveries = self.state.setdefault("pushover_deliveries", {}).setdefault(
            release_id, {}
        )
        all_succeeded = True
        cover_attachment = None
        for index, destination in enumerate(destinations, start=1):
            token = destination.get("api_token", "")
            user = destination.get("user_key", "")
            include_sensitive_data = bool(
                destination.get("include_sensitive_data", False)
            )
            destination_id = hashlib.sha256(
                f"{token}\0{user}".encode()
            ).hexdigest()
            if destination_id in deliveries:
                continue
            if not token or not user:
                logger.error(
                    f"❌ {release_id} — Pushover destination {index} is missing "
                    "api_token or user_key"
                )
                all_succeeded = False
                continue

            try:
                payload = {
                    "token": token,
                    "user": user,
                    "message": "New AW release found",
                }
                files = None
                if include_sensitive_data:
                    if not rss_title or not cover_url or not release_url:
                        raise RuntimeError(
                            "release title, cover, or URL is unavailable"
                        )
                    if cover_attachment is None:
                        cover_response = requests.get(cover_url, timeout=30)
                        cover_response.raise_for_status()
                        cover_bytes = cover_response.content
                        if len(cover_bytes) > 5 * 1024 * 1024:
                            raise RuntimeError("release cover exceeds Pushover's 5 MiB limit")
                        cover_type = cover_response.headers.get(
                            "Content-Type", "image/jpeg"
                        ).split(";", 1)[0]
                        if not cover_type.startswith("image/"):
                            raise RuntimeError("release cover response is not an image")
                        cover_attachment = ("release-cover", cover_bytes, cover_type)

                    payload.update({
                        "title": self._truncate_utf8(rss_title, 250),
                        "message": self._truncate_utf8(
                            release_story.strip() or "Story unavailable", 1024
                        ),
                        "url": release_url,
                        "url_title": "View on Akiba-Web",
                    })
                    files = {"attachment": cover_attachment}

                response = requests.post(
                    "https://api.pushover.net/1/messages.json",
                    data=payload,
                    files=files,
                    timeout=30,
                )
                response.raise_for_status()
                result = response.json()
                if result.get("status") != 1:
                    raise RuntimeError(result.get("errors", "unknown API error"))
                logger.info(
                    f"🔔 {release_id} — Pushover destination {index} notified"
                )
                deliveries[destination_id] = datetime.now(timezone.utc).isoformat()
                self.save_state()
            except Exception as e:
                all_succeeded = False
                logger.error(
                    f"❌ {release_id} — Pushover destination {index} failed: {e}"
                )

        if all_succeeded:
            notified[release_id] = datetime.now(timezone.utc).isoformat()
            self.state.setdefault("pending_notifications", {}).pop(release_id, None)
            self.save_state()
        return all_succeeded

    # ── State persistence ───────────────────────────────────────────

    def load_state(self):
        if self.state_file.exists():
            try:
                with open(self.state_file) as f:
                    self.state = json.load(f)
                # Migrate state files created before Pushover support.
                self.state.setdefault("found_links", {})
                self.state.setdefault("queued_releases", {})
                self.state.setdefault("notified_releases", {})
                self.state.setdefault("pending_notifications", {})
                self.state.setdefault("pushover_deliveries", {})
                self.state.setdefault("round_robin_index", 0)
                legacy_timestamp = datetime.fromtimestamp(
                    self.state_file.stat().st_mtime, tz=timezone.utc
                ).isoformat()
                for release_id, queued_at in list(
                    self.state["queued_releases"].items()
                ):
                    if not isinstance(queued_at, str):
                        self.state["queued_releases"][release_id] = legacy_timestamp
                # Migrate old state by extracting release IDs from successfully
                # queued link filenames.
                for link in self.state["found_links"]:
                    match = re.search(
                        r"(?:^|[/_])([A-Z]{2,8}-\d{1,4})(?:_|\.|$)",
                        link,
                        re.IGNORECASE,
                    )
                    if match:
                        self.state["queued_releases"].setdefault(
                            match.group(1).upper(), legacy_timestamp
                        )
                logger.info(
                    f"Loaded state: "
                    f"{len(self.state.get('found_links', {}))} previously found"
                )
            except Exception as e:
                logger.error(f"State load failed: {e}")
                self.state = {
                    "found_links": {},
                    "queued_releases": {},
                    "notified_releases": {},
                    "pending_notifications": {},
                    "pushover_deliveries": {},
                    "round_robin_index": 0,
                }

    def save_state(self):
        try:
            self.state_file.parent.mkdir(parents=True, exist_ok=True)
            temp_file = self.state_file.with_suffix(self.state_file.suffix + ".tmp")
            with self.state_lock:
                with open(temp_file, "w") as f:
                    json.dump(self.state, f, indent=2)
                    f.flush()
                    os.fsync(f.fileno())
                os.replace(temp_file, self.state_file)
        except Exception as e:
            logger.error(f"State save failed: {e}")

    # ── Pipeline ───────────────────────────────────────────────────

    def run_pipeline(self):
        """Run at most one pipeline at a time."""
        if not self.pipeline_lock.acquire(blocking=False):
            logger.info("⏭️ Pipeline already running; duplicate request skipped")
            return False
        try:
            return self._run_pipeline()
        finally:
            self.pipeline_lock.release()

    def _run_pipeline(self):
        """Execute the full scraping pipeline: Akiba → Forum → MyJD → RSS."""
        start = time.time()
        logger.info("═══ Pipeline run started ═══")

        self.load_state()

        # Initialize scrapers from config
        self.akiba_scraper = AkibaScraper(self.config["akiba_base"])
        self.forum_scraper = ForumScraper(
            self.config["forum_base"],
            self.config["forum_thread"],
            max_pages=self.config["forum_pages"],
        )

        # Step 1: Homepage releases
        releases = self.akiba_scraper.fetch_releases(
            self.config.get("akiba_releases_path", "/search/?narrow=2&sort=1")
        )
        if not releases:
            logger.error("No releases found on homepage")
            return

        # Step 2: Forum links
        forum_links = self.forum_scraper.fetch_forum_links()

        # Log what IDs we have from each source for debugging
        akiba_ids = {r["id"] for r in releases}
        forum_ids = set(forum_links.keys())
        matched_ids = akiba_ids & forum_ids

        logger.info(f"Akiba IDs found: {sorted(akiba_ids)}")
        logger.info(f"Forum IDs with k2s links: {sorted(forum_ids)}")
        logger.info(f"Matched IDs (will download): {sorted(matched_ids) if matched_ids else 'NONE'}")

        # Log specific missing matches for debugging
        if akiba_ids and not matched_ids:
            logger.warning(
                f"⚠️ No matches! Forum has {len(forum_ids)} IDs but none match Akiba's {len(akiba_ids)}. "
                f"Check if Akiba series (SPSF/THZA) have forum links posted yet."
            )

        # Step 3: Process each release
        rss_items = []
        processed_count = 0

        sorted_releases = releases

        for release in sorted_releases:
            release_id = release["id"]
            try:
                logger.info(f"Processing {release_id}...")

                detail = self.akiba_scraper.fetch_product_detail(release["url"])
                if not detail:
                    continue

                download_links = forum_links.get(release_id, [])
                description_html = build_description(
                    detail, release_id, download_links
                )
                rss_title = f"[{release_id}] {detail.get('title', '')}"
                if download_links:
                    # Filter out links already successfully queued
                    pending_links = [
                        link for link in download_links
                        if link not in self.state.get("found_links", {})
                    ]

                    if not pending_links:
                        logger.info(
                            f"⏭️ {release_id} — all links already queued, skipping"
                        )
                    else:
                        logger.info(
                            f"✅ {release_id} — {len(pending_links)} new links, "
                            f"{len(download_links)-len(pending_links)} already done"
                        )
                        if self.jd and self.jd_devices:
                            queued = self.add_links_to_jd(pending_links, release_id)
                            if queued:
                                self.state.setdefault("pending_notifications", {})[
                                    release_id
                                ] = True
                                self.save_state()
                        else:
                            logger.warning(
                                f"⚠️ {release_id} — no JD devices"
                            )
                    if release_id in self.state.get("pending_notifications", {}):
                        self.send_pushover_notification(
                            release_id,
                            rss_title=rss_title,
                            release_story=detail.get("story", ""),
                            cover_url=detail.get("cover", ""),
                            release_url=release["url"],
                        )
                else:
                    logger.info(
                        f"❌ {release_id} — no forum links found yet, will retry next run"
                    )

                pub_date_str = (
                    release.get("listing_release_date")
                    or detail.get("release_date", "")
                ).strip()
                try:
                    pub_date = datetime.strptime(
                        pub_date_str, "%Y/%m/%d"
                    ).replace(tzinfo=timezone.utc)
                except (ValueError, TypeError):
                    pub_date = datetime.now(timezone.utc)

                rss_items.append(
                    {
                        "title": rss_title,
                        "link": release["url"],
                        "pub_date": pub_date,
                        "description": description_html,
                        "guid": hashlib.md5(
                            (release_id + release["url"]).encode()
                        ).hexdigest(),
                    }
                )
                processed_count += 1

            except Exception as e:
                logger.error(f"Error processing {release_id}: {e}")
                continue

        # Step 4: Sort by date newest first
        rss_items.sort(key=lambda x: x["pub_date"], reverse=True)

        # Step 5: Generate RSS
        fg = FeedGenerator()
        fg.id(f"http://localhost:{self.port}/")
        fg.title("Akiba-Web Heroine Collection")
        fg.link(href=f"http://localhost:{self.port}/", rel="self")
        fg.description(
            "GIGA superheroine releases with metadata, screenshots (large 600px inline), and download links"
        )
        fg.language("en")
        fg.pubDate(datetime.now(timezone.utc))
        fg.lastBuildDate(datetime.now(timezone.utc))

        # FeedGenerator prepends entries internally, so emit oldest-to-newest
        # to keep the resulting RSS document in New-to-Old order.
        for item in reversed(rss_items):
            fe = fg.add_entry()
            fe.id(item["guid"])
            fe.title(item["title"])
            fe.link(href=item["link"])
            fe.pubDate(item["pub_date"])
            fe.description(item["description"])  # Keep as-is for RSS
            fe.guid(item["guid"], permalink=False)

        self.rss_file.parent.mkdir(parents=True, exist_ok=True)
        fg.rss_file(str(self.rss_file))  # CHANGED: atom_file → rss_file
        self.last_cache_time = time.time()
        logger.info(
            f"═══ Pipeline complete: {processed_count} items, "
            f"{len(matched_ids)} downloads matched, "
            f"RSS saved to {self.rss_file} ({time.time()-start:.1f}s) ═══"
        )

        self.save_state()

        if self.akiba_scraper:
            self.akiba_scraper.close()
        if self.forum_scraper:
            self.forum_scraper.session.close()

    # ── Scheduler ──────────────────────────────────────────────────

    def scheduler_loop(self):
        while True:
            logger.info(f"Scheduler started (interval={self.schedule_interval}s)")
            time.sleep(self.schedule_interval)
            try:
                self.run_pipeline()
            except Exception as e:
                logger.error(f"Scheduler pipeline failed: {e}")

    # ── Entry point ───────────────────────────────────────────────

    def start(self):
        global start_time
        start_time = time.time()

        self.load_state()
        self.init_myjd()

        # Run initial scrape
        self.run_pipeline()

        # Start scheduler thread
        scheduler_thread = Thread(target=self.scheduler_loop, daemon=True)
        scheduler_thread.start()

        logger.info(f"Starting Flask on 0.0.0.0:{self.port}")
        self.app.run(host="0.0.0.0", port=self.port, threaded=True)

if __name__ == "__main__":
    server = RSSServer()
    server.start()
