# HTTP API

All responses are JSON unless noted. Times are RFC 3339 in UTC. Availability values are ratios between 0 and 1, or `null` when there is no data for the window.

## Public endpoints

These follow the status page: they are off when `status_page.enabled` is `false`, and they only include public monitors (those listed in a status page group). They send `Access-Control-Allow-Origin: *`, so a website can read them directly.

### `GET /api/v1/status`

The machine-readable status page.

```json
{
  "title": "Acme Cloud Status",
  "status": "operational",
  "updated_at": "2026-09-29T04:49:00Z",
  "groups": [
    {
      "name": "Website and API",
      "monitors": [
        {
          "id": "api",
          "name": "Public API",
          "status": "up",
          "since": "2026-09-29T04:48:50Z",
          "uptime": { "24h": 1, "7d": 0.9977, "30d": 0.9991, "90d": 0.9994 }
        }
      ]
    }
  ],
  "incidents": [
    { "id": 12, "monitor_id": "api", "monitor": "Public API", "started_at": "2026-09-26T06:12:00Z",
      "ended_at": "2026-09-26T06:35:00Z", "duration_seconds": 1380 }
  ],
  "maintenance": [
    { "name": "Mail relay migration", "start": "2026-09-30T17:00:00Z", "end": "2026-09-30T18:30:00Z", "monitors": ["mail"] }
  ]
}
```

`status` is one of `operational`, `degraded`, `partial`, `major`, `maintenance`, `unknown`. Monitor `status` is one of `up`, `degraded`, `down`, `maintenance`, `paused`, `unknown`. Incident `cause` is included only when `status_page.show_causes` is on.

### `GET /api/v1/incidents?days=30`

Incidents of public monitors that were open during the last `days` (default `status_page.incident_days`, at most 400), newest first.

### `GET|HEAD|POST /api/v1/push/{token}`

Records a heartbeat for a push monitor. Optional parameters (query string, or form body for POST):

| Parameter | Meaning |
|---|---|
| `status` | `up` (default) or `down` |
| `msg` | A message shown with the check, e.g. the failure reason (500 characters max) |
| `ms` | How long the job took, in milliseconds |

Returns `200 {"ok": true, "monitor": "backup"}`, or `404` for an unknown token.

### `GET /badge/{id}/status.svg` and `GET /badge/{id}/uptime.svg?window=30d`

SVG badges for READMEs; `window` is `24h`, `7d`, `30d` (default) or `90d`. Public monitors only.

```markdown
![API status](https://status.example.com/badge/api/status.svg)
![API uptime](https://status.example.com/badge/api/uptime.svg?window=90d)
```

### `GET /healthz`

`200 {"status": "ok", "version": "…", "uptime_seconds": …}` while the process is serving.

### `GET /metrics`

Prometheus text format. Protected by `server.metrics.token` when set (`Authorization: Bearer …`).

| Metric | Labels | Meaning |
|---|---|---|
| `pharos_monitor_up` | monitor, name, type | 1 up or degraded, 0 down; absent while unknown, paused or in maintenance |
| `pharos_monitor_status` | monitor, name, type, status | 1 for the current status, 0 for the others |
| `pharos_check_duration_seconds` | monitor, name, type | Duration of the last check |
| `pharos_last_check_timestamp_seconds` | monitor, name, type | Time of the last check or heartbeat |
| `pharos_cert_expiry_timestamp_seconds` | monitor, name, type | Expiry of the monitored certificate |
| `pharos_availability_ratio` | monitor, name, type, window | Availability over 24h, 7d and 30d |
| `pharos_incident_open` | monitor, name, type | 1 while an incident is open |
| `pharos_build_info` | version | Always 1 |

Example alert rule:

```yaml
- alert: CertificateExpiresSoon
  expr: pharos_cert_expiry_timestamp_seconds - time() < 7 * 86400
```

## Dashboard endpoints

These need a signed-in session (or, without a configured password, a request from the same machine). Requests other than GET also need the CSRF token from the page's `<meta name="csrf-token">`, sent as `X-CSRF-Token`.

| Endpoint | Description |
|---|---|
| `GET /api/v1/admin/monitors` | Every monitor, including private ones, with last check, open incident and availability |
| `GET /api/v1/admin/monitors/{id}` | One monitor |
| `GET /api/v1/admin/monitors/{id}/checks?limit=100&before=<RFC 3339>&failures=1` | Stored check results, newest first |
| `GET /api/v1/admin/monitors/{id}/latency?range=24h` | Response-time series for `24h`, `7d`, `30d` or `90d` |
| `POST /admin/monitors/{id}/check` · `/pause` · `/resume` | Actions; send `Accept: application/json` for a JSON reply |
| `GET /admin/events` | Server-sent events: `check` and `status`, for live updates |
