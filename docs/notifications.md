# Notifications

Pharos sends a notification when a confirmed status change happens, never for a single failed check. Messages are written in the language set by `status_page.language` (English or Traditional Chinese) and link to the monitor in the dashboard when `server.base_url` is set.

## Events

| Event | When | Default |
|---|---|---|
| `down` | An outage is confirmed (`confirm.down` failures in a row). The message includes the cause and when it began. | on |
| `up` | A down monitor recovers. The message includes how long it was down. | on |
| `reminder` | Every `remind_every` while an outage continues. | on (when `remind_every` is set) |
| `cert` | A TLS certificate expires within `cert_expiry_warn`. At most once a day. | on |
| `degraded` | Responses became slower than `expect.max_latency`, or went back to normal. | off |

A notifier receives every event except `degraded` unless it lists `events`. Nothing is sent during maintenance windows or for paused monitors. If a maintenance window begins while an incident is open, the incident is closed and an `up` event with `"status": "maintenance"` tells whoever was alerted why.

Events for the same monitor and notifier are delivered in order: a recovery message never overtakes a down alert that is still being retried.

## Alerts that happen together

When many monitors fail at once, for example because a network link went down, one message per monitor would flood the channel and run into the channel's rate limit (a Discord webhook takes 5 messages per 2 seconds and 30 per minute), delaying the alerts that matter. So Slack, Discord, Telegram, ntfy and email notifiers send **at most one message per `group_interval`** (default `10s`):

- An alert after a quiet interval is sent **at once**; a single outage is never delayed.
- Alerts that follow within the interval wait for it to end and go out together:

  ```
  Monitors: 3 down, 1 recovered
  • DOWN: Public API · HTTP 503 Service Unavailable
  • DOWN: Sign-in · timed out after 10s
  • DOWN: Search · connection refused
  • RECOVERED: CDN · down for 2 minutes
  ```

  A grouped message lists up to 20 alerts, then "…and 12 more", and links to the dashboard overview.
- Alerts that arrive while a message is being retried join the next one, so a channel that was unreachable gets one catch-up message.

Set `group_interval: 0s` on a notifier to send every alert on its own; they still go out one at a time, so a rate limit is met by one refused request rather than a crowd of retries. Webhooks never group and are sent in parallel: their payload describes one event, and the receiving program can batch as it likes. Every alert appears in the delivery log as *pending* as soon as it is queued.

With 1,000 monitors of which 300 became unreachable at once and a channel enforcing Discord's limits, all 300 alerts arrived in two messages, the first as soon as the first outage was confirmed. Pharos 0.2.0, which sent one message per alert, got 55 of them through in five minutes ([measured](performance.md#during-an-outage)).

## Delivery and retries

Deliveries run in the background and never slow down checks. A failed delivery is retried after 5 s, 30 s, 2 min and 10 min. When a service answers 429 Too Many Requests, Pharos waits as long as it asks (`Retry-After`, or `retry_after` in Discord's and Telegram's replies) and tries again without counting it as a failure. Other HTTP 4xx responses (except 408) mean the request itself is wrong, such as a deleted webhook, so they are not retried. Every attempt appears in the dashboard's *Delivery log*. Errors never contain webhook URLs or bot tokens, which usually embed secrets.

Test a notifier with `pharos notify-test -c pharos.yaml <name>` or the *Send test* button in the dashboard.

## Channels

### Webhook

```yaml
- name: automation
  type: webhook
  url: https://hooks.example.com/pharos
  headers:
    Authorization: Bearer ${HOOK_TOKEN}
  secret: ${WEBHOOK_SECRET}
```

Pharos POSTs this JSON document. Its shape is part of Pharos's stable interface.

```json
{
  "event": "down",
  "at": "2026-09-29T06:30:00Z",
  "status": "down",
  "previous_status": "up",
  "monitor": { "id": "api", "name": "Public API", "type": "http", "target": "https://api.example.com/health" },
  "title": "DOWN: Public API",
  "text": "Public API is down.\nCause: HTTP 503 Service Unavailable\nSince: 2026-09-29 14:30 UTC+8\nTarget: https://api.example.com/health",
  "message": "HTTP 503 Service Unavailable",
  "incident": { "id": 7, "monitor_id": "api", "started_at": "2026-09-29T06:30:00Z", "cause": "HTTP 503 Service Unavailable" },
  "url": "https://status.example.com/admin/monitors/api"
}
```

`up` events add `duration_seconds` (the outage length) and a closed incident with `ended_at`; `cert` events add `cert_expiry`. The `X-Pharos-Event` header repeats the event name.

With `secret` set, the request carries `X-Pharos-Signature: sha256=<hex>`, the HMAC-SHA256 of the raw body. Verify it before trusting the payload:

```go
mac := hmac.New(sha256.New, []byte(secret))
mac.Write(body)
expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Pharos-Signature"))) {
	http.Error(w, "bad signature", http.StatusUnauthorized)
	return
}
```

```python
expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
if not hmac.compare_digest(expected, request.headers["X-Pharos-Signature"]):
    abort(401)
```

### Slack

Create an [incoming webhook](https://api.slack.com/messaging/webhooks) and use its URL.

```yaml
- name: team
  type: slack
  url: ${SLACK_WEBHOOK_URL}
```

### Discord

Channel settings → Integrations → Webhooks → New Webhook → Copy URL. Messages are embeds colored by severity; mentions are disabled.

```yaml
- name: chat
  type: discord
  url: ${DISCORD_WEBHOOK_URL}
```

### Telegram

Create a bot with [@BotFather](https://t.me/BotFather), add it to your chat, and find the chat id (for example by sending a message and opening `https://api.telegram.org/bot<token>/getUpdates`).

```yaml
- name: phone
  type: telegram
  token: ${TELEGRAM_BOT_TOKEN}
  chat_id: "-1001234567890"
```

### ntfy

Push notifications to phones and desktops through [ntfy](https://ntfy.sh), hosted or self-hosted. Outages are sent at the highest priority. Use a hard-to-guess topic name on public servers.

```yaml
- name: pager
  type: ntfy
  url: https://ntfy.sh/acme-alerts-7f3k2
  token: ${NTFY_TOKEN}        # for protected topics
  events: [down, up, reminder]
```

### Email

```yaml
- name: mail
  type: email
  from: Pharos <pharos@example.com>
  to: [ops@example.com, "Alex <alex@example.com>"]
  smtp:
    host: smtp.example.com
    port: 587                 # default: 587 for starttls, 465 for tls, 25 for none
    username: pharos
    password: ${SMTP_PASSWORD}
    security: starttls        # starttls (default), tls, or none
```

With `starttls`, Pharos refuses to send if the server does not offer STARTTLS rather than silently falling back to plain text. Messages are UTF-8 plain text.
