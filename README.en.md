# napm

**English** | [中文](README.md)

napm manages several [new-api](https://github.com/QuantumNous/new-api) sites from one web page. It is for administrators who run more than one new-api. It collects user counts, groups, balances and usage from each site, changes the balance of chosen users or a whole group, and runs those operations on a schedule.

## Features

- One Go binary with the web page embedded. Data lives in a single SQLite file, with no CGO and no external database.
- Each administrator sees and operates only the platforms granted to them. A read-only account can look but not change.
- A balance can be added to, subtracted from, or set to a fixed value, and the amount can be entered in USD or raw quota.
- Scheduled jobs refresh statistics, collect usage or change balances, using a cron expression or an `@every` interval.
- An MCP endpoint lets an AI client manage platforms with an administrator's API token (`Authorization: Bearer`). The token has that administrator's permissions, and a successful write is written to the audit log.
- napm runs one job at a time per platform and spaces its requests, so it stays under new-api's rate limit.

## Status and limits

napm has not been verified against a real new-api site. Try it on a test site first.

- The target site must run new-api v1.0.0-rc.41 or newer. Earlier releases have no personal access token and no balance-adjust endpoint.
- napm serves plain HTTP and has no TLS. See [Deployment and security](docs/deploy.en.md).
- Platform tokens are stored in plaintext in the SQLite file.

## Installation

Requires Go 1.27.1 or newer.

```bash
go install github.com/nerdneilsfield/newapi-multi-website-manager@latest
```

This command installs an executable named `newapi-multi-website-manager`. To get a file named `napm`, build from source:

```bash
git clone https://github.com/nerdneilsfield/newapi-multi-website-manager.git
cd newapi-multi-website-manager
make install
```

`make install` puts `napm` in `GOBIN`, or in `$(go env GOPATH)/bin` when `GOBIN` is not set. You can also run napm with [Docker](docs/deploy.en.md#run-with-docker).

## Quick start

1. Set the initial root password (at least 8 characters) in the environment, then start napm:

   ```bash
   NAPM_ADMIN_PASSWORD=<password> napm serve --db napm.db
   ```

   The terminal prints a `listening` line whose `addr` is `127.0.0.1:8080`.

2. Open `http://127.0.0.1:8080` in a browser and log in with the user name `admin` and the password from step 1.
3. Open **Platforms**, enter a name, the target site's URL and an access token, and click **Add**. [Prepare an access token](docs/usage.en.md#prepare-an-access-token) explains how to create the token.
4. Return to **Stats** and click **Refresh stats**. When the refresh finishes, the page shows the user count, total balance, total usage, request count and a per-group breakdown.

The web interface is available in Chinese and English. Use the language button in the top bar to switch.

## Documentation

- [User guide](docs/usage.en.md): create tokens, grant administrators access, change balances, schedule jobs, troubleshoot.
- [Reference](docs/reference.en.md): command-line flags, environment variables, HTTP API, status values and limits.
- [Deployment and security](docs/deploy.en.md): listen address, reverse proxy, Docker, backup and security boundaries.

## Development

```bash
make check
```

`make check` runs the format check, lint, `go vet`, the tests and `goreleaser check`. `just` provides targets with the same names. Install golangci-lint and goreleaser separately; gofumpt, goimports and lefthook are `tool` dependencies in `go.mod`.

The Git hooks check the format of staged Go files and require Conventional Commit messages.

To release, push a `v*` tag; GoReleaser builds the binaries and images. Images go only to `ghcr.io/nerdneilsfield/napm`, authenticated with the repository's own `GITHUB_TOKEN`, so no extra secret is needed.

## License

This project is licensed under the MIT License. See [LICENSE](LICENSE).
