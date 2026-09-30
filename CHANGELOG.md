# Changelog

All notable changes to Pharos are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and Pharos uses
[semantic versioning](https://semver.org/).

## [Unreleased]

### Added
- `pharos check [monitor...]` probes monitors once from this machine and prints the status, response time, phase timing, failure reason and certificate expiry, or JSON with `-json`. Nothing is stored or sent. It exits 1 when a check is down.
- A reproducible load test (`tools/loadtest`) and [measured performance](docs/performance.md) for 100 to 1,000 monitors.
- CI now runs the container image: demo mode, a real instance on a volume, HTTPS checks inside the image, the health check, and a restart on the same volume.

### Fixed
- **Outages of many monitors at once were detected late or not at all.** At most 32 checks ran at the same time, so when many targets stopped answering, their timeouts filled every slot and all other checks waited. With 1,000 monitors of which 300 became unreachable, none was confirmed down within three minutes; now all are, within 70 seconds with the default settings. Each monitor still runs one check at a time, and there is no longer a global limit.
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

[Unreleased]: https://github.com/useless-husband/pharos/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/useless-husband/pharos/releases/tag/v0.1.0
