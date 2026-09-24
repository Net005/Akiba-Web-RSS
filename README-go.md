# Akiba-Web RSS — Go edition (control panel)

Drop-in replacement for `akiba_rss_server.py`. Same `config.json`, same
`.scraper_state.json`, same feed URL (`/giga/feed`) and `/health`.

## Run

```sh
chmod +x akiba-web-rss-linux-amd64
./akiba-web-rss-linux-amd64 -data ./data     # then open http://<host>:5000/
```

`-data` is the folder for `settings.json`, `auth.json`, the state file, feed and
logs (env `AKIBA_DATA` works too). A systemd user unit is included
(`akiba-web-rss.service`). Rebuild from source with Go 1.25+:
`CGO_ENABLED=0 go build -o akiba-web-rss .` (the web UI is embedded).

## Docker

```sh
docker compose up -d
docker compose logs -f      # first run: look for the FIRST-RUN SETUP line with your setup code
```

Everything lives in the `./data` volume (uid/gid 1000 — `chown -R 1000:1000 data`).
Images are published to `ghcr.io/net005/akiba-web-rss` by GitHub Actions.

## First run, login and settings

There is **no `config.json` any more** — settings are edited in the web panel
(Settings page) and stored in `data/settings.json`.

* **Existing `config.json`:** if `data/config.json` exists and there is no
  `settings.json`, it is imported automatically on startup and renamed to
  `config.json.imported`. (An invalid file is left untouched and reported in the log.)
* **Account:** log in with a normal form (no browser/Windows-style basic auth
  popup). On first run, open `/setup`, enter the one-time **setup code printed in the
  server log**, and choose a username + password (bcrypt-hashed in `auth.json`).
  A `web_user`/`web_password` from an imported config becomes that account.
* **Sessions:** HttpOnly + SameSite cookie, 30 days if "keep me signed in", otherwise
  12 h; failed logins are throttled per IP; changing the password signs out other devices.
* **RSS feed needs no login:** `/giga/feed`, `/giga/feed/raw` and `/health` are public.
  Everything else (panel, API, `/giga/feed/refresh|realtime`) requires a session.
* **State file:** `.scraper_state.json` missing or unreadable → a fresh one is created
  (a corrupt file is kept as `.scraper_state.json.corrupt-<time>`); never a startup failure.

## Control panel

| Page | What it does |
|------|--------------|
| Dashboard | live pipeline phase + progress, next run countdown, stats, mini log, feed URL, Run / Stop |
| Live Log | real-time stream (SSE), level filters, text search, pause, follow, wrap, download |
| Releases | every release with full-bleed cover art, link progress, "Send to JD", re-notify, forget state |
| Activity | history of pipeline runs (result, matches, queued links, notifications, forum status) |
| System | MyJDownloader devices + reconnect, Pushover test buttons, list of links already sent |
| Settings | all configuration + account password, in the browser (secrets masked) |

Extra settings: `listen_host`, `public_base_url`, `forum_retries`, `releases_file`, `history_file`.
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
