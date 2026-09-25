# Akiba-Web RSS Control

A pure Go service that scrapes [akiba-web.com](https://www.akiba-web.com) GIGA
superheroine releases, matches their Keep2Share download links from a
configured vipergirls.to forum thread, queues new links in MyJDownloader,
sends Pushover notifications, and publishes an RSS feed — with a themed,
dense ops-console web control panel and live log built in. No Python, no
external dependencies beyond the Go modules in `go.sum`; the web UI is
embedded in the binary.

## Run

```sh
chmod +x akiba-web-rss-linux-amd64
./akiba-web-rss-linux-amd64 -data ./data     # then open http://<host>:5000/
```

`-data` is the folder for `settings.json`, `auth.json`, the state file, feed
and logs (the `AKIBA_DATA` env var works too). A systemd user unit is included
(`akiba-web-rss.service`). Rebuild from source with Go 1.25+:

```sh
CGO_ENABLED=0 go build -o akiba-web-rss .
```

## Docker

```sh
docker compose up -d
docker compose logs -f      # first run: look for the FIRST-RUN SETUP line with your setup code
```

Everything lives in the `./data` volume (uid/gid 1000 — `chown -R 1000:1000 data`).
Images are published to `ghcr.io/net005/akiba-web-rss` by GitHub Actions
(`.github/workflows/docker.yml`, multi-arch: linux/amd64, linux/arm64).

## First run, login and settings

There is no `config.json` any more — all configuration is edited in the web
panel's Settings page and stored in `data/settings.json`.

* **Existing `config.json`:** if `data/config.json` exists and there is no
  `settings.json` yet, it is imported automatically on startup and renamed to
  `config.json.imported`. An invalid file is left completely untouched and
  reported in the log rather than risk losing it.
* **Account:** sign in with a normal form — no browser/OS-style Basic Auth
  popup. On first run, open `/setup`, enter the one-time **setup code printed
  in the server log**, and choose a username + password (bcrypt-hashed in
  `auth.json`). A `web_user`/`web_password` found in an imported config
  becomes that account automatically.
* **Sessions:** HttpOnly + SameSite=Lax cookie (Secure when served over
  HTTPS); 30 days if "keep me signed in", otherwise 12h. Failed logins are
  throttled per IP with exponential backoff. Changing the password signs out
  every other session.
* **RSS feed needs no login:** `/giga/feed`, `/giga/feed/raw` and `/health`
  are always public. Everything else — the panel, the JSON API, and the
  legacy `/giga/feed/refresh` and `/giga/feed/realtime` endpoints — requires a
  session.
* **State file:** if `.scraper_state.json` is missing or unreadable, a fresh
  one is created automatically (a corrupt file is preserved alongside it as
  `.scraper_state.json.corrupt-<timestamp>`); this is never a startup
  failure.

## Control panel

| Page | What it does |
|------|--------------|
| Dashboard | live pipeline phase + progress bar, current item, stat tiles, mini log, connection summary, feed URL, Run / Stop |
| Live Log | real-time stream (SSE), level filters, text search, pause, follow, wrap, download, clear |
| Releases | every tracked release as a full-bleed cover-art tile, link/queue progress, per-release "Send to JD", re-notify and forget |
| Run History | every pipeline run — result, releases found, matched, links queued, notifications sent, duration |
| System | MyJDownloader devices + reconnect, Pushover destination count, public endpoint reference, list of every known download link |
| Settings | every configuration field plus the account password, all in the browser (secrets are masked in API responses) |

Extra settings beyond the essentials: `listen_host`, `public_base_url`,
`forum_retries`, `releases_file`, `history_file`. `schedule_interval` and the
MyJDownloader credentials apply immediately on save; `port` and `listen_host`
need a restart.

### Pushover

Add one destination per notification target in Settings. Each destination has
its own API token, user/group key, and an "include sensitive data" toggle:

* off — sends only `New AW release found`.
* on — uses the release title, story, cover image and a link to the product
  page.

Pushover limits titles to 250 UTF-8 bytes, messages to 1,024 UTF-8 bytes, and
attachments to 5 MiB; longer content is truncated safely. Deliveries are
tracked per release per destination in the state file, so a failed
destination is retried on the next run without re-notifying destinations that
already succeeded.

## The forum "Connection reset by peer" bug

Old behaviour: page-discovery failed → the code assumed the thread had a
single page → it scraped page 1 (the oldest) → new releases never matched,
silently.

Now: every forum request is retried with exponential backoff on a fresh
connection; discovery has a second strategy (request an out-of-range page)
and probes forward from the last known page; the last known thread length is
persisted in the state file (`forum_max_page`). If nothing is known, the
forum step is skipped with an explicit error and previously known links stay
in the feed. Covered by `forum_test.go` (`go test ./...`).

## The Akiba-Web listing "failed to locate .search_sam_box" bug

`akiba-web.com`'s age-gate endpoint clears the pre-gate session cookie with a
domain-scoped `Set-Cookie: PHPSESSID=deleted`. Go's `net/http/cookiejar`
honors that delete strictly and drops the real session, so the listing page
falls back to the age-gate HTML (still HTTP 200) with no releases to parse.
Fixed with a cookie-jar wrapper that ignores that specific deletion, plus a
defensive retry-with-reprime if a fetch still comes back empty. Covered by the
opt-in live test in `live_test.go` (`LIVE=1 go test -run TestLiveAkiba -v`).
