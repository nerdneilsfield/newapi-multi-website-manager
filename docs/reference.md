# 参考

[English](reference.en.md) | **中文**

本页列出 napm 的命令行参数、环境变量、HTTP 接口、状态值和限制。操作步骤见[使用手册](usage.md)。

## 命令行

### `napm serve`

启动网页和定时任务调度器。收到 `SIGINT` 或 `SIGTERM` 后，napm 停止接受新请求，并在 5 秒内关闭 HTTP 服务。

```text
napm serve [--addr <地址>] [--db <路径>] [--interval <时长>] [--usage-months <月数>] [-v]
```

| 参数 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `--addr` | 字符串 | `127.0.0.1:8080` | HTTP 监听地址，格式为 `<主机>:<端口>`。 |
| `--db` | 路径 | `napm.db` | SQLite 数据库文件。文件不存在时创建。每次启动都把文件权限设为 `0600`。 |
| `--interval` | 时长 | `600ms` | 对同一个平台发出两次请求之间的最小间隔。例如 `1s`。 |
| `--usage-months` | 整数 | `12` | 保留多少个月的用量。第一次采集回填这么久，之后每次采集删除更早的行。 |
| `-v`, `--verbose` | 布尔 | 关闭 | 输出调试日志。适用于全部命令。 |

日志写入 stderr。

启动时，napm 把上次进程退出时仍为 `running` 的执行记录标记为 `interrupted`。

### `napm version`

输出版本、提交、构建时间和 Go 版本。

## 环境变量

下列变量只在数据库里还没有任何管理员时读取。之后修改它们没有效果。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `NAPM_ADMIN_USER` | `admin` | 初始 root 管理员的用户名。 |
| `NAPM_ADMIN_PASSWORD` | 随机生成 | 初始 root 管理员的密码，至少 8 个字符。少于 8 个字符时 `napm serve` 报错并退出。未设置时，napm 生成随机密码，在 stderr 打印一次。 |

## 金额

| 项 | 规则 |
| --- | --- |
| 模式 `add` | 金额必须大于 0。 |
| 模式 `subtract` | 金额必须大于 0。 |
| 模式 `override` | 金额必须大于或等于 0。 |
| 单位 `usd` | 额度 = 金额 × `quota_per_unit`，四舍五入到整数。`quota_per_unit` 取自目标站点的 `/api/status`。 |
| 单位 `quota` | 金额即原始额度。 |
| 上限 | 换算后的额度大于 2^53 − 1（9007199254740991）时，整次执行失败。 |

## 定时表达式

napm 接受 `robfig/cron` 的标准写法，表达式不含秒字段：

- 5 段 cron：`<分> <时> <日> <月> <周>`。
- 预定义：`@hourly`、`@daily`、`@weekly`、`@monthly`、`@yearly`。
- 间隔：`@every <时长>`，时长的写法与 Go 的 `time.ParseDuration` 相同，如 `@every 90m`。
- 时区：默认使用运行 napm 的机器的本地时区。表达式开头可以加 `CRON_TZ=<时区> `，例如 `CRON_TZ=UTC 0 3 * * *`。

6 段（含秒）的表达式会被拒绝。

## 执行状态

| 状态 | 含义 |
| --- | --- |
| `running` | 正在执行。 |
| `ok` | 全部用户处理成功。刷新统计成功时也是 `ok`。 |
| `partial` | 至少一位用户成功，也至少一位用户失败。 |
| `failed` | 整次执行失败，或没有任何用户成功。 |
| `interrupted` | napm 在执行中途被停止，或启动时发现上次进程遗留的 `running` 记录。「成功/总数」是下限，见[使用手册](usage.md#调额执行状态是-interrupted)。 |
| `skipped` | 定时任务触发时，该平台已有任务在运行，这次触发被跳过。 |

一次执行最多保存 50 条错误，超出的部分以一行「另有 N 条错误」代替。

## HTTP 接口

网页通过这些接口工作。它们没有版本承诺，可能随版本变化。

### 通用规则

- 请求和响应的正文都是 JSON。请求正文不得超过 1 MiB。
- 除 `POST /api/login` 外，接口要求登录会话或 API 令牌。会话 Cookie 名为 `napm_session`，有效期 7 天，属性为 `HttpOnly` 和 `SameSite=Strict`；请求通过 HTTPS 到达 napm 时才带 `Secure`。API 令牌放在请求头 `Authorization: Bearer <令牌>` 里，见[API 令牌](#api-令牌)。请求带了 `Authorization` 头时只认令牌，令牌无效返回 `401`，不会退回去看 Cookie。
- 用会话的状态变更请求（非 `GET`）要求 `Content-Type: application/json`，且不能是跨源请求，否则返回 `415` 或 `403`。跨源由 Go 标准库的 `http.CrossOriginProtection` 判断：先看 `Sec-Fetch-Site`，浏览器没有发这个头时，再要求 `Origin` 的主机与 `Host` 相同。用令牌的请求不做这两项检查，因为令牌不会被浏览器自动带上。
- 失败时返回对应的 HTTP 状态码和 `{"error": "<原因>"}`。
- 带 `{id}` 的平台接口要求当前管理员被授权该平台，否则返回 `403`。

### 会话

| 方法与路径 | 权限 | 说明 |
| --- | --- | --- |
| `POST /api/login` | 无 | 请求 `{"username", "password"}`。成功时设置会话 Cookie。失败返回 `401`；被锁定返回 `429`。 |
| `POST /api/logout` | 无 | 删除当前会话。 |
| `GET /api/me` | 已登录 | 返回 `{"id", "username", "root"}`。未登录返回 `401`。 |

### 平台

| 方法与路径 | 权限 | 说明 |
| --- | --- | --- |
| `GET /api/platforms` | 已登录 | 返回当前管理员可见的平台，每项含 `id`、`name`、`base_url`。不返回令牌。 |
| `POST /api/platforms` | root | 添加平台。请求 `{"name", "base_url", "token"}`。URL 不是 `http(s)`、缺少主机或带用户名密码时返回 `400`。 |
| `PUT /api/platforms/{id}` | root | 修改平台。`token` 为空表示保持原值。 |
| `DELETE /api/platforms/{id}` | root | 删除平台及其统计、任务、记录和授权。 |

### 统计与用户

| 方法与路径 | 说明 |
| --- | --- |
| `GET /api/platforms/{id}/stats` | 返回 `{"stats", "last_run"}`。`stats` 为最近一次成功刷新的快照，从未刷新时为 `null`。`last_run` 为最近一次执行记录。 |
| `POST /api/platforms/{id}/refresh` | 开始刷新统计。返回 `{"run_id"}`。该平台已有任务在运行时返回 `409`。 |
| `GET /api/platforms/{id}/users` | 查询参数 `page`（默认 1）、`group`、`keyword`。每页 100 个用户，已排除目标站点中已删除的用户。目标站点出错时返回 `502`。 |

`stats` 的字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `at` | 整数 | 刷新完成时间，Unix 秒。 |
| `quota_per_unit` | 数值 | 目标站点的额度换算比。 |
| `users` | 整数 | 用户总数。 |
| `quota` | 整数 | 总余额，原始额度。 |
| `used` | 整数 | 总用量，原始额度。 |
| `requests` | 整数 | 总请求数。 |
| `groups` | 数组 | 按分组名升序，每项含 `name`、`users`、`quota`、`used`、`requests`。 |

### 调整余额

`POST /api/platforms/{id}/quota`

请求正文是一个调整计划：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `mode` | 字符串 | 是 | `add`、`subtract` 或 `override`。 |
| `amount` | 数值 | 是 | 金额，规则见[金额](#金额)。 |
| `unit` | 字符串 | 是 | `usd` 或 `quota`。 |
| `group` | 字符串 | 与 `user_ids` 二选一 | 分组名。 |
| `user_ids` | 整数数组 | 与 `group` 二选一 | 用户 ID。重复的 ID 只处理一次。 |

`group` 和 `user_ids` 必须且只能提供一个。参数无效返回 `400`；该平台已有任务在运行返回 `409`；成功开始返回 `{"run_id"}`。调整在后台执行。

示例：给分组 `vip` 的每位用户增加 1 美元。

```json
{"mode": "add", "amount": 1, "unit": "usd", "group": "vip"}
```

### 用量

| 方法与路径 | 说明 |
| --- | --- |
| `POST /api/platforms/{id}/usage/collect` | 开始采集用量，返回 `{"run_id"}`。需要平台令牌带 `log:read`。该平台已有任务在运行时返回 `409`。 |
| `GET /api/platforms/{id}/usage` | 返回已采集的用量，不做分析。 |

`GET` 的查询参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `from`、`to` | 30 天前、现在 | Unix 秒，区间为 `[from, to)`。 |
| `step` | `day` | `day` 把小时行合并成天，`hour` 返回小时行。`hour` 的区间最长 14 天。其他值返回 `400`。 |
| `tz` | `0` | 东区为正的分钟数，决定「天」从几点开始。例如东八区是 `480`。 |

返回 `{"state", "from", "to", "step", "models", "users"}`。`state` 为 `{"collected_to", "quota_per_unit"}`，从未采集时为 `null`。`models` 每项含 `t`（桶的起点，Unix 秒）、`model`、`count`（请求数）、`quota`（原始额度）、`tokens`；`users` 同理，把 `model` 换成 `username`。`quota` 除以 `quota_per_unit` 得金额。

new-api 在一个小时结束后还会继续累加它的计数，所以每次采集从上次的水位往前多取 2 小时，重复取到的小时覆盖旧值而不是相加。

### 定时任务与执行记录

| 方法与路径 | 说明 |
| --- | --- |
| `GET /api/platforms/{id}/jobs` | 列出该平台的任务。 |
| `POST /api/platforms/{id}/jobs` | 新增任务。 |
| `PUT /api/platforms/{id}/jobs/{job}` | 修改任务。 |
| `DELETE /api/platforms/{id}/jobs/{job}` | 删除任务。 |
| `GET /api/platforms/{id}/runs` | 返回最近 50 条执行记录，新的在前。 |

任务的字段：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `name` | 字符串 | 是 | 任务名称。 |
| `kind` | 字符串 | 是 | `refresh`、`usage` 或 `quota`。 |
| `spec` | 字符串 | 是 | 定时表达式，规则见[定时表达式](#定时表达式)。无效时返回 `400`。 |
| `enabled` | 布尔 | 否 | 是否启用。 |
| `plan` | 对象 | `kind` 为 `quota` 时必填 | 调整计划，字段同上。`kind` 为 `refresh` 或 `usage` 时忽略。 |

### 操作记录

| 方法与路径 | 权限 | 说明 |
| --- | --- | --- |
| `GET /api/audit` | 已登录 | 每次返回 200 条操作记录，新的在前。查询参数 `before` 为上一页最后一条的 `id`，返回比它更早的 200 条；不带则从最新开始。root 看到全部管理员的记录，其他管理员只看到自己的。 |

每条记录含 `id`、`at`（Unix 秒）、`admin_id`、`admin`、`platform_id`、`platform`、`action` 和 `detail`。`admin` 和 `platform` 是操作发生时的名称，之后即使账号或平台被删除，记录仍保留。管理员被删除后，他的记录只有 root 能看到，同名重新创建的账号看不到。`action` 取值：

| `action` | 触发时机 | `detail` 的内容 |
| --- | --- | --- |
| `login` | 登录成功 | 空对象。登录失败不记录。 |
| `refresh` | 手动开始刷新 | `run_id` |
| `usage.collect` | 手动开始采集用量 | `run_id` |
| `quota` | 手动开始调额 | `run_id` 和 `plan` |
| `job.create`、`job.update` | 新增、修改任务，包括启用或停用 | `name`、`kind`、`spec`、`enabled`、`plan` |
| `job.delete` | 删除任务 | `name` |
| `platform.create`、`platform.update` | 添加、修改平台 | `name`、`base_url`、`token_changed`。不含令牌本身。 |
| `platform.delete` | 删除平台 | `name` |
| `admin.create`、`admin.update` | 新增、修改管理员 | `username`、`root`、`read_only`、`platform_ids`，修改时另有 `password_changed`。不含密码。 |
| `admin.delete` | 删除管理员 | `username` |
| `token.create`、`token.delete` | 生成、删除 API 令牌 | `username`，即令牌所属的管理员 |

只有人通过网页或接口做的操作会记录。定时任务自己触发的执行不进操作记录，它们在执行记录里，来源为 `cron`。

### API 令牌

令牌让 MCP 客户端或脚本以某位管理员的身份调用 HTTP 接口。令牌的权限与该管理员完全相同：能访问哪些平台、是不是 root，都取决于管理员当前的设置，改了立即生效。

每个管理员最多有一个令牌。管理员可以给自己生成，root 也可以替任何管理员生成，root 能直接读到任何管理员的令牌，管理员只能读自己的。重新生成会替换旧令牌，旧令牌立即失效。令牌以 `napm_` 开头，后接 64 个十六进制字符，没有过期时间。napm 以明文保存令牌，和平台的访问令牌一样靠数据库文件的 `0600` 权限保护。管理员被删除、或管理员的密码被修改时，令牌随之删除。

下面三个令牌接口，以及新增、修改、删除管理员的 `POST /api/admins`、`PUT /api/admins/{id}`、`DELETE /api/admins/{id}`，只接受会话，用令牌调用返回 `403`。否则泄露的 root 令牌可以新建一个密码自己定的 root 管理员，或者换掉自己的令牌，删除原令牌也就挡不住它了。`GET /api/admins` 仍可用令牌读取，它只返回 `has_token`，不返回令牌本身。

| 方法与路径 | 说明 |
| --- | --- |
| `GET /api/admins/{id}/token` | 返回 `{"token"}`，没有令牌时为空字符串。自己的，或 root 读任何人的；别人的返回 `403`，管理员不存在返回 `404`。 |
| `POST /api/admins/{id}/token` | 生成新令牌并返回 `{"token"}`，已有的令牌作废。权限同上。 |
| `DELETE /api/admins/{id}/token` | 删除令牌，立即失效。权限同上。 |

生成和删除令牌会记入操作记录，动作为 `token.create` 和 `token.delete`，详情只含管理员的用户名。操作人是执行的人，所以 root 替别人生成时，记录里是 root。

### 管理员

以下接口都要求 root。

| 方法与路径 | 说明 |
| --- | --- |
| `GET /api/admins` | 列出管理员，每项含 `id`、`username`、`root`、`read_only`、`platform_ids`、`has_token`。 |
| `POST /api/admins` | 新增。请求 `{"username", "password", "root", "read_only", "platform_ids"}`。密码少于 8 个字符返回 `400`，`root` 与 `read_only` 同时为真也返回 `400`。 |
| `PUT /api/admins/{id}` | 修改。`password` 为空表示不改密码。修改密码或权限会使该管理员的会话失效。 |
| `DELETE /api/admins/{id}` | 删除。不能删除自己。 |

## MCP 接口

napm 在 `POST /mcp` 提供一个 [MCP](https://modelcontextprotocol.io) 服务，让 AI 客户端用工具管理平台。

### 连接方式

- 地址：`http(s)://<napm 地址>/mcp`。
- 认证：请求头 `Authorization: Bearer <API 令牌>`。只认令牌，不认会话 Cookie，所以浏览器里打开的页面无法驱动它。没有令牌或令牌无效返回 `401`。
- 实现：napm 使用官方 Go SDK [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) 的 Streamable HTTP 处理器，协议版本、请求头校验和错误码都由它负责。
- 传输：无状态模式。每个 `POST` 是一次独立的请求，napm 用一个 JSON 正文回答，不使用 `Mcp-Session-Id`。请求要带 `Content-Type: application/json`，`Accept` 要同时包含 `application/json` 和 `text/event-stream`，这是 SDK 的要求。无状态模式下 `GET /mcp` 和 `DELETE /mcp` 返回 `405`，napm 不推送服务端消息。
- 能力：只有工具，没有资源和提示。

### 工具

每个工具在进程内调用上表对应的 HTTP 接口，用的是调用者自己的令牌。所以授权检查、参数校验和操作记录与网页一致：成功的写操作会记录，读取和失败的请求不记录。工具的参数名与接口的 JSON 字段相同，`platform_id` 对应路径里的平台 `{id}`，`job_id` 对应任务 ID。

| 工具 | 作用 | 对应的 HTTP 接口 |
| --- | --- | --- |
| `list_platforms` | 列出你可管理的平台。 | `GET /api/platforms` |
| `get_stats` | 读取最近一次刷新得到的统计和最近一次执行。 | `GET /api/platforms/{id}/stats` |
| `refresh_stats` | 开始刷新统计，立即返回 `run_id`。 | `POST /api/platforms/{id}/refresh` |
| `list_users` | 分页列出平台用户，可按 `group`、`keyword` 过滤。 | `GET /api/platforms/{id}/users` |
| `adjust_quota` | 修改某些用户或整个分组的余额，立即返回 `run_id`。 | `POST /api/platforms/{id}/quota` |
| `get_usage` | 读取已采集的用量行（按模型、按用户，按天或按小时）。趋势、排行、异常等分析由调用方自己基于这些行计算。 | `GET /api/platforms/{id}/usage` |
| `collect_usage` | 开始采集平台用量，立即返回 `run_id`。 | `POST /api/platforms/{id}/usage/collect` |
| `list_runs` | 列出最近 50 次执行。 | `GET /api/platforms/{id}/runs` |
| `list_jobs`、`create_job`、`update_job`、`delete_job` | 管理定时任务。 | `/api/platforms/{id}/jobs` |
| `list_audit` | 读取操作记录，可用 `before` 翻页。 | `GET /api/audit` |

工具失败时，napm 返回正常的 JSON-RPC 结果，其中 `isError` 为 `true`，文本是接口返回的 `{"error": "..."}`，客户端和模型可以据此修正参数。错误信息是中文。

`adjust_quota` 会真实修改目标站点上的余额。`add` 和 `subtract` 不是幂等的，客户端重试会重复生效，需要幂等时用 `override`。

### 客户端配置示例

在网页的「令牌」页创建令牌，页面会同时给出配置。支持 HTTP 传输和自定义请求头的客户端，配置的形式如下：

```json
{
  "mcpServers": {
    "napm": {
      "type": "http",
      "url": "https://napm.example.com/mcp",
      "headers": { "Authorization": "Bearer napm_<令牌>" }
    }
  }
}
```

## 限制

| 项 | 值 |
| --- | --- |
| 登录失败锁定 | 同一客户端 IP 或同一用户名连续失败 5 次，锁定 1 分钟。 |
| 会话有效期 | 7 天，保存在内存中。 |
| 每页用户数 | 100，由 new-api 的上限决定。 |
| 请求间隔 | 默认 600 ms，对同一平台。new-api 默认每个 IP 每 180 秒允许 360 次请求。 |
| 读请求遇到 HTTP 429 | 按 `Retry-After` 等待（缺失或大于 300 秒时等待 10 秒），最多重试 3 次。写请求不重试。 |
| 并发 | 同一平台同一时间只运行一个任务。 |
| HTTP 超时 | 读请求头 10 秒，读整个请求 30 秒，写响应 2 分钟，空闲连接 2 分钟。 |
| 重定向 | napm 不跟随目标站点的重定向。 |

## 数据

SQLite 数据库包含下列表：

| 表 | 内容 |
| --- | --- |
| `admins` | 管理员。密码以 bcrypt 哈希保存，API 令牌明文保存。 |
| `platforms` | 平台，令牌以明文保存。 |
| `admin_platforms` | 管理员与平台的授权关系。 |
| `snapshots` | 每个平台最近一次统计快照。 |
| `usage_model`、`usage_user` | 采集到的小时级用量，按平台、小时和模型或用户各一行。 |
| `usage_state` | 每个平台用量采集到哪个时间，以及当时的 `quota_per_unit`。 |
| `jobs` | 定时任务。 |
| `runs` | 执行记录。 |
| `audit` | 操作记录。不随平台或管理员的删除而删除，也不会自动清理。 |

删除平台或管理员时，相关的授权、快照、任务和执行记录一并删除，操作记录保留。删除管理员时，他的 API 令牌随之删除。
