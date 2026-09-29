# Deployment

Pharos is one static binary with an embedded web UI and an SQLite database. It needs no external services: each monitor is one goroutine that wakes up once per interval, and a bounded number of checks run at the same time.

## Docker

```sh
mkdir pharos && cd pharos
docker run --rm ghcr.io/useless-husband/pharos:latest init -o - > pharos.yaml   # an example to edit
docker run -d --name pharos --restart unless-stopped \
  -p 8080:8080 \
  -v "$PWD/pharos.yaml:/etc/pharos/pharos.yaml:ro" \
  -v pharos-data:/data \
  ghcr.io/useless-husband/pharos:latest
```

The image runs as a non-root user on a distroless base. It reads `/etc/pharos/pharos.yaml` and stores the database at `/data/pharos.db` (via `PHAROS_STORAGE_PATH`, used when `storage.path` is not set). It has a built-in health check. Add `--cap-add NET_RAW` if you use ping monitors.

A compose file is in [`deploy/docker-compose.yml`](../deploy/docker-compose.yml). Reload the configuration after editing it:

```sh
docker kill --signal HUP pharos
```

## Binary and systemd

Download the archive for your platform from the [releases page](https://github.com/useless-husband/pharos/releases), verify it against `checksums.txt`, and install:

```sh
sudo useradd --system --home /var/lib/pharos --shell /usr/sbin/nologin pharos
sudo install -m 0755 pharos /usr/local/bin/pharos
sudo install -d -m 0750 -o root -g pharos /etc/pharos
sudo install -m 0640 -o root -g pharos pharos.yaml /etc/pharos/pharos.yaml
sudo cp pharos.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now pharos
```

Set `storage.path: /var/lib/pharos/pharos.db` in the configuration. The unit in [`deploy/pharos.service`](../deploy/pharos.service) runs Pharos sandboxed (read-only system, private `/tmp`, no new privileges) and grants only `CAP_NET_RAW` for ping. `systemctl reload pharos` re-reads the configuration.

With Go 1.26 or newer you can also build from source: `go install github.com/useless-husband/pharos/cmd/pharos@latest`.

## Behind a reverse proxy

Serve Pharos over HTTPS through a reverse proxy, bind it to localhost, and tell it to trust the proxy's forwarded headers:

```yaml
server:
  listen: 127.0.0.1:8080
  base_url: https://status.example.com
  trust_proxy: true
```

Only enable `trust_proxy` when every request reaches Pharos through the proxy; otherwise a client could claim any address in `X-Forwarded-For`.

**Caddy** (handles certificates automatically):

```
status.example.com {
	reverse_proxy 127.0.0.1:8080
}
```

**nginx**: the dashboard's live updates use server-sent events, which must not be buffered.

```nginx
server {
    listen 443 ssl;
    server_name status.example.com;
    # ssl_certificate ...;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
    location /admin/events {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
        proxy_read_timeout 1h;
    }
}
```

To keep the dashboard off the internet entirely, proxy only `/`, `/history/`, `/assets/`, `/badge/` and `/api/v1/` (public endpoints and push), and reach `/admin` over a VPN or SSH tunnel.

## Securing the dashboard

Create a password hash and put it in the configuration:

```sh
pharos hash-password
```

```yaml
server:
  admin:
    username: admin
    password_hash: "$2a$12$..."
```

Without a hash the dashboard only answers requests from the same machine, which is convenient on a laptop and safe by default on a server. Sign-in is rate-limited per address, sessions are signed cookies (`HttpOnly`, `SameSite=Lax`, `Secure` over HTTPS) valid for seven days, and every change requires a CSRF token.

## Publishing a static status page

`pharos export -o site` writes the public status page, each public monitor's history page, `status.json` and the assets as static files with relative links. Upload them anywhere, for example from a cron job, if you prefer not to expose Pharos itself:

```sh
*/5 * * * *  pharos export -c /etc/pharos/pharos.yaml -o /var/www/status && rsync -a /var/www/status/ web:/srv/status/
```

Export reads the database without writing to it, so it can run next to the live instance.

## Backups

The database is a single SQLite file in WAL mode. Back it up while Pharos runs with SQLite's online backup:

```sh
sqlite3 /var/lib/pharos/pharos.db ".backup /backups/pharos-$(date +%F).db"
```

Copying the file alone while Pharos is writing can produce an inconsistent copy; copy it together with `pharos.db-wal`, or stop Pharos first.

## Upgrading

Replace the binary or image and restart. Database migrations run automatically at start-up; a newer database is never opened by an older version (Pharos refuses to start instead). Back up the database before upgrading across minor versions.

## Monitoring Pharos itself

Scrape `/metrics` with Prometheus, or point an external check at `/healthz`. A monitor cannot report that the machine running it went down, so watch Pharos from somewhere else, for example a free external uptime service checking `https://status.example.com/healthz`.
