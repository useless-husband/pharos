# Security policy

## Reporting a vulnerability

Please report security issues privately through
[GitHub security advisories](https://github.com/useless-husband/pharos/security/advisories/new),
not as public issues. Include the version, the configuration involved (with secrets removed) and steps to reproduce.
You can expect an acknowledgement within a few days. Fixes are released as a new version and credited unless you prefer otherwise.

## Supported versions

Security fixes are made for the latest release.

## Security model

- **Public endpoints** (`/`, `/history/`, `/api/v1/status`, `/api/v1/incidents`, `/badge/`) expose only monitors listed in status page groups, and never failure messages unless `status_page.show_causes` is enabled.
- **The dashboard** requires a password when `server.admin.password_hash` is set. Without it, only direct loopback requests addressed to `localhost` are admitted: requests carrying proxy headers are never treated as local, and a local Host is required to defeat DNS rebinding. Changing the password hash invalidates all sessions.
- **Client addresses** behind a proxy come from the right-most `X-Forwarded-For` entry (with `server.trust_proxy`), so clients cannot choose the address used for rate limiting.
- **Metrics** name every monitor; without `server.metrics.token` they are served to local clients only.
- **Sessions** are HMAC-signed cookies (`HttpOnly`, `SameSite=Lax`, `Secure` over HTTPS). State-changing requests require a CSRF token and a same-origin `Origin` header when present. Sign-in attempts are rate-limited per client address.
- **Push tokens** are secrets: anyone with a monitor's URL can report heartbeats for it.
- **Monitors are defined by the operator.** Pharos makes whatever requests its configuration describes, so treat write access to the configuration file as administrative access.
- **Secrets** (webhook URLs, bot tokens, SMTP passwords) can be supplied through environment variables and are kept out of error messages.
- Every page is served with a strict Content Security Policy.
