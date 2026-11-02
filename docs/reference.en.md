# Reference

**English** | [中文](reference.md)

This page lists napm's command-line flags, environment variables, HTTP API, status values and limits. For procedures, see the [user guide](usage.en.md).

## Command line

### `napm serve`

Starts the web page and the job scheduler. After `SIGINT` or `SIGTERM`, napm stops accepting new requests and shuts the HTTP server down within 5 seconds.

```text
napm serve [--addr <address>] [--db <path>] [--interval <duration>] [--usage-months <months>] [-v]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--addr` | string | `127.0.0.1:8080` | HTTP listen address, in the form `<host>:<port>`. |
| `--db` | path | `napm.db` | SQLite database file. napm creates it if it does not exist and sets its file mode to `0600` on every start. |
| `--interval` | duration | `600ms` | Minimum gap between two requests to one platform. For example, `1s`. |
| `--usage-months` | integer | `12` | How many months of usage to keep. The first collection backfills that far, and each later one deletes older rows. |
| `-v`, `--verbose` | boolean | off | Print debug logs. Applies to all commands. |

Logs go to stderr.

At start-up, napm marks every run that was still `running` when the previous process exited as `interrupted`.

### `napm version`

Prints the version, commit, build time and Go version.

## Environment variables

napm reads these variables only while the database contains no administrator. Changing them later has no effect.

| Variable | Default | Description |
| --- | --- | --- |
| `NAPM_ADMIN_USER` | `admin` | User name of the initial root administrator. |
| `NAPM_ADMIN_PASSWORD` | generated at random | Password of the initial root administrator, at least 8 characters. If it is shorter, `napm serve` reports an error and exits. If it is not set, napm generates a random password and prints it once on stderr. |

## Amounts

| Item | Rule |
| --- | --- |
| Mode `add` | The amount must be greater than 0. |
| Mode `subtract` | The amount must be greater than 0. |
| Mode `override` | The amount must be greater than or equal to 0. |
| Unit `usd` | quota = amount × `quota_per_unit`, rounded to an integer. `quota_per_unit` comes from the target site's `/api/status`. |
| Unit `quota` | The amount is the raw quota. |
| Ceiling | If the converted quota is greater than 2^53 − 1 (9007199254740991), the whole run fails. |

## Schedule expressions

napm accepts the standard `robfig/cron` forms. An expression has no seconds field:

- 5-field cron: `<minute> <hour> <day of month> <month> <day of week>`.
- Predefined: `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly`.
- Interval: `@every <duration>`, with the same duration syntax as Go's `time.ParseDuration`, such as `@every 90m`.
- Time zone: the local time zone of the machine that runs napm by default. You can start an expression with `CRON_TZ=<zone> `, for example `CRON_TZ=UTC 0 3 * * *`.

napm rejects 6-field expressions that include seconds.

## Run statuses

| Status | Meaning |
| --- | --- |
| `running` | The run is in progress. |
| `ok` | All users were handled successfully. A successful statistics refresh is also `ok`. |
| `partial` | At least one user succeeded and at least one user failed. |
| `failed` | The whole run failed, or no user succeeded. |
| `interrupted` | napm was stopped in the middle of the run, or found a `running` record left by the previous process at start-up. The "succeeded/total" count is a lower bound; see the [user guide](usage.en.md#a-balance-run-has-the-status-interrupted). |
| `skipped` | A scheduled job fired while the platform already had a job running, so this firing was skipped. |

A run stores at most 50 errors. Any further errors are replaced by one line reading `另有 N 条错误` (N more errors). The text is stored in Chinese.

## HTTP API

The web page works through these endpoints. They carry no version guarantee and may change between releases.

### General rules

- Request and response bodies are JSON. A request body must not exceed 1 MiB.
- Every endpoint except `POST /api/login` requires a logged-in session or an API token. The session cookie is named `napm_session`, lasts 7 days, and has the attributes `HttpOnly` and `SameSite=Strict`. napm adds `Secure` only when the request reaches napm over TLS. An API token goes in the header `Authorization: Bearer <token>`; see [API tokens](#api-tokens). When a request carries an `Authorization` header, only the token counts: an invalid token returns `401` and napm does not fall back to the cookie.
- A state-changing request that uses a session (any method except `GET`) requires `Content-Type: application/json`. The request must also not be cross-origin. Otherwise napm returns `415` or `403`. Go's standard `http.CrossOriginProtection` decides: it checks `Sec-Fetch-Site` first and, when the browser sends none, requires the host in `Origin` to equal `Host`. A request that uses a token skips both checks, because a browser never attaches a token on its own.
- On failure, napm returns the matching HTTP status code and `{"error": "<reason>"}`.
- A platform endpoint with `{id}` requires the current administrator to be granted that platform; otherwise napm returns `403`.

### Session

| Method and path | Permission | Description |
| --- | --- | --- |
| `POST /api/login` | none | Request `{"username", "password"}`. On success, napm sets the session cookie. A failure returns `401`; a locked-out client gets `429`. |
| `POST /api/logout` | none | Deletes the current session. |
| `GET /api/me` | logged in | Returns `{"id", "username", "root"}`. Returns `401` when not logged in. |

### Platforms

| Method and path | Permission | Description |
| --- | --- | --- |
| `GET /api/platforms` | logged in | Returns the platforms visible to the current administrator, each with `id`, `name` and `base_url`. It never returns tokens. |
| `POST /api/platforms` | root | Adds a platform. Request `{"name", "base_url", "token"}`. Returns `400` when the URL is not `http(s)`, has no host, or contains a user name or password. |
| `PUT /api/platforms/{id}` | root | Edits a platform. An empty `token` keeps the stored token. |
| `DELETE /api/platforms/{id}` | root | Deletes the platform with its statistics, jobs, records and grants. |

### Statistics and users

| Method and path | Description |
| --- | --- |
| `GET /api/platforms/{id}/stats` | Returns `{"stats", "last_run"}`. `stats` is the snapshot of the latest successful refresh, or `null` if the platform has never been refreshed. `last_run` is the latest run record. |
| `POST /api/platforms/{id}/refresh` | Starts a statistics refresh. Returns `{"run_id"}`. Returns `409` when the platform already has a job running. |
| `GET /api/platforms/{id}/users` | Query parameters: `page` (default 1), `group`, `keyword`. Returns 100 users per page and leaves out users deleted on the target site. Returns `502` when the target site reports an error. |

Fields of `stats`:

| Field | Type | Description |
| --- | --- | --- |
| `at` | integer | Time the refresh finished, in Unix seconds. |
| `quota_per_unit` | number | The target site's quota conversion ratio. |
| `users` | integer | Total number of users. |
| `quota` | integer | Total balance, in raw quota. |
| `used` | integer | Total usage, in raw quota. |
| `requests` | integer | Total number of requests. |
| `groups` | array | Sorted by group name in ascending order. Each item has `name`, `users`, `quota`, `used` and `requests`. |

### Usage

| Method and path | Description |
| --- | --- |
| `POST /api/platforms/{id}/usage/collect` | Starts collecting usage and returns `{"run_id"}`. The platform token needs `log:read`. Returns `409` when a run is already in progress on the platform. |
| `GET /api/platforms/{id}/usage` | Returns the collected usage, without any analysis. |

Query parameters of the `GET`:

| Parameter | Default | Description |
| --- | --- | --- |
| `from`, `to` | 30 days ago, now | Unix seconds, the range is `[from, to)`. With `step=hour`, `from` defaults to 14 days ago. |
| `step` | `day` | `day` sums the hourly rows into days, `hour` returns the hourly rows. An `hour` range is at most 14 days. Any other value returns `400`. |
| `tz` | `0` | Minutes east of UTC, which sets where a day starts. UTC+8 is `480`. |

It returns `{"state", "from", "to", "step", "models", "users"}`. `state` is `{"collected_to", "quota_per_unit"}`, or `null` when nothing was collected. Each item of `models` has `t` (the start of the bucket, Unix seconds), `model`, `count` (requests), `quota` (raw quota) and `tokens`; `users` is the same with `username` in place of `model`. Divide `quota` by `quota_per_unit` for money.

new-api keeps adding to an hour's counters after the hour ends, so each collection starts 2 hours before the last watermark, and an hour fetched again replaces its old value instead of adding to it.

### Change balances

`POST /api/platforms/{id}/quota`

The request body is an adjustment plan:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `mode` | string | yes | `add`, `subtract` or `override`. |
| `amount` | number | yes | The amount. See [Amounts](#amounts) for the rules. |
| `unit` | string | yes | `usd` or `quota`. |
| `group` | string | one of `group` and `user_ids` | Group name. |
| `user_ids` | integer array | one of `group` and `user_ids` | User IDs. An ID that appears more than once is handled once. |

Exactly one of `group` and `user_ids` must be given. An invalid parameter returns `400`; a platform that already has a job running returns `409`; a successful start returns `{"run_id"}`. The change runs in the background.

Example: add 1 USD to every user in the group `vip`.

```json
{"mode": "add", "amount": 1, "unit": "usd", "group": "vip"}
```

### Jobs and run records

| Method and path | Description |
| --- | --- |
| `GET /api/platforms/{id}/jobs` | Lists the platform's jobs. |
| `POST /api/platforms/{id}/jobs` | Adds a job. |
| `PUT /api/platforms/{id}/jobs/{job}` | Edits a job. |
| `DELETE /api/platforms/{id}/jobs/{job}` | Deletes a job. |
| `GET /api/platforms/{id}/runs` | Returns the last 50 run records, newest first. |

Fields of a job:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | yes | Job name. |
| `kind` | string | yes | `refresh`, `usage` or `quota`. |
| `spec` | string | yes | Schedule expression. See [Schedule expressions](#schedule-expressions). An invalid one returns `400`. |
| `enabled` | boolean | no | Whether the job is enabled. |
| `plan` | object | required when `kind` is `quota` | Adjustment plan, with the fields above. Ignored when `kind` is `refresh` or `usage`. |

### Audit log

| Method and path | Permission | Description |
| --- | --- | --- |
| `GET /api/audit` | Signed in | Returns 200 entries per call, newest first. The query parameter `before` is the `id` of the last entry of the previous page and returns the 200 entries older than it; without it the list starts at the newest. Root sees the entries of every administrator; other administrators see only their own. |

Each entry has `id`, `at` (Unix seconds), `admin_id`, `admin`, `platform_id`, `platform`, `action` and `detail`. `admin` and `platform` are the names at the time of the action, so an entry stays readable after the account or platform is deleted. After an administrator is deleted, only root can see that administrator's entries; an account later created with the same name cannot. Values of `action`:

| `action` | When | Content of `detail` |
| --- | --- | --- |
| `login` | A successful login | Empty object. Failed logins are not recorded. |
| `refresh` | A manual refresh starts | `run_id` |
| `usage.collect` | A usage collection is started by hand | `run_id` |
| `quota` | A manual balance change starts | `run_id` and `plan` |
| `job.create`, `job.update` | A job is added or changed, including enabling or disabling it | `name`, `kind`, `spec`, `enabled`, `plan` |
| `job.delete` | A job is deleted | `name` |
| `platform.create`, `platform.update` | A platform is added or changed | `name`, `base_url`, `token_changed`. The token itself is never stored. |
| `platform.delete` | A platform is deleted | `name` |
| `admin.create`, `admin.update` | An administrator is added or changed | `username`, `root`, `read_only`, `platform_ids`, and `password_changed` on update. The password is never stored. |
| `admin.delete` | An administrator is deleted | `username` |
| `token.create`, `token.delete` | An API token is generated or deleted | `username`, the administrator the token belongs to |

Only actions that a person takes through the web page or the API are recorded. Runs fired by a scheduled job are not in the audit log; they are in the run records with the source `cron`.

### API tokens

A token lets an MCP client or a script call the HTTP API as one administrator. It has exactly that administrator's permissions: which platforms it reaches and whether it is root depend on the administrator's current settings, and a change applies at once.

Each administrator has at most one token. An administrator can generate it for themself, and root can generate it for any administrator. Root can read anyone's token, while an administrator can read only their own. Generating a new one replaces the old one, which stops working at once. A token starts with `napm_` followed by 64 hexadecimal characters and never expires. napm stores it in plaintext, protected like the platforms' access tokens by the `0600` mode of the database file. Deleting an administrator, or changing the administrator's password, deletes the token with it.

The three token endpoints below, and `POST /api/admins`, `PUT /api/admins/{id}` and `DELETE /api/admins/{id}` that add, change and delete administrators, accept a session only. A call with a token returns `403`. Otherwise a leaked root token could create a root administrator with a password of its own choosing, or swap itself for a new token, and deleting the original token would not stop it. `GET /api/admins` can still be read with a token; it returns `has_token` but never the token itself.

| Method and path | Description |
| --- | --- |
| `GET /api/admins/{id}/token` | Returns `{"token"}`, an empty string when there is none. Your own, or anyone's for root. Another administrator's returns `403`, and an unknown administrator returns `404`. |
| `POST /api/admins/{id}/token` | Generates a new token and returns `{"token"}`; the existing one is revoked. Same permissions as above. |
| `DELETE /api/admins/{id}/token` | Deletes the token, which stops working at once. Same permissions as above. |

Generating and deleting a token is written to the audit log as `token.create` and `token.delete`; the details hold only the administrator's user name. The actor is the person who did it, so when root generates a token for someone else, the entry names root.

### Administrators

All of these endpoints require root.

| Method and path | Description |
| --- | --- |
| `GET /api/admins` | Lists administrators, each with `id`, `username`, `root`, `read_only`, `platform_ids` and `has_token`. |
| `POST /api/admins` | Adds one. Request `{"username", "password", "root", "read_only", "platform_ids"}`. A password shorter than 8 characters returns `400`, and so do `root` and `read_only` both true. |
| `PUT /api/admins/{id}` | Edits one. An empty `password` keeps the password. Changing the password or permissions ends that administrator's sessions. |
| `DELETE /api/admins/{id}` | Deletes one. You cannot delete yourself. |

## MCP endpoint

napm serves an [MCP](https://modelcontextprotocol.io) server at `POST /mcp`, so an AI client can manage platforms with tools.

### How to connect

- URL: `http(s)://<napm address>/mcp`.
- Authentication: the header `Authorization: Bearer <API token>`. Only a token works, never the session cookie, so a page open in the browser cannot drive it. A missing or invalid token returns `401`.
- Implementation: napm uses the Streamable HTTP handler of the official Go SDK, [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk). The SDK handles protocol versions, header checks and error codes.
- Transport: stateless mode. Each `POST` is an independent request and napm answers with one JSON body; it does not use `Mcp-Session-Id`. The request needs `Content-Type: application/json` and an `Accept` that contains both `application/json` and `text/event-stream`, as the SDK requires. In stateless mode `GET /mcp` and `DELETE /mcp` return `405`, and napm pushes no server messages.
- Capabilities: tools only, no resources or prompts.

### Tools

Each tool calls the HTTP endpoint in the table below inside the process, with the caller's own token. Authorization, validation and the audit log therefore behave as they do on the web page: a successful write is recorded; reads and failed requests are not. The argument names match the endpoint's JSON fields; `platform_id` is the platform `{id}` in the path and `job_id` is the job ID.

| Tool | What it does | HTTP endpoint |
| --- | --- | --- |
| `list_platforms` | Lists the platforms you may manage. | `GET /api/platforms` |
| `get_stats` | Reads the statistics from the last refresh and the last run. | `GET /api/platforms/{id}/stats` |
| `refresh_stats` | Starts a refresh and returns a `run_id` at once. | `POST /api/platforms/{id}/refresh` |
| `list_users` | Lists the platform's users page by page; filter by `group` or `keyword`. | `GET /api/platforms/{id}/users` |
| `adjust_quota` | Changes the balance of some users or of a whole group and returns a `run_id` at once. | `POST /api/platforms/{id}/quota` |
| `get_usage` | Reads the collected usage rows (per model and per user, by day or by hour). Trends, rankings and anomalies are for the caller to compute from these rows. | `GET /api/platforms/{id}/usage` |
| `collect_usage` | Starts collecting the platform's usage and returns a `run_id` at once. | `POST /api/platforms/{id}/usage/collect` |
| `list_runs` | Lists the last 50 runs. | `GET /api/platforms/{id}/runs` |
| `list_jobs`, `create_job`, `update_job`, `delete_job` | Manage scheduled jobs. | `/api/platforms/{id}/jobs` |
| `list_audit` | Reads the audit log; page back with `before`. | `GET /api/audit` |

When a tool fails, napm still returns a normal JSON-RPC result with `isError` set to `true`, and the text is the endpoint's `{"error": "..."}`, so the client and the model can correct the arguments. The messages are in Chinese.

`adjust_quota` really changes balances on the target site. `add` and `subtract` are not idempotent, so a client retry applies them again; use `override` when you need idempotence.

### Example client configuration

Create a token on the **Tokens** page; the page shows the configuration too. For a client that supports HTTP transport and custom headers, it looks like this:

```json
{
  "mcpServers": {
    "napm": {
      "type": "http",
      "url": "https://napm.example.com/mcp",
      "headers": { "Authorization": "Bearer napm_<token>" }
    }
  }
}
```

## Limits

| Item | Value |
| --- | --- |
| Login lockout | 5 consecutive failures from one client IP or for one user name lock it for 1 minute. |
| Session lifetime | 7 days, kept in memory. |
| Users per page | 100, set by new-api's cap. |
| Request gap | 600 ms by default, per platform. new-api by default allows 360 requests per 180 seconds per IP. |
| Read hits HTTP 429 | napm waits for the `Retry-After` time (10 seconds when it is missing or above 300 seconds) and retries up to 3 times. Writes are not retried. |
| Concurrency | One job runs per platform at a time. |
| HTTP timeouts | 10 seconds to read request headers, 30 seconds to read the whole request, 2 minutes to write the response, 2 minutes for an idle connection. |
| Redirects | napm does not follow redirects from a target site. |

## Data

The SQLite database contains these tables:

| Table | Content |
| --- | --- |
| `admins` | Administrators. Passwords are stored as bcrypt hashes and API tokens in plaintext. |
| `platforms` | Platforms. Tokens are stored in plaintext. |
| `admin_platforms` | Which administrator may manage which platform. |
| `snapshots` | The latest statistics snapshot of each platform. |
| `usage_model`, `usage_user` | Collected hourly usage, one row per platform, hour and model or user. |
| `usage_state` | How far each platform's usage has been collected, and its `quota_per_unit` at the time. |
| `jobs` | Scheduled jobs. |
| `runs` | Run records. |
| `audit` | Audit log. It is not deleted with a platform or an administrator, and it is never pruned automatically. |

Deleting a platform or an administrator also deletes the related grants, snapshots, jobs and run records. The audit log is kept. Deleting an administrator also deletes that administrator's API token.
