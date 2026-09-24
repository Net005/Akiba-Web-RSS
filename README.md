# Akiba-Web RSS Server

This service scrapes Akiba-Web releases, matches their Keep2Share links from the
configured forum thread, adds new links to MyJDownloader, and publishes an RSS
feed. It can also send one Pushover message per release successfully added to
JDownloader.

## Configuration

All settings are read from `config.json` in the same directory as
`akiba_rss_server.py`. Relative file paths are resolved from that directory.
Restart the service after changing the configuration.

```json
{
  "myjd_email": "name@example.com",
  "myjd_password": "your-myjd-password",
  "pushover_destinations": [
    {
      "api_token": "your-pushover-application-api-token",
      "user_key": "first-user-or-group-key",
      "include_sensitive_data": false
    },
    {
      "api_token": "your-pushover-application-api-token",
      "user_key": "second-user-or-group-key",
      "include_sensitive_data": true
    }
  ],
  "akiba_base": "https://www.akiba-web.com",
  "akiba_releases_path": "/search/?narrow=2&sort=1",
  "forum_base": "https://vipergirls.to",
  "forum_thread": "threads/5913172-Heroine-superheroine-JAV-movie-collection",
  "forum_pages": 3,
  "port": 5050,
  "schedule_interval": 3600,
  "rss_file": "feed.rss",
  "state_file": ".scraper_state.json",
  "log_file": "scraper.log"
}
```

### Pushover

Create a Pushover application to obtain an application API token, then add one
object per destination to `pushover_destinations`. A destination's `user_key`
may be a user key or a delivery group key. The application API token may be
reused when all destinations use the same Pushover application.

For example, if the application token is `azGDORePK8gMaW8It92cA6m9p12345` and the
two destination keys are `uQiRzpo4DXghDmr9QzzfQu27cmVRsG` and
`gznej3rKEVAvPUxu9vvNnqpmZpokzF`, the exact configuration is:

```json
"pushover_destinations": [
  {
    "api_token": "azGDORePK8gMaW8It92cA6m9p12345",
    "user_key": "uQiRzpo4DXghDmr9QzzfQu27cmVRsG",
    "include_sensitive_data": false
  },
  {
    "api_token": "azGDORePK8gMaW8It92cA6m9p12345",
    "user_key": "gznej3rKEVAvPUxu9vvNnqpmZpokzF",
    "include_sensitive_data": true
  }
]
```

These are example values only. Replace them with the application token and
user/group keys displayed in your Pushover account.

`include_sensitive_data` controls the content independently for each
destination:

- `false` sends only `New AW release found`.
- `true` uses the RSS item title as the notification title, the release story
  as the message, uploads the release's main cover as the image attachment, and
  adds a `View on Akiba-Web` link to the product page.

Pushover limits titles to 250 UTF-8 bytes, messages to 1,024 UTF-8 bytes, and
attachments to 5 MiB. Longer RSS content is safely truncated to those mandatory
limits. If Akiba-Web provides no story, the message says `Story unavailable`.

To disable Pushover, use an empty list:

```json
"pushover_destinations": []
```

After at least one link for a new release is successfully queued in JD, every
configured destination receives this exact message:

```text
New AW release found
```

Successful deliveries are recorded in `state_file`. If one destination fails,
only that destination is retried on a later pipeline run; successful
destinations are not sent the same release again. Do not manually remove the
notification entries from the state file unless you intentionally want to
allow them to be sent again.

Successful JD additions are tracked by exact link, while release IDs record
completion and notification history. JDownloader's own LinkCollector duplicate
manager remains authoritative for detecting equivalent or changed URLs already
in its download list. Pipeline runs are serialized, so simultaneous scheduler
and HTTP refresh requests cannot overwrite each other's state.

When MyJDownloader explicitly reports a link as duplicate or already present,
the scraper records that URL as successful in `found_links`. It will not retry
the duplicate on later runs, and the log states that it was marked successful.

Values in `queued_releases` are ISO 8601 timestamps. When upgrading a legacy
state file that only tracked links, its file modification time is used as the
best available approximate timestamp for migrated releases.

### Other settings

- `myjd_email` and `myjd_password`: MyJDownloader account credentials.
- `akiba_base` and `akiba_releases_path`: Akiba-Web host and release listing.
- `forum_base` and `forum_thread`: forum host and thread containing download
  links.
- `forum_pages`: number of latest thread pages to inspect.
- `port`: local HTTP port used by the Flask server.
- `schedule_interval`: seconds between automatic pipeline runs.
- `rss_file`: generated RSS output path.
- `state_file`: persistent JD-link and Pushover-delivery state.
- `log_file`: rotating application log path.

Keep `config.json` private because it contains account and API credentials.
The server refuses to start when this file contains invalid JSON, preventing a
configuration typo from silently disabling MyJDownloader or Pushover. You can
validate it before restarting with:

```sh
python -m json.tool config.json > /dev/null
```

## Run

Install dependencies and start the server:

```sh
python -m pip install -r requirements.txt
python akiba_rss_server.py
```

The RSS feed is available at `/giga/feed`, and service status at `/health`.
