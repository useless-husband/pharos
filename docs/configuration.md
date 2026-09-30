# Configuration reference

Pharos reads one YAML file, by default `$PHAROS_CONFIG` or `./pharos.yaml`. Run `pharos init` to write a commented example and `pharos validate -c pharos.yaml` to check a file: every problem is reported at once, with its line number.

```text
$ pharos validate -c pharos.yaml
pharos.yaml: 3 problem(s)
  line 9: unknown setting "intervall" in monitor
  line 14: monitor "api": timeout must be positive and not longer than the interval (30s)
  line 22: status page group "Core" refers to unknown monitor "web"
```

- **Durations** accept `ms`, `s`, `m`, `h`, `d` and `w`, and combinations: `30s`, `1h30m`, `7d`.
- **Environment variables** can appear in any value: `${NAME}`, or `${NAME:-fallback}` when the variable may be unset. A missing variable without a fallback is an error. Write `$$` for a literal `$`.
- **Reloading**: send `SIGHUP` or press *Reload configuration* in the dashboard. An invalid file is rejected and the running configuration stays active. Monitors whose settings did not change keep running undisturbed; changed monitors restart with their state; removed monitors stop and their history is kept. `server.listen` and `storage.path` take effect after a restart.

Contents: [server](#server) · [storage](#storage) · [status_page](#status_page) · [defaults](#defaults) · [monitors](#monitors) · [notifiers](#notifiers) · [maintenance](#maintenance)

## server

| Setting | Default | Description |
|---|---|---|
| `listen` | `:8080` | Address of the HTTP server. Use `127.0.0.1:8080` behind a reverse proxy. |
| `base_url` | – | Public URL of this instance, e.g. `https://status.example.com`. Used for links in notifications and the push URLs shown in the dashboard. |
| `trust_proxy` | `false` | Honor `X-Forwarded-For`, `X-Forwarded-Proto` and `X-Forwarded-Host` from one reverse proxy you control. The client address is the **right-most** `X-Forwarded-For` entry, the one your proxy appended; entries to its left are client-supplied and ignored. |
| `admin.username` | `admin` | Dashboard user. |
| `admin.password_hash` | – | bcrypt hash from `pharos hash-password`. **Without it the dashboard only answers requests made directly from this machine to `localhost`**: requests that passed through a proxy, or that name another host (DNS rebinding), are refused. Changing the hash signs everyone out. |
| `metrics.enabled` | `true` | Serve Prometheus metrics at `/metrics`. |
| `metrics.token` | – | Require `Authorization: Bearer <token>` on `/metrics`. **Without a token, `/metrics` answers local clients only**, because metric labels name every monitor, private ones included. |

## storage

| Setting | Default | Description |
|---|---|---|
| `path` | `$PHAROS_STORAGE_PATH`, else `pharos.db` next to the config file | SQLite database. Relative paths are resolved against the config file's directory. |
| `retention` | `30d` | How long individual check results are kept (minimum `1d`). Status history, incidents and hourly latency aggregates are kept for 400 days regardless. |

## status_page

| Setting | Default | Description |
|---|---|---|
| `enabled` | `true` | Serve the public page at `/`. When `false`, `/` redirects to the dashboard and the public API is off. |
| `title` | `Service Status` | Page and browser title. |
| `description` | – | One paragraph under the title. |
| `language` | `en` | `en` or `zh-TW`, for the status page, the dashboard and notifications. |
| `timezone` | host time zone | IANA name such as `Asia/Taipei`. Daily history bars and all displayed times use it. |
| `history_days` | `90` | Days of history bars (7–365). |
| `incident_days` | `14` | Days of past incidents listed. |
| `show_causes` | `false` | Publish failure messages (`connection refused`, `HTTP 503`…). Off by default because they can reveal internal names. |
| `groups` | all monitors | `- name: …` and `monitors: [ids]`. **Monitors not listed in any group are private**: they appear only in the dashboard. With no groups, every monitor is public. |
| `links` | – | `- label: …` and `url: …`, shown in the page header. |

## defaults

Every monitor inherits these unless it sets its own.

| Setting | Default | Description |
|---|---|---|
| `interval` | `1m` | Time between checks. |
| `timeout` | `10s` | Per-check timeout; capped at the interval. |
| `retry_interval` | `15s` | Used instead of `interval` while a failure is being confirmed and while a monitor is down, so outages are confirmed and recoveries noticed quickly. |
| `confirm.down` | `3` | Consecutive failed checks before a monitor is down (1–20). |
| `confirm.up` | `2` | Consecutive successful checks before a down monitor recovers (1–20). |
| `notify` | – | Notifier names used by monitors that do not list their own. |
| `remind_every` | off | Repeat the down alert while an incident stays open, e.g. `1h`. |
| `cert_expiry_warn` | `14d` | Raise a `cert` event when a TLS certificate expires within this window (at most once a day). |

### How a status is decided

A single failed check is noise. With `confirm.down` = N and `confirm.up` = M:

- **Down**: N failed checks with no successful check between them. Slow checks in between neither count nor reset the streak, so a service that alternates between failing and slow is still reported down. The outage is **dated from the first of those failures**, not from the moment it was confirmed, so reported downtime is accurate.
- **Degraded**: N consecutive slow-or-failed checks from up, while there are not yet enough failures to call it down.
- **Recovery**: M consecutive checks that did not fail. The new status is the latest result (up or degraded), dated from the first of those checks.
- **Degraded → up**: M consecutive normal checks.

The first healthy result after start-up is accepted immediately.

Availability is **time-weighted**: the share of counted time a monitor was up or degraded. Maintenance, paused and unknown time is excluded from both sides of the ratio, so planned work and gaps in monitoring never lower the number. If Pharos itself was stopped, the time it could not observe is recorded as unknown.

## monitors

Common settings:

| Setting | Description |
|---|---|
| `id` | Required. Lowercase letters, digits, `.`, `_`, `-`. Used in URLs and the database; renaming an id starts a new history. |
| `name` | Display name. Defaults to the id. |
| `type` | `http`, `tcp`, `tls`, `dns`, `ping` or `push`. |
| `description` | Shown under the name on the status page. |
| `interval`, `timeout`, `retry_interval`, `confirm`, `remind_every`, `cert_expiry_warn` | Override the defaults. |
| `notify` | Notifier names for this monitor. Omit to use `defaults.notify`; `notify: []` sends nothing. |
| `expect.max_latency` | Checks slower than this are **degraded** rather than up. Must be shorter than the timeout. |

### Trying a monitor

`pharos check` probes monitors once from the machine it runs on and prints what a check would record: the status, the response time, the time spent in each phase and the reason for a failure. Nothing is stored and no notification is sent, so it is safe to run next to a live instance.

```console
$ pharos check -c pharos.yaml api db
MONITOR  STATUS    TIME    TARGET
api      up        142ms   https://api.example.com/health
                           dns 12ms · connect 20ms · tls 45ms · first byte 60ms
                           certificate valid until 2026-12-01 (61 days)
db       down      5.0s    db.internal:5432
                           connection timed out after 5s

2 monitors checked: 1 up, 1 down.
```

Without monitor ids it checks every monitor except push monitors. `-json` prints the results for scripts. It exits with 1 when a check is down and 2 when the configuration is invalid or an id is unknown. It is a single probe: the confirmation thresholds and maintenance windows that decide a monitor's status do not apply.

In a container, run it inside the container, where the network is the one Pharos uses: `docker exec pharos pharos check api`.

### http

```yaml
- id: api
  type: http
  url: https://api.example.com/health
  method: GET                   # GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS
  headers:
    Authorization: Bearer ${API_TOKEN}
    Host: internal.example.com  # overrides the Host header
  body: '{"ping":true}'
  follow_redirects: true        # up to 10
  insecure_skip_verify: false   # certificate expiry is still reported
  ip_version: "4"               # force IPv4 or "6"
  expect:
    status: [200, "3xx", "500-503"]   # default: 200-399
    body_contains: '"status":"ok"'
    body_not_contains: maintenance
    body_regex: 'version":\s*"2\.'
    headers:
      Content-Type: application/json  # the header must contain this value
    json:
      - { path: status, equals: ok }
      - { path: checks.database, equals: up }
      - { path: "items[0].id", exists: true }
    max_latency: 1500ms
```

Every check uses a fresh connection, so the response time includes DNS, connect, TLS and the server's own time; the dashboard shows that breakdown for the last check. Up to 1 MiB of the body is read for assertions. JSON paths use dots and indexes (`data.items[0].state`); numbers compare numerically (`equals: 1` matches `1.0`).

### tcp

```yaml
- id: database
  type: tcp
  address: db.internal:5432
  send: "PING\r\n"          # optional
  expect:
    banner: "+PONG"         # the first bytes the server sends must contain this
```

### tls

```yaml
- id: certificate
  type: tls
  address: example.com      # port 443 is assumed
  server_name: example.com  # SNI, defaults to the host
  expect:
    min_days_valid: 7       # fail when the certificate expires sooner
```

HTTPS monitors also report certificate expiry, so a separate `tls` monitor is only needed for non-HTTP services or a hard failure threshold.

### dns

```yaml
- id: dns
  type: dns
  query: example.com
  record: A                 # A, AAAA, CNAME, MX, TXT, NS
  resolver: 1.1.1.1         # port 53 assumed; default: the system resolver
  expect:
    values: [93.184.215.14] # every listed value must be in the answer
```

Names are compared case-insensitively without the trailing dot; TXT values are compared exactly.

### ping

```yaml
- id: gateway
  type: ping
  address: 192.0.2.1
```

Three ICMP echo requests are sent; the check passes if any reply arrives, and partial loss is noted in the message. Pharos uses unprivileged ICMP sockets, which macOS allows by default and Linux allows when `net.ipv4.ping_group_range` includes the process's group. Otherwise it falls back to raw sockets, which need `CAP_NET_RAW` (the systemd unit and compose file grant it).

### push

A push monitor waits for a job to report in, instead of checking something itself.

```yaml
- id: backup
  type: push
  heartbeat: 24h            # how often the job runs
  grace: 1h                 # default: heartbeat / 10, between 30s and 1h
  token: ${BACKUP_TOKEN}    # optional, at least 16 characters
```

The job requests `<base_url>/api/v1/push/<token>` (GET, HEAD or POST) when it succeeds. Without `token`, Pharos derives a stable secret one; the dashboard shows the full URL. The monitor goes down when no heartbeat arrives within `heartbeat + grace`. A job can also report failure explicitly:

```sh
curl -fsS -m 10 --retry 3 https://status.example.com/api/v1/push/TOKEN                       # success
curl -fsS "https://status.example.com/api/v1/push/TOKEN?status=down&msg=disk%20full"          # failure
curl -fsS "https://status.example.com/api/v1/push/TOKEN?ms=73500"                             # with duration
```

## notifiers

```yaml
notifiers:
  - name: team
    type: slack
    url: ${SLACK_WEBHOOK_URL}
    events: [down, up]      # default: everything except degraded
```

| Type | Settings |
|---|---|
| `webhook` | `url`, optional `headers`, optional `secret` (HMAC-SHA256 signature) |
| `slack` | `url` (incoming webhook) |
| `discord` | `url` (channel webhook) |
| `telegram` | `token` (bot token), `chat_id` |
| `ntfy` | `url` (topic URL, e.g. `https://ntfy.sh/my-alerts`), optional `token` |
| `email` | `from`, `to` (list), `smtp.host`, `smtp.port`, `smtp.username`, `smtp.password`, `smtp.security` (`starttls`, `tls` or `none`) |

Events: `down`, `up`, `degraded` (slow, and back to normal), `reminder`, `cert`. See [notifications.md](notifications.md) for payloads and retry behavior. Send a test with `pharos notify-test -c pharos.yaml <name>` or from the dashboard.

## maintenance

During a maintenance window, checks still run and are stored, but failures raise no alerts and the time is excluded from availability. The status page announces windows starting within the next seven days.

```yaml
maintenance:
  # One-off
  - name: Database upgrade
    monitors: [database, api]   # empty: every monitor
    start: "2026-10-12 01:00"   # or RFC 3339
    end: "2026-10-12 03:00"
    timezone: Asia/Taipei       # default: status_page.timezone

  # Recurring
  - name: Weekly backup window
    monitors: [database]
    days: [sun]                 # mon..sun, or daily
    at: "03:00"
    duration: 1h
```

Recurring windows keep their wall-clock time across daylight-saving changes. An incident that is open when maintenance begins is closed at that moment.
