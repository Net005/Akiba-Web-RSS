# Akiba-Web RSS — Go edition (control panel)

Drop-in replacement for `akiba_rss_server.py`. Same `config.json`, same
`.scraper_state.json`, same feed URL (`/giga/feed`) and `/health`.

## Run

```sh
chmod +x akiba-web-rss-linux-amd64
./akiba-web-rss-linux-amd64 -config ./config.json     # then open http://<host>:<port>/
```

A systemd user unit is included (`akiba-web-rss.service`). Rebuild from source
with Go 1.24+: `CGO_ENABLED=0 go build -o akiba-web-rss .` (the web UI is
embedded in the binary).

## Docker

```sh
mkdir -p data && cp config.example.json data/config.json   # edit it (MyJD login, web_user/web_password…)
docker compose up -d
docker compose logs -f
```

The container keeps everything (`config.json`, state, `feed.rss`, logs) in the
`./data` volume and runs as uid/gid 1000 — `chown -R 1000:1000 data` if needed.
Panel + feed: `http://<host>:5000/` and `/giga/feed`. The port in
`docker-compose.yml` must match `port` in `data/config.json`.
Images are published to `ghcr.io/net005/akiba-web-rss` by GitHub Actions
(`.github/workflows/docker.yml`): `latest` from `main`, `vX.Y.Z` from tags,
`linux/amd64` + `linux/arm64`.

## Control panel

| Page | What it does |
|------|--------------|
| Dashboard | live pipeline phase + progress, next run countdown, stats, mini log, feed URL, Run / Stop |
| Live Log | real-time stream (SSE), level filters, text search, pause, follow, wrap, download |
| Releases | every release with full-bleed cover art, link progress, "Send to JD", re-notify, forget state |
| Activity | history of pipeline runs (result, matches, queued links, notifications, forum status) |
| System | MyJDownloader devices + reconnect, Pushover test buttons, list of links already sent |
| Settings | edit `config.json` in the browser (secrets masked, unknown keys preserved) |

New optional config keys: `listen_host`, `web_user`, `web_password`
(HTTP basic auth for the panel — the RSS feed and `/health` stay public),
`public_base_url`, `forum_retries`, `releases_file`, `history_file`.
`schedule_interval` and MyJD credentials apply immediately; `port` /
`listen_host` need a restart.

## The forum "Connection reset by peer" bug

Old behaviour: page-discovery failed → code assumed the thread had **1 page** →
scraped page 1 (the oldest) → new releases never matched, silently.

Now: every forum request is retried with exponential backoff on a fresh
connection; discovery has a second strategy (request page 99999) and probes
forward from the last known page; the last known thread length is persisted in
the state file (`forum_max_page`); if nothing is known the forum step is skipped
with an explicit error and previously known links stay in the feed. Covered by
tests in `forum_test.go` (`go test ./...`).
