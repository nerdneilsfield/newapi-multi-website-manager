# napm

[English](README.en.md) | **中文**

napm 在一个网页里管理多个 [new-api](https://github.com/QuantumNous/new-api) 站点，面向同时运营多个 new-api 的管理员。它汇总各站点的用户数、分组、余额和用量，调整指定用户或整个分组的余额，并按计划定时执行这些操作。

## 特点

- 一个 Go 二进制，网页内嵌，数据存放在一个 SQLite 文件中，不依赖 CGO 和外部数据库。
- 每位管理员只能看到并操作被授权的平台。只读账号能看，不能改。
- 余额可以增加、扣减或设为固定值，金额可以按美元或原始额度输入。
- 定时任务支持刷新统计、采集用量和调整余额三种，使用 cron 表达式或 `@every` 间隔。
- 提供 MCP 接口，AI 客户端凭管理员的 API 令牌（`Authorization: Bearer`）管理平台，权限与该管理员相同，成功的写操作记入操作记录。
- 对同一平台一次只运行一个任务，并限制请求间隔，避免触发 new-api 的限流。

## 状态与限制

napm 尚未对真实的 new-api 站点做过验证，请先在测试站点上试用。

- 目标站点需要运行 new-api v1.0.0-rc.41 或更新版本。更早的版本没有个人访问令牌，也没有调整余额的接口。
- napm 只提供明文 HTTP，没有 TLS。详见[部署与安全](docs/deploy.md)。
- 平台令牌以明文存放在 SQLite 文件中。

## 通过 Homebrew / Scoop 安装

```bash
# macOS / Linux
brew install --cask nerdneilsfield/apps/napm
```

```powershell
# Windows
scoop bucket add nerdneilsfield https://github.com/nerdneilsfield/homebrew-apps
scoop install nerdneilsfield/napm
```

## 安装

需要 Go 1.27.1 或更新版本。

```bash
go install github.com/nerdneilsfield/newapi-multi-website-manager@latest
```

这条命令安装的可执行文件名为 `newapi-multi-website-manager`。如果要得到名为 `napm` 的文件，从源码构建：

```bash
git clone https://github.com/nerdneilsfield/newapi-multi-website-manager.git
cd newapi-multi-website-manager
make install
```

`make install` 把 `napm` 放入 `GOBIN`，未设置 `GOBIN` 时放入 `$(go env GOPATH)/bin`。也可以用 [Docker](docs/deploy.md#用-docker-运行) 运行。

## 快速开始

1. 在环境变量中设置初始 root 管理员的密码（至少 8 个字符），然后启动：

   ```bash
   NAPM_ADMIN_PASSWORD=<密码> napm serve --db napm.db
   ```

   终端输出一行 `listening`，其中 `addr` 为 `127.0.0.1:8080`。

2. 在浏览器中打开 `http://127.0.0.1:8080`，以用户名 `admin` 和上一步的密码登录。
3. 打开「平台管理」，填写名称、目标站点的 URL 和访问令牌，点击「添加」。令牌的创建方法见[准备访问令牌](docs/usage.md#准备访问令牌)。
4. 回到「统计」，点击「刷新统计」。刷新完成后，页面显示用户数、总余额、总用量、请求数和分组明细。

## 文档

- [使用手册](docs/usage.md)：创建令牌、授权管理员、调整余额、设置定时任务、排查问题。
- [参考](docs/reference.md)：命令行参数、环境变量、HTTP 接口、状态值和限制。
- [部署与安全](docs/deploy.md)：监听地址、反向代理、Docker、备份和安全边界。

## 开发

```bash
make check
```

`make check` 依次运行格式检查、lint、`go vet`、测试和 `goreleaser check`。`just` 提供同名目标。golangci-lint 和 goreleaser 需要另行安装；gofumpt、goimports 和 lefthook 是 `go.mod` 中的 `tool` 依赖。

Git 钩子检查暂存的 Go 文件格式，并要求提交信息符合 Conventional Commits。

发布时推送 `v*` 标签，由 GoReleaser 构建二进制和镜像。镜像只发布到 `ghcr.io/nerdneilsfield/napm`，用仓库自带的 `GITHUB_TOKEN` 认证，不需要额外配置密钥。

## 许可证

本项目使用 MIT 许可证，见 [LICENSE](LICENSE)。
