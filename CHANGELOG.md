# Changelog

All notable changes to Pharos are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and Pharos uses
[semantic versioning](https://semver.org/).

## [Unreleased]

### Added
- **Alerts that happen together are sent together.** Slack, Discord, Telegram, ntfy and email notifiers send at most one message per `group_interval` (default 10 s): the first alert after a quiet interval goes out at once, and the alerts that follow are combined into one message ("Monitors: 23 down, 1 recovered") with a line per alert. When 300 of 1,000 monitors failed at once, with a channel enforcing Discord's rate limits, all 300 alerts arrived in two messages; before, 55 arrived within five minutes and the channel refused 1,065 requests. `group_interval: 0s` restores one message per alert; webhooks never group.
- The dashboard's notifier list shows each notifier's grouping.
- `tools/loadtest` options: `-tls` for HTTPS targets, `-chat` for a channel with Discord's rate limits, `-group`; the target now runs in its own process so its CPU is not counted as Pharos's.

### Fixed
- Rate limits are respected: on HTTP 429, a delivery waits as long as the service asks (`Retry-After`, or `retry_after` in Discord's and Telegram's replies) and does not use up a retry. Chat, push and email notifiers send one message at a time, so a limit is met by one refused request: with grouping off, 300 alerts drew 24 refusals in five minutes, where Pharos 0.2.0 drew 1,065.
- Alerts appear in the delivery log as pending as soon as they are queued, so alerts waiting their turn are visible and marked failed if Pharos stops before sending them.
- A reader holding a database snapshot, such as a backup tool or a SQLite shell, could hold up every check result and the status page for up to 10 seconds at the hourly checkpoint, and the incomplete checkpoint went unnoticed. Checkpoints no longer wait for readers, the write-ahead log is kept small by `journal_size_limit`, and check results are stored without holding the monitor's state lock.
- `pharos check` exits with 2 when the configuration file cannot be read (it exited 1, as for a down check) and with 130 when interrupted, without printing checks that were cut short.
- The dashboard's live refresh: a slow or failed request no longer pauses updates for twenty times its duration. Only successful refreshes set the pace, the pause is at most 30 seconds, a request is abandoned after 20 seconds, and an event that arrives during a refresh is no longer dropped.

## [0.2.0] - 2026-10-01

### Added
- `pharos check [monitor...]` probes monitors once from this machine and prints the status, response time, phase timing, failure reason and certificate expiry, or JSON with `-json`. Nothing is stored or sent. It exits 1 when a check is down.
- A reproducible load test (`tools/loadtest`) and [measured performance](docs/performance.md) for 100 to 1,000 monitors.
- CI now runs the container image: demo mode, a real instance on a volume, HTTPS checks inside the image, the health check, and a restart on the same volume.

### Fixed
- **Outages of many monitors at once were detected late or not at all.** At most 32 checks ran at the same time, so when many targets stopped answering, their timeouts filled every slot and all other checks waited. With 1,000 monitors of which 300 became unreachable, none was confirmed down within three minutes; now they are confirmed as quickly as a single outage would be (in that test, all within 70 seconds). Each monitor still runs one check at a time, and there is no longer a global limit.
- The dashboard's live refresh no longer reloads a monitor's page when other monitors are checked, and slows down on large dashboards so an open tab costs the server at most about 5% of its time.
- The dashboard of `pharos demo` refused visitors when it ran in Docker, because requests through a published port do not come from loopback. Demo mode, whose data is simulated, now opens the dashboard to every visitor; real instances are unchanged.
- The start-up jitter that spreads checks after a restart was capped at about 4.3 seconds; it now spans up to 10 seconds, as intended.
- The write-ahead log is checkpointed at start-up and after pruning, so the `-wal` file no longer keeps the size of deleted checks.

### Changed
- The dashboard overview reads every monitor's response times in one pass instead of several queries per monitor: with 1,000 monitors it renders in 61 ms instead of 150 ms under the same conditions.
- Times in notifications and the dashboard name the UTC offset (`2026-09-29 14:30 UTC+8`) instead of a zone abbreviation, which is ambiguous (`CST`) or missing for many zones.
- `pharos check` was an undocumented alias of `pharos validate`; use `validate` to only check the configuration.

## [0.1.0] - 2026-09-29

First public release.

### Monitoring
- HTTP(S), TCP, TLS certificate, DNS, ICMP ping and push (heartbeat) monitors.
- HTTP assertions on status codes, headers, body text, regular expressions and JSON values; per-phase timing (DNS, connect, TLS, server time).
- Response-time thresholds that mark a service as degraded.
- Confirmation thresholds for outages and recoveries, with a faster retry interval while confirming; outages are dated from their first failure.
- One-off and recurring maintenance windows that suppress alerts and are excluded from availability.
- Pause and resume from the dashboard.

### Status page and dashboard
- Public status page with 90-day history, incident history, maintenance announcements and groups; private monitors and failure details stay off it by default.
- Dashboard with live updates, response-time charts, check log, heartbeat URLs, notifier tests and configuration reload.
- English and Traditional Chinese; light and dark themes.
- Static export of the status page.

### Alerts and integrations
- Slack, Discord, Telegram, ntfy, email and HMAC-signed webhooks, with retries and a delivery log.
- Reminders during long outages and certificate expiry warnings.
- JSON API, README badges and Prometheus metrics.

### Operations
- Single binary with an embedded SQLite database; Docker image for amd64, arm64 and armv7; hardened systemd unit.
- Configuration validation with line numbers, environment variable expansion and hot reload.
- A demo mode with 90 days of simulated history.

[Unreleased]: https://github.com/useless-husband/pharos/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/useless-husband/pharos/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/useless-husband/pharos/releases/tag/v0.1.0
