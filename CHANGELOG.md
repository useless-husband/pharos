# Changelog

All notable changes to Pharos are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and Pharos uses
[semantic versioning](https://semver.org/).

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

[0.1.0]: https://github.com/useless-husband/pharos/releases/tag/v0.1.0
