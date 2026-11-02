# Deployment and security

**English** | [中文](deploy.md)

This page explains how to run napm on a server and what its security boundaries are.

## Security boundaries

Read these points before you deploy. They decide which networks napm can sit in.

- napm serves plain HTTP and has no TLS. Login passwords and session cookies cross the network in clear text. Listen on the local machine only, or put napm behind a reverse proxy that terminates TLS.
- Platform access tokens are stored in plaintext in the SQLite file, whose file mode is `0600`. Anyone who can read the file can use the tokens to change balances on the target sites.
- An access token has the `user:write` permission. Its holder can change the balance of any user on the target site.
- Sessions are kept in memory, and napm is designed to run as a single instance. Do not run several napm processes on the same database.
- napm does not restrict the platform addresses you can add, including private addresses, because a self-hosted new-api usually sits on an internal network. Only root administrators can add platforms, so give root only to people you trust.
- The audit log records who did what and when, but not each user's balance before and after a change. A run record stores only the time, source, status and errors.
- An API token (used for MCP) has the same permissions as the administrator it belongs to, and a root token can change balances on every platform. napm stores the token in plaintext, protected by the `0600` mode of the database file, and root can see every administrator's token on the web page. A token never expires, so after a leak you must regenerate or delete it on the web page. A token travels in the `Authorization` header in plaintext, so `/mcp` must sit behind TLS as well.

Protections napm provides:

| Threat | Measure |
| --- | --- |
| Password guessing | 5 consecutive failures from one client IP or for one user name lock each for 1 minute. |
| Cross-site requests | A state-changing request must be JSON and pass Go's standard `http.CrossOriginProtection` check (`Sec-Fetch-Site`, or `Origin` compared with `Host`). The session cookie has `SameSite=Strict`. |
| Token sent to another host | napm does not follow redirects from a target site. |
| Privilege escalation | A non-root administrator who requests a platform they are not granted gets `403`. |
| Clickjacking and content sniffing | Responses carry `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff` and `Referrer-Policy: no-referrer`. |

### Client IP behind a reverse proxy

The login lockout counts failures separately by the connection's source address and by user name. A client is refused once either count reaches 5. napm does not read `X-Forwarded-For`. Behind a reverse proxy, every request comes from the proxy's address, so anyone who enters a wrong password 5 times in a row blocks everybody from logging in for 1 minute. An attacker who sends 5 wrong logins per minute can keep all administrators out.

If napm is exposed to an untrusted network, limit the request rate of `POST /api/login` at the reverse proxy, or allow only trusted source addresses to reach napm.

### The `Secure` cookie attribute

napm adds `Secure` to the session cookie only when it receives a TLS connection itself. Behind a reverse proxy that terminates TLS, napm sees a plaintext connection and does not add `Secure`. If you need it, add `Secure` to the response's `Set-Cookie` at the reverse proxy.

## Run on a server

Prerequisites:

- A machine that can reach every target site.
- A data directory that napm can write to.

Steps:

1. Build and install `napm`. See the [README](../README.en.md#installation).
2. Create a dedicated data directory, such as `/var/lib/napm`, that only the user running napm can access.
3. Set the initial password on the first start:

   ```bash
   NAPM_ADMIN_PASSWORD=<password> napm serve --db /var/lib/napm/napm.db
   ```

4. After you log in, change the password or create other administrators under **Admins**.

napm uses the initial password only while the database has no administrator. Later starts do not need the variable.

### As a systemd service

The following unit runs as the user `napm` and keeps data in `/var/lib/napm`. Before the first start, write the initial password to `/etc/napm/env` as `NAPM_ADMIN_PASSWORD=<password>` and set its mode to `0600`.

```ini
[Unit]
Description=napm
After=network-online.target

[Service]
User=napm
EnvironmentFile=/etc/napm/env
ExecStart=/usr/local/bin/napm serve --db /var/lib/napm/napm.db
Restart=on-failure
StateDirectory=napm

[Install]
WantedBy=multi-user.target
```

After `SIGTERM`, napm stops accepting new requests. A balance run that is in progress is interrupted at that moment and gets the status `interrupted`; see the [user guide](usage.en.md#a-balance-run-has-the-status-interrupted). Before you stop the service, check on the **Runs** page that no balance run is in progress.

### Reverse proxy

Keep napm listening on `127.0.0.1:8080` and let a reverse proxy provide HTTPS. With Caddy, for example:

```text
napm.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

The reverse proxy must keep the `Host` header and forward the browser's `Origin` header unchanged. When the browser sends no `Sec-Fetch-Site`, napm compares the host in `Origin` with `Host` and rejects a state-changing request when they differ.

## Run with Docker

The image runs as a non-root user (UID 65532). Its working directory and data volume are `/data`, and its default command is:

```text
serve --addr 0.0.0.0:8080 --db /data/napm.db
```

Listening on `0.0.0.0` inside a container is the usual practice. The `-p` option of `docker run` controls exposure, so publish the port to the local machine only and forward to it from a reverse proxy:

```bash
docker run -d --name napm \
  -p 127.0.0.1:8080:8080 \
  -v napm-data:/data \
  -e NAPM_ADMIN_PASSWORD=<password> \
  ghcr.io/nerdneilsfield/napm:latest
```

Notes:

- Do not write `-p 8080:8080`. It publishes plain HTTP on every network interface of the host.
- If you do not set `NAPM_ADMIN_PASSWORD`, the random initial password appears only in the container log. Read it with `docker logs napm`.
- When you mount a host directory at `/data`, the directory must be writable by UID 65532. A named volume, like `napm-data` above, needs no extra setup.

The author has not yet built and run the image to verify it.

## Backup and restore

The database uses WAL mode. While napm runs, `napm.db-wal` and `napm.db-shm` exist next to `napm.db`. Copying only `napm.db` can miss data that has not yet been written back.

To back up:

1. Stop napm, wait until `napm.db-wal` disappears, then copy `napm.db`.
2. Or, while napm runs, use SQLite's online backup command:

   ```bash
   sqlite3 /var/lib/napm/napm.db ".backup '/backup/napm.db'"
   ```

A backup contains plaintext tokens. Treat it as a secret: restrict access to it and do not put it on public or shared storage.

To restore, stop napm, replace the database file with the backup, delete the old `-wal` and `-shm` files, and start napm.

## Upgrade

1. Stop napm. Make sure no balance run is in progress.
2. Back up the database.
3. Replace the binary or the image, then start napm.

At start-up, napm brings the database up to the current version with [goose](https://github.com/pressly/goose) and keeps the existing data. The version is recorded in the table `goose_db_version`. A migration cannot be rolled back, so the backup you took before upgrading is the only way back.
