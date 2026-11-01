# Reference

**English** | [中文](reference.md)

This page lists napm's command-line flags, environment variables, HTTP API, status values and limits. For procedures, see the [user guide](usage.en.md).

## Command line

### `napm serve`

Starts the web page and the job scheduler. After `SIGINT` or `SIGTERM`, napm stops accepting new requests and shuts the HTTP server down within 5 seconds.

```text
napm serve [--addr <address>] [--db <path>] [--interval <duration>] [-v]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--addr` | string | `127.0.0.1:8080` | HTTP listen address, in the form `<host>:<port>`. |
| `--db` | path | `napm.db` | SQLite database file. napm creates it if it does not exist and sets its file mode to `0600` on every start. |
| `--interval` | duration | `600ms` | Minimum gap between two requests to one platform. For example, `1s`. |
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
- Every endpoint except `POST /api/login` requires a logged-in session. The session cookie is named `napm_session`, lasts 7 days, and has the attributes `HttpOnly` and `SameSite=Strict`. napm adds `Secure` only when the request reaches napm over TLS.
- A state-changing request (any method except `GET`) requires `Content-Type: application/json`. If the request carries an `Origin` header, its host must equal `Host`. Otherwise napm returns `415` or `403`.
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
| `kind` | string | yes | `refresh` or `quota`. |
| `spec` | string | yes | Schedule expression. See [Schedule expressions](#schedule-expressions). An invalid one returns `400`. |
| `enabled` | boolean | no | Whether the job is enabled. |
| `plan` | object | required when `kind` is `quota` | Adjustment plan, with the fields above. Ignored when `kind` is `refresh`. |

### Audit log

| Method and path | Permission | Description |
| --- | --- | --- |
| `GET /api/audit` | Signed in | Returns 200 entries per call, newest first. The query parameter `before` is the `id` of the last entry of the previous page and returns the 200 entries older than it; without it the list starts at the newest. Root sees the entries of every administrator; other administrators see only their own. |

Each entry has `id`, `at` (Unix seconds), `admin_id`, `admin`, `platform_id`, `platform`, `action` and `detail`. `admin` and `platform` are the names at the time of the action, so an entry stays readable after the account or platform is deleted. After an administrator is deleted, only root can see that administrator's entries; an account later created with the same name cannot. Values of `action`:

| `action` | When | Content of `detail` |
| --- | --- | --- |
| `login` | A successful login | Empty object. Failed logins are not recorded. |
| `refresh` | A manual refresh starts | `run_id` |
| `quota` | A manual balance change starts | `run_id` and `plan` |
| `job.create`, `job.update` | A job is added or changed, including enabling or disabling it | `name`, `kind`, `spec`, `enabled`, `plan` |
| `job.delete` | A job is deleted | `name` |
| `platform.create`, `platform.update` | A platform is added or changed | `name`, `base_url`, `token_changed`. The token itself is never stored. |
| `platform.delete` | A platform is deleted | `name` |
| `admin.create`, `admin.update` | An administrator is added or changed | `username`, `root`, `platform_ids`, and `password_changed` on update. The password is never stored. |
| `admin.delete` | An administrator is deleted | `username` |

Only actions that a person takes through the web page or the API are recorded. Runs fired by a scheduled job are not in the audit log; they are in the run records with the source `cron`.

### Administrators

All of these endpoints require root.

| Method and path | Description |
| --- | --- |
| `GET /api/admins` | Lists administrators, each with `id`, `username`, `root` and `platform_ids`. |
| `POST /api/admins` | Adds one. Request `{"username", "password", "root", "platform_ids"}`. A password shorter than 8 characters returns `400`. |
| `PUT /api/admins/{id}` | Edits one. An empty `password` keeps the password. Changing the password or permissions ends that administrator's sessions. |
| `DELETE /api/admins/{id}` | Deletes one. You cannot delete yourself. |

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
| `admins` | Administrators. Passwords are stored as bcrypt hashes. |
| `platforms` | Platforms. Tokens are stored in plaintext. |
| `admin_platforms` | Which administrator may manage which platform. |
| `snapshots` | The latest statistics snapshot of each platform. |
| `jobs` | Scheduled jobs. |
| `runs` | Run records. |
| `audit` | Audit log. It is not deleted with a platform or an administrator, and it is never pruned automatically. |

Deleting a platform or an administrator also deletes the related grants, snapshots, jobs and run records. The audit log is kept.
