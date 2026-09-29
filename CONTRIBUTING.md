# Contributing to Pharos

Thanks for your interest. Bug reports, documentation fixes and focused pull requests are all welcome.

## Before you start

- For anything larger than a small fix, open an issue first to agree on the approach.
- Security problems: do not open a public issue; see [SECURITY.md](SECURITY.md).

## Development setup

You need Go 1.26 or newer. No other tools or services are required.

```sh
git clone https://github.com/useless-husband/pharos
cd pharos
make test        # unit and integration tests
make race        # the same with the race detector (what CI runs)
make lint        # gofmt, go vet, staticcheck
make demo        # run the dashboard with simulated data at http://127.0.0.1:8080
```

[docs/architecture.md](docs/architecture.md) explains how the packages fit together.

## Guidelines

- **Tests come with changes.** Engine logic is tested with the fake clock in `internal/clock`; network code against local servers. Tests must not reach the internet (the ping test is opt-in with `PHAROS_TEST_PING=1`).
- **Keep dependencies few.** A new dependency needs a good reason.
- **User-facing text** goes through `internal/i18n` in both English and Traditional Chinese; `TestCatalogsAreComplete` checks that every key exists in both.
- **Configuration changes** need validation with a clear error message, a line in [docs/configuration.md](docs/configuration.md), and a mention in [CHANGELOG.md](CHANGELOG.md).
- **Database changes** are new migrations appended to `internal/store/migrate.go`; never edit a released migration.
- **The web UI** works without JavaScript and follows the Content Security Policy: no inline scripts or styles.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:` …).

## Releasing (maintainers)

Update `CHANGELOG.md`, then tag: `git tag -a v0.2.0 -m v0.2.0 && git push origin v0.2.0`. The release workflow builds the binaries and the container image.
