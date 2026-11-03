# 部署与安全

[English](deploy.en.md) | **中文**

本页说明如何在服务器上运行 napm，以及它的安全边界。

## 安全边界

部署前先了解下面几点，它们决定了 napm 能放在什么网络环境里。

- napm 只提供明文 HTTP，没有 TLS。登录密码和会话 Cookie 以明文在网络上传输。只在本机监听，或者放在会终止 TLS 的反向代理后面。
- 平台的访问令牌以明文保存在 SQLite 文件中，文件权限为 `0600`。能读到这个文件的人就能用这些令牌调整目标站点的余额。
- 访问令牌拥有 `user:write` 权限。持有者可以修改目标站点上任何用户的余额。
- 会话保存在内存中，napm 按单实例设计。不要同时运行多个共用同一份数据库的 napm 进程。
- napm 不限制可添加的平台地址，包括内网地址，因为自建的 new-api 通常就在内网。只有 root 管理员能添加平台，请只把 root 交给你信任的人。
- 操作记录会记下谁在什么时候做了什么，但不保存每位用户调整前后的余额。执行记录只保存时间、来源、状态和错误。
- API 令牌（用于 MCP）与它所属的管理员权限相同，root 的令牌能调整所有平台的余额。napm 以明文保存令牌，靠数据库文件的 `0600` 权限保护，root 能在网页里看到所有管理员的令牌。令牌没有过期时间，泄露后要在网页里重新生成或删除。令牌通过 `Authorization` 头明文传输，所以 `/mcp` 同样要放在 TLS 之后。

napm 已有的防护：

| 威胁 | 做法 |
| --- | --- |
| 猜测密码 | 同一客户端 IP 或同一用户名连续失败 5 次，各锁定 1 分钟。 |
| 跨站请求 | 状态变更请求必须是 JSON，并通过 Go 标准库 `http.CrossOriginProtection` 的检查（`Sec-Fetch-Site`，或 `Origin` 与 `Host` 比较）；会话 Cookie 带 `SameSite=Strict`。 |
| 令牌泄露给其他主机 | napm 不跟随目标站点的重定向。 |
| 越权 | 普通管理员访问未授权的平台时返回 `403`。 |
| 点击劫持与内容嗅探 | 响应带 `X-Frame-Options: DENY`、`X-Content-Type-Options: nosniff` 和 `Referrer-Policy: no-referrer`。 |

### 在反向代理后面运行时的客户端 IP

登录失败的锁定按连接的来源地址和用户名分别计数，任一计数达到 5 次就拒绝该请求的登录。napm 不读取 `X-Forwarded-For`。放在反向代理后面时，所有请求的来源地址都是代理的地址，所以任何人连续输错 5 次密码，都会让所有人在 1 分钟内无法登录。攻击者每分钟发送 5 次错误登录，就能持续阻止所有管理员登录。

如果 napm 要暴露给不可信的网络，在反向代理处限制登录接口 `POST /api/login` 的请求频率，或只允许可信的来源地址访问 napm。

### Cookie 的 `Secure` 属性

napm 只在自己收到 TLS 连接时才给会话 Cookie 加 `Secure`。放在终止 TLS 的反向代理后面时，napm 看到的是明文连接，所以不加 `Secure`。需要时，在反向代理处给响应的 `Set-Cookie` 补上 `Secure`。

## 在服务器上运行

前提：

- 一台能访问所有目标站点的机器。
- 一个 napm 可以写入的数据目录。

步骤：

1. 构建并安装 `napm`，见 [README](../README.md#安装)。
2. 创建专用的数据目录，例如 `/var/lib/napm`，仅允许运行 napm 的用户访问。
3. 首次启动时设置初始密码：

   ```bash
   NAPM_ADMIN_PASSWORD=<密码> napm serve --db /var/lib/napm/napm.db
   ```

4. 登录后，在「管理员」页修改密码或创建其他管理员。

初始密码只在数据库没有任何管理员时使用。之后启动不再需要这个环境变量。

### 作为 systemd 服务

下面的单元文件以用户 `napm` 运行，并把数据放在 `/var/lib/napm`。首次启动前，把初始密码写入 `/etc/napm/env`，内容为 `NAPM_ADMIN_PASSWORD=<密码>`，权限设为 `0600`。

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

napm 收到 `SIGTERM` 后停止接受新请求。正在执行的调整任务在这一刻被中断，状态变为 `interrupted`，见[使用手册](usage.md#调额执行状态是-interrupted)。因此，停止服务前先在「执行记录」页确认没有正在运行的调额任务。

### 反向代理

napm 的监听地址保持 `127.0.0.1:8080`，由反向代理对外提供 HTTPS。以 Caddy 为例：

```text
napm.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

反向代理需要保留 `Host` 头，并把浏览器的 `Origin` 头原样转发。浏览器不带 `Sec-Fetch-Site` 时，napm 比较 `Origin` 的主机和 `Host`，两者不一致就拒绝状态变更请求。

## 用 Docker 运行

镜像以非 root 用户（UID 65532）运行，工作目录和数据卷为 `/data`，默认命令为：

```text
serve --addr 0.0.0.0:8080 --db /data/napm.db
```

容器内监听 `0.0.0.0` 是容器的常规做法。对外暴露由 `docker run` 的 `-p` 参数控制，所以把端口只发布到本机，再由反向代理转发：

```bash
docker run -d --name napm \
  -p 127.0.0.1:8080:8080 \
  -v napm-data:/data \
  -e NAPM_ADMIN_PASSWORD=<密码> \
  ghcr.io/nerdneilsfield/napm:latest
```

要点：

- 不要写成 `-p 8080:8080`，它会把明文 HTTP 发布到宿主机的所有网卡。
- 不设置 `NAPM_ADMIN_PASSWORD` 时，初始随机密码只出现在容器日志中，用 `docker logs napm` 查看。
- 把宿主机目录挂载到 `/data` 时，该目录必须允许 UID 65532 写入。使用命名卷（如上例的 `napm-data`）不需要额外设置。

用 Docker Compose 时，把下面的内容存为 `docker-compose.yml`，在同一目录运行 `docker compose up -d`：

```yaml
services:
  napm:
    image: ghcr.io/nerdneilsfield/napm:latest
    container_name: napm
    restart: unless-stopped
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - napm-data:/data
    env_file: napm.env

volumes:
  napm-data:
```

`napm.env` 里写 `NAPM_ADMIN_PASSWORD=<密码>`，权限设为 `0600`。初始密码只在第一次创建管理员时使用，所以这个文件在首次启动后就能删掉；把它单独放而不是直接写进 `docker-compose.yml`，是因为 compose 文件常被提交到仓库。

`restart: unless-stopped` 会在宿主机重启后自动拉起容器，适合配合反向代理长期运行。停容器时 `docker compose stop` 发送 `SIGTERM`，正在执行的调额任务会被中断，这一点与直接运行二进制相同。

镜像尚未在作者的机器上构建并运行验证过。

## 备份与恢复

数据库使用 WAL 模式，运行时除 `napm.db` 外还有 `napm.db-wal` 和 `napm.db-shm`。直接复制 `napm.db` 可能漏掉尚未写回的数据。

备份：

1. 停止 napm，等待 `napm.db-wal` 消失，然后复制 `napm.db`。
2. 或在 napm 运行时使用 SQLite 的在线备份命令：

   ```bash
   sqlite3 /var/lib/napm/napm.db ".backup '/backup/napm.db'"
   ```

备份文件含有明文令牌，按密钥对待：限制访问权限，不要放进公开或共享的存储。

恢复：停止 napm，用备份文件替换数据库文件，删除旧的 `-wal` 和 `-shm` 文件，再启动。

## 升级

1. 停止 napm。确认没有正在运行的调额任务。
2. 备份数据库。
3. 替换二进制或镜像，再启动。

napm 启动时用 [goose](https://github.com/pressly/goose) 把数据库升到当前版本，已有数据保留。版本记录在表 `goose_db_version` 中。迁移不能回退，所以升级前的备份是回退的唯一办法。
