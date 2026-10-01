# Performance

How much Pharos needs, measured rather than estimated. Every number on this page comes from `tools/loadtest`, which you can run yourself.

## Summary

With 1,000 HTTP monitors checked every 30 seconds and 30 days of history, Pharos used about 2% of one CPU core (4% when the targets were HTTPS) and 73 MB of memory, and its slowest page, the dashboard overview of every monitor, answered in 69 ms. For tens to a few hundred monitors, resource use is negligible. What grows with scale is disk space.

When 300 of those 1,000 targets stopped answering at once, all 300 were confirmed down as quickly as a single outage would be, and all 300 alerts reached a channel with Discord's rate limits in two messages, the first as soon as the outage was confirmed. See [During an outage](#during-an-outage).

## Method

`tools/loadtest` runs a complete instance in one process. The monitored targets run in a second process, so their CPU time is not counted as Pharos's, and answer in 2 to 22 ms, over HTTP or, with `-tls`, HTTPS.

1. It writes a configuration with *N* HTTP monitors (a fifth of them on the status page) and seeds a database as a long-running instance would have it: 90 days of status history with an outage per monitor about every two weeks, 30 days of check results, and hourly aggregates. The last 25 hours of checks are seeded at the real check interval, because that is what the 24-hour charts read; older checks, which pages only read through hourly aggregates, are seeded every 10 minutes to keep seeding fast. Checks are inserted in time order across monitors, as a running instance writes them, so the database's layout on disk is realistic.
2. It starts the engine and the web server with their default settings and lets them check live for three minutes, sampling memory and goroutines every second and measuring the process's CPU time.
3. While checks continue, it requests each page 20 times and reports the median and 95th percentile, and how many minutes of checks are not aggregated into hours yet, which the overview's time depends on.

Checks are counted by their timestamps, so the hourly prune that runs during the test does not affect the count. With `-hang 0.3`, 30% of the targets accept connections but never answer, as hosts behind a failed network link do, and the tool reports when each was confirmed down. With `-chat`, alerts go to a local channel that enforces Discord's webhook limits (5 messages per 2 seconds, 30 per minute) and answers 429 beyond them.

```sh
go run ./tools/loadtest -monitors 500 -interval 30s -history 30d -duration 3m
go run ./tools/loadtest -monitors 1000 -interval 30s -history 1d -duration 5m -hang 0.3 -chat
```

The results below were measured with Pharos 0.3.0 on an Apple M5 (10 cores, 16 GB) with macOS 27 and Go 1.27, with nothing else under load. Each run used one database on the local SSD.

## Results

| | 100 monitors, every 60 s | 500 monitors, every 30 s | 1,000 monitors, every 30 s | 1,000 HTTPS monitors, every 30 s |
|---|---|---|---|---|
| Checks per second | 1.7 | 16.7 | 33.3 | 33.3 |
| Database after seeding | 50 MB | 314 MB | 629 MB | 629 MB |
| CPU, share of one core | 0.4% | 1.1% | 2.0% | 4.4% |
| Resident memory, peak | 42 MB | 54 MB | 73 MB | 68 MB |
| Go heap in use, peak | 9 MB | 15 MB | 23 MB | 24 MB |
| Goroutines, peak | 109 | 513 | 1,026 | 1,036 |

Page response times over HTTP, median and 95th percentile of 20 requests, while checks run:

| Page | 100 monitors | 500 monitors | 1,000 monitors |
|---|---|---|---|
| Status page (a fifth of the monitors) | 3.1 / 5.1 ms | 12.8 / 14.1 ms | 25.4 / 28.3 ms |
| Status JSON | 0.4 / 0.7 ms | 1.6 / 2.2 ms | 3.6 / 4.0 ms |
| Dashboard overview (every monitor) | 5.0 / 5.2 ms | 33.2 / 34.4 ms | 68.9 / 72.1 ms |
| Monitor page, 24-hour chart | 2.6 / 3.0 ms | 6.0 / 6.7 ms | 5.3 / 5.7 ms |
| Monitor page, 30-day chart | 1.2 / 1.6 ms | 1.3 / 2.1 ms | 1.2 / 2.2 ms |
| Prometheus metrics | 4.5 / 5.1 ms | 27.4 / 32.0 ms | 43.7 / 45.9 ms |
| *Checks not aggregated yet* | *20 minutes* | *23 minutes* | *27 minutes* |

Resident memory is the whole process, SQLite's page cache included. CPU time varied by up to three times between otherwise identical runs, most likely because macOS moves light work between performance and efficiency cores: read it as an order of magnitude. Page times varied by about 10%.

What the numbers mean:

- **CPU** grows with the number of checks per second and stays in the low single digits of one core. An HTTPS check costs about twice as much as a plain HTTP one, because every check makes a fresh connection and so a TLS handshake (which is also why its timing breakdown is real). The local certificate is self-signed and its chain is not verified; verifying a real chain adds a little more.
- **Memory** grows by about 15 MB per 500 monitors.
- **The status page and the metrics** grow linearly with the monitors they list: availability over several windows is computed from status history for each one.
- **The dashboard overview** grows linearly too, and is the slowest page at scale. Most of its time goes to the checks of the last hour or so, which are not aggregated yet: between 15 and 75 minutes' worth, depending on the time of the hour, so its time grows with that window. Reading those checks for all monitors in one pass, rather than one query per monitor as Pharos 0.1.0 did, cut it from 150 ms to 61 ms at 1,000 monitors in back-to-back runs with 20 minutes of them. A covering index would halve it again at the cost of about 20% more disk per check; disk space mattered more. Between two live updates the overview waits 20 times as long as the last refresh took, at most 30 seconds, so a dashboard left open costs the server about 5% of its time while a refresh takes under 1.5 seconds (at 1,000 monitors it takes about 70 ms).
- **Monitor pages and JSON endpoints** stay within a few milliseconds at every scale: they read one monitor through indexes, and charts beyond 24 hours read hourly aggregates.

## During an outage

The case that matters most is when many targets fail at once, for example when a network link goes down: each check then waits for its full timeout, and every failure becomes an alert. The same 1,000 monitors every 30 seconds, with 300 of the targets accepting connections but never answering, and default settings (10-second timeout, `retry_interval` of 15 seconds, three failures to confirm):

| Detection | At most 32 checks at once, as in 0.1.0 | Pharos 0.3.0 |
|---|---|---|
| Checks per second (35.3 without queueing) | 10.0 | 35.4 |
| Unreachable monitors confirmed down within 3 minutes | 0 of 300 | 300 of 300 |
| Confirmed after, median and slowest | – | 65 s, 70 s |

The first column is Pharos run with `-concurrency 32`, the limit Pharos 0.1.0 had: the timeouts filled every slot, and every other check, including those of healthy monitors, waited behind them. Pharos now runs each monitor's checks on its own schedule with no global limit, so a mass outage is confirmed as quickly as a single one. In this test the targets failed before Pharos started, so each first failed check came within the start-up jitter (up to 10 seconds), followed by three checks that each waited 10 seconds for the timeout, 15 seconds apart: at most 70 seconds. For an outage that starts while Pharos runs, the first failed check comes up to one interval later, so expect at most the interval plus 60 seconds with these settings.

Then the alerts. Each monitor notifies a channel that enforces Discord's limits:

| Alerts | One message per alert (Pharos 0.2.0) | One message per alert, waiting as asked (`group_interval: 0s`) | Grouped (default) |
|---|---|---|---|
| Down alerts delivered within 5 minutes | 55 of 300 | 120 of 300 | 300 of 300 |
| Last of them delivered after | – | – | 70 s |
| Messages posted, and refused with 429 | 55, 1,065 | 120, 24 | 2, 0 |
| Alerts still waiting after 5 minutes | 245 | 180 | 0 |

Pharos 0.2.0 was measured with the same scenario before this change. It ignored the channel's `Retry-After` and retried on a fixed schedule, so most retries hit the limit again. Sending one message at a time and waiting as asked keeps refusals to one each time the limit is reached, but at 30 messages a minute 300 alerts still take ten minutes and flood the channel. Grouped, the first alert went out as soon as it was confirmed and the other 299 followed together in one message, 70 seconds after the start. Holding 300 unanswered connections raised resident memory to 87 MB.

## Disk space

Disk space is what grows with scale. Measured on a seeded database, a stored check takes **81 bytes**, table and both indexes included; the estimates below use 100 bytes to allow for failure messages and for index pages that are not full. Hourly aggregates add about 47 bytes per monitor and hour, kept for 400 days: about 0.45 MB per monitor at most.

```
check data ≈ monitors × (86,400 ÷ interval in seconds) × retention in days × 100 bytes
```

| Monitors | Interval | `storage.retention` | Checks stored | Check data | Hourly aggregates after 400 days |
|---|---|---|---|---|---|
| 20 | 60 s | 30 days | 0.9 million | 86 MB | 9 MB |
| 100 | 60 s | 30 days | 4.3 million | 430 MB | 45 MB |
| 100 | 60 s | 7 days | 1.0 million | 100 MB | 45 MB |
| 1,000 | 30 s | 30 days | 86 million | 8.6 GB | 450 MB |
| 1,000 | 30 s | 7 days | 20 million | 2.0 GB | 450 MB |

If that is more than you want to keep, lower `storage.retention`. Raw checks are only needed for the check log and for charts of the last day; availability, status history and longer charts do not depend on them. Pruning runs hourly and SQLite reuses the freed pages, so once the retention is reached the file only grows with the hourly aggregates. It shrinks only after a `VACUUM`.

## Limits

- Pharos is one process with one SQLite database. It is designed for the size of team that runs its own status page, not for tens of thousands of monitors; at 1,000 monitors the dashboard overview is the slowest page, at 69 ms with 27 minutes of checks not aggregated yet and more later in the hour.
- All writes go through one connection. At 1,000 monitors every 30 seconds that is 33 small writes per second, a small fraction of what SQLite handles on an SSD; on an SD card or network storage, measure before relying on that margin.
- The target in these tests answers locally. Real targets add network latency, which lengthens each check but costs almost no CPU while waiting.
- These numbers come from a fast desktop processor. Smaller machines were not measured; expect CPU and page times to scale with single-core speed.
