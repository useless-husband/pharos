# Performance

How much Pharos needs, measured rather than estimated. Every number on this page comes from `tools/loadtest`, which you can run yourself.

## Summary

With 1,000 HTTP monitors checked every 30 seconds and 30 days of history, Pharos used under 4% of one CPU core and 72 MB of memory, and every page but the dashboard overview answered within 50 ms. For tens to a few hundred monitors, resource use is negligible. What grows with scale is disk space, and the dashboard overview, which shows every monitor on one page.

When 300 of those 1,000 targets stopped answering at once, every one of them was confirmed down within 70 seconds, the time the default settings imply. With the limit of 32 concurrent checks that Pharos 0.1.0 had, none of them was confirmed within three minutes; see [During an outage](#during-an-outage).

## Method

`tools/loadtest` runs a complete instance in one process against a local HTTP target that answers in 2 to 22 ms:

1. It writes a configuration with *N* HTTP monitors (a fifth of them on the status page) and seeds a database as a long-running instance would have it: 90 days of status history with an outage per monitor about every two weeks, 30 days of check results, and hourly aggregates. The last 25 hours of checks are seeded at the real check interval, because that is what the 24-hour charts read; older checks, which pages only read through hourly aggregates, are seeded every 10 minutes to keep seeding fast. Checks are inserted in time order across monitors, as a running instance writes them, so the database's layout on disk is realistic.
2. It starts the engine and the web server with their default settings and lets them check live for three minutes, sampling memory and goroutines every second and measuring the process's CPU time.
3. While checks continue, it requests each page 20 times and reports the median and 95th percentile.

Checks are counted by their timestamps, so the hourly prune that runs during the test does not affect the count. With `-hang 0.3`, 30% of the targets accept connections but never answer, as hosts behind a failed network link do; the tool then also reports when each of them was confirmed down.

```sh
go run ./tools/loadtest -monitors 500 -interval 30s -history 30d -duration 3m
```

The results below were measured on an Apple M5 (10 cores, 16 GB) with macOS 27 and Go 1.27, with nothing else under load. Each run used one database on the local SSD.

## Results

| | 100 monitors, every 60 s | 500 monitors, every 30 s | 1,000 monitors, every 30 s |
|---|---|---|---|
| Checks per second | 1.7 | 16.7 | 33.3 |
| Database after seeding | 50 MB | 314 MB | 629 MB |
| CPU, share of one core (two runs) | 0.1–0.4% | 0.8–2.1% | 1.2–3.4% |
| Resident memory, peak | 42 MB | 55 MB | 72 MB |
| Go heap in use, peak | 10 MB | 16 MB | 24 MB |
| Goroutines, peak | 117 | 522 | 1,026 |

Page response times, median and 95th percentile of 20 requests, while checks run:

| Page | 100 monitors | 500 monitors | 1,000 monitors |
|---|---|---|---|
| Status page (a fifth of the monitors) | 3.3 / 7.3 ms | 13.1 / 14.0 ms | 25.4 / 26.4 ms |
| Status JSON | 0.4 / 0.7 ms | 1.9 / 4.6 ms | 3.4 / 4.2 ms |
| Dashboard overview (every monitor) | 7.4 / 7.9 ms | 54.1 / 55.9 ms | 110.2 / 113.5 ms |
| Monitor page, 24-hour chart | 3.2 / 3.9 ms | 4.9 / 6.0 ms | 5.1 / 5.6 ms |
| Monitor page, 30-day chart | 1.3 / 1.7 ms | 1.2 / 2.0 ms | 1.3 / 2.5 ms |
| Prometheus metrics | 4.9 / 5.5 ms | 23.0 / 24.9 ms | 45.1 / 46.2 ms |

Resident memory is the whole process, SQLite's page cache included. CPU time varied by up to three times between otherwise identical runs, most likely because macOS moves light work between performance and efficiency cores, so both runs are shown. Page times varied by about 10%.

What the numbers mean:

- **CPU** grows with the number of checks per second and stays in the low single digits of one core.
- **Memory** grows by about 15 MB per 500 monitors.
- **The status page and the metrics** grow linearly with the monitors they list: availability over several windows is computed from status history for each one.
- **The dashboard overview** grows linearly too, and is the slowest page at scale. Most of its time goes to the last hour's checks, which are not aggregated yet: between 15 and 75 minutes' worth, depending on the time of the hour, so expect its time to vary with it. Reading those checks for all monitors in one pass, instead of one query per monitor as before, cut it from 150 ms to 61 ms at 1,000 monitors in back-to-back runs under the same conditions. A covering index would halve it again at the cost of about 20% more disk per check; disk space mattered more. The overview's live updates wait at least 20 times as long as the last refresh took, so a dashboard left open costs the server at most about 5% of its time.
- **Monitor pages and JSON endpoints** stay within a few milliseconds at every scale: they read one monitor through indexes, and charts beyond 24 hours read hourly aggregates.

## During an outage

The case that matters most is when many targets fail at once, for example when a network link goes down: each check then waits for its full timeout. The same 1,000 monitors every 30 seconds, with 300 of the targets accepting connections but never answering, and default settings (10-second timeout, `retry_interval` of 15 seconds, three failures to confirm):

| | At most 32 checks at once, as in 0.1.0 | This version |
|---|---|---|
| Checks per second (35.3 without queueing) | 10.0 | 35.2 |
| Unreachable monitors confirmed down within 3 minutes | 0 of 300 | 300 of 300 |
| Time to confirm, median and slowest | – | 65 s, 70 s |

The first column is this version run with `-concurrency 32`, the limit Pharos 0.1.0 had. The timeouts filled every slot, and every other check, including those of healthy monitors, waited behind them. Pharos now runs each monitor's checks on its own schedule with no global limit. 70 seconds is what the settings imply without any waiting: up to 10 seconds of start-up jitter, then three checks that each wait 10 seconds for the timeout, 15 seconds apart. Holding 300 unanswered connections raised resident memory to 103 MB.

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

- Pharos is one process with one SQLite database. It is designed for the size of team that runs its own status page, not for tens of thousands of monitors; at 1,000 monitors the dashboard overview takes about a tenth of a second.
- All writes go through one connection. At 1,000 monitors every 30 seconds that is 33 small writes per second, a small fraction of what SQLite handles on an SSD; on an SD card or network storage, measure before relying on that margin.
- The target in these tests answers locally. Real targets add network latency, which lengthens each check but costs almost no CPU while waiting.
- These numbers come from a fast desktop processor. Smaller machines were not measured; expect CPU and page times to scale with single-core speed.
