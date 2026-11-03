# User guide

**English** | [中文](usage.md)

This guide covers day-to-day use of napm. For installation and the first start, see the [README](../README.en.md).

The web interface is available in Chinese and English. Use the language button in the top bar to switch; the choice is kept in the browser. On first visit the page follows the browser language. This guide uses the English labels.

## Prepare an access token

napm reads users and changes balances through the target site's admin API, so each platform needs a personal access token (PAT).

Prerequisites:

- The target site runs new-api v1.0.0-rc.41 or newer.
- You can log in to the target site as an administrator.

Steps:

1. Log in to the target site and open the security settings ("安全设置").
2. Create an access token with the three permissions `user:read`, `user:write` and `group:read`. To collect usage (see [View usage](#view-usage)), add `log:read`. The token starts with `nap_`.
3. Copy the token. The target site shows it only when you create it.

You can create an access token only by hand on the target site, not with another access token. The account that owns the token must be an administrator of the target site.

## Add a platform

Only root administrators can add, edit and delete platforms.

1. Open **Platforms**.
2. Enter a name, a URL and the token, then click **Add**. The URL starts with `http://` or `https://` and must not contain a user name or password.
3. After you save, napm no longer shows the token. When you edit a platform, leave the token empty to keep the stored one.

Deleting a platform also deletes its statistics, scheduled jobs, run records and administrator grants.

## Manage administrators

Only root administrators can manage administrator accounts.

1. Open **Admins**.
2. In the last row, enter a user name and a password (at least 8 characters), tick the platforms the administrator may manage, and click **Add**.
3. A non-root administrator sees only the ticked platforms after logging in.
4. Ticking "root" makes the account a root administrator, who can manage all platforms and all administrators.
5. Ticking "Read-only" makes the account look but not touch. It can see the granted platforms' statistics, users, usage, jobs and run records, but cannot refresh, collect, adjust balances, change jobs, or add or delete platforms and administrators. Its API token is read-only too, and it can still manage its own token. Root cannot also be read-only.

After you change an administrator's password or permissions, that administrator's logged-in sessions end at once and the administrator must log in again. A root administrator cannot remove their own root permission or delete their own account.

## View statistics

The **Stats** page shows the result of the latest refresh: the number of users, total balance, total usage and request count, and the same four values for each group. Amounts are shown in USD, converted with the target site's `quota_per_unit`.

Clicking **Refresh stats** reads all users on the target site and totals them again. A refresh takes a while when there are many users, because napm waits at least 600 ms between requests to one platform and reads 100 users per page. While the refresh runs, the button reads **Refreshing…**, and the page updates when it finishes.

The **Users** page lists the target site's users page by page. You can filter by keyword (user name, email or ID) and by group. This page shows balances as raw quota.

## View usage

The **Usage** page answers where the money goes, when, and who is using it. Its data comes from the target site's data dashboard (`/api/data/`), so the platform token needs `log:read` and the site must have the dashboard on.

1. Open **Usage** and click **Collect usage**. The first run backfills the last 12 months; later runs fetch only what is new. A long backfill goes in pieces, and if it fails halfway, click again to resume from where it stopped.
2. Under **Schedules**, add a job of type "Collect usage", for example `0 * * * *`, to keep the data fresh.
3. Pick a range at the top: the last hour, 5 hours, 12 hours, 1 day, 7 days, 14 days or 30 days. Ranges longer than a day are counted in days, cut by your local time zone; shorter ones are counted in whole hours, and the current, unfinished hour is left out.

The page shows four figures (spend, requests, tokens and active users, each compared with the period of the same length before), a set of "What stands out" lines, a bar chart of spend per day, or per hour for the shorter ranges, the spread of requests across the day, and rankings by model, by user and by group.

A user name or group name in the rankings opens that user's or group's own page: spend, requests, share of the platform, spend per day, daily share of the platform, requests across the day, and a set of "What stands out" lines about it. The **Usage** button on each row of the **Users** page goes straight to that user. Groups are the ones users were in at the last statistics refresh, so refresh statistics once before **By group** appears. A user who has since moved is counted, history included, in the new group.

Every line under "What stands out" is computed from your data, and a line without support does not appear:

- The change against the previous period, and a month's spend extrapolated from the daily average of the last 7 days.
- An unusual day: spend more than three spreads above the usual (the median) and at least 75% higher. The bar chart outlines it.
- One user or model with a very large share; the biggest rise and drop among users; how many users are new and how many did not appear again.
- The quietest four hours in a row each day, a good window for maintenance or bulk adjustments, and the busiest hour; weekends clearly lower or higher than weekdays.

The analysis runs in your browser. The server stores only the hourly rows. A "day" follows your computer's local time zone, and today is not counted because it is not over. An hour range works the same way: only finished hours are counted. Anomaly days, the weekend ratio and the hour-of-day view only appear in day ranges, since one or two hours cannot support them. Usage is kept for 12 months by default, set with `--usage-months`; rows beyond that are deleted after each collection.

## Change balances

1. Open **Adjust balance**.
2. Choose a mode:
   - Add: add the amount to the current balance.
   - Subtract: subtract the amount from the current balance.
   - Set: set the balance to the amount, whatever the balance was before.
3. Enter the amount and choose the unit. **USD** converts the amount to quota with the target site's `quota_per_unit`. **Raw quota** uses the number as entered.
4. Choose the target: **Whole group**, typing the group name; the box suggests the known groups (the list comes from the last statistics refresh), and a group the site added since then can simply be typed; or **Specific users**, ticking users in the table below. The table filters by keyword and group, and the picked users stay when you change page or filter. The tags above the table remove them one by one.
5. Click **Run** and confirm the dialog.

The change runs in the background and the page returns at once. Open **Runs** to see progress and the result.

napm changes users one at a time. When a user fails, napm records the reason and continues with the other users. The following cases fail a single user and leave the others unaffected:

- A subtraction is larger than the user's balance at the start of the run. new-api itself allows a negative balance; napm does not.
- The balance after an addition would exceed new-api's balance ceiling (2^53 − 1).
- The target site rejects the request, or the given user does not exist.

If the amount itself exceeds the balance ceiling, the whole run fails and no user is changed.

napm never retries a failed change automatically.

## Schedule jobs

1. Open **Schedules**.
2. Enter a job name and choose the type: **Refresh stats**, **Collect usage** or **Adjust balance**.
3. Enter the schedule. Two forms are accepted:
   - A 5-field cron expression in the order minute, hour, day of month, month, day of week. For example, `0 3 * * *` runs every day at 03:00.
   - A predefined form such as `@every <interval>` or `@daily`. For example, `@every 6h` runs every 6 hours.
4. For the type **Adjust balance**, also enter the mode, amount, unit and target. The rules are the same as for an immediate change.
5. Click **Add job**.

Times follow the local time zone of the machine that runs napm. Tick or clear the **Enabled** box in a job's row to enable or disable the job.

Only one job runs per platform at a time. If a scheduled job fires while the platform already has a job running, napm does not run it and records an entry with the status `skipped`.

### A scheduled "add" is not idempotent

new-api's adjust endpoint has no de-duplication, and napm does not keep a ledger of past changes. A scheduled "add" therefore adds again every time it fires. To bring a balance back to a fixed value, use "set".

## View run records

The **Runs** page lists the last 50 runs of the platform. Each entry shows the start time, type, content, source, status and **OK / total**. The source is the user name of the administrator who triggered the run, or `cron` (shown as **scheduler** in the English interface). Error messages stored with a run are in Chinese in both languages, because the server writes them when the run happens.

The [reference](reference.en.md#run-statuses) describes each status. When a run has failed users, the entry shows up to 50 errors below it, each with the user name, user ID and reason.

## Connect an AI client with MCP

napm offers an MCP endpoint, so an AI client can read statistics, change balances and manage scheduled jobs as you.

1. Log in to the web page, open **Tokens** and click **Generate token**. Root can also generate one for any administrator on the **Admins** page.
2. Click **Show** to see the full token and a client configuration, and **Copy** to copy the token.
3. Put the configuration into an MCP client that supports HTTP transport and custom headers. The URL is `<napm address>/mcp` and the header is `Authorization: Bearer <token>`. The page starts from the address of the browser tab. If clients reach napm at another address, such as a reverse-proxy domain, change it in **Address clients use to reach napm**. The configuration follows, and the browser remembers it.

Each administrator has one token, and you can see it at any time. **Regenerate** replaces it with a new one and the old one stops working at once, so update any client that uses it. **Delete** leaves you with no token.

A token has your permissions. You manage only the platforms granted to you, and so does the token. A root token can change balances on every platform, so keep it like a password. Root can see every administrator's token on the **Admins** page, while an administrator sees only their own. When a token is lost, leaked or no longer needed, regenerate or delete it; any client that uses the old one loses access at once. Changing your password also revokes it. A token cannot generate or delete tokens, nor add, change or delete administrator accounts. Do those on the web page after you log in.

When an AI client calls `adjust_quota`, it really changes balances, and a retried "add" or "subtract" applies again. Try it on a test platform before you let an AI client change balances. As on the web page, a successful write (refresh, balance change, creating, changing or deleting a job) is written to the audit log under the token's owner. Reads are not recorded, and neither is a request that is refused or fails validation. The tools are listed in the [reference](reference.en.md#mcp-endpoint).

## View the audit log

The **Audit log** page lists actions, newest first, 200 at a time. Click **Load older entries** to go further back. Root sees the actions of every administrator; other administrators see only their own. Each entry has the time, administrator, platform, action and details.

Recorded actions: signing in, refreshing stats, adjusting balances, adding, changing or deleting a job, adding, changing or deleting a platform, adding, changing or deleting an administrator, and generating or deleting an API token. The details say what changed. They never contain a token or a password. Failed logins are not recorded.

Runs fired by a scheduled job are not here; see the **Runs** page, where the source is `cron`. The person who created or changed the job appears here.

Entries stay after an administrator or platform is deleted. Only root can see the entries of a deleted administrator; an account later created with the same name cannot. They are not pruned automatically. To remove old ones, delete rows from the `audit` table in the database.

## Troubleshooting

### A refresh fails after you add a platform

The error in the run record gives the cause:

| Error message | Cause and fix |
| --- | --- |
| `HTTP 401` or `HTTP 403` | The token is invalid, deleted, or lacks a permission. Create a new token on the target site with all three permissions, then update the token under **Platforms** in napm. |
| `unexpected response (is the URL a new-api site?)` | The URL does not point to a new-api site, or points to the site's front end instead of its API. Check the URL and enter only the site's root address. |
| `HTTP 429` | The target site's rate limit was hit. For reads, napm waits for the `Retry-After` time and retries up to 3 times. If it still fails, raise the request gap with `--interval` and restart. |
| Connection timeout or refused | The machine running napm cannot reach the URL. On that machine, run `curl <URL>/api/status` to check. |

### You cannot log in

After 5 failed logins, the client IP and the user name are each locked for 1 minute, and napm returns HTTP 429. Wait 1 minute and try again.

### You must log in again after a restart

Sessions are kept in memory. Restarting napm logs everyone out.

### A balance run has the status `interrupted`

napm was stopped in the middle of the run, for example because the process was killed or the container restarted. The "succeeded/total" count in the record is a lower bound: at the moment napm stopped, the last user may already have been changed but not yet counted. The record also does not store which users were handled.

To recover:

1. Check the balances of the affected users on the target site.
2. If the mode was "set", run it again. The result is the same.
3. If the mode was "add" or "subtract", do not rerun the whole batch, or users that were already handled are changed a second time. Rerun only for the users that were not handled.
