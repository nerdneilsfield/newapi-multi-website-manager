projectname := "go-template"
pkg := "github.com/nerdneilsfield/go-template"
version := `git describe --tags --always --dirty 2>/dev/null || echo dev`
commit := `git rev-parse --short HEAD 2>/dev/null || echo none`
date := `date -u +%Y-%m-%dT%H:%M:%SZ`
ldflags := "-X {{pkg}}/internal/buildinfo.Version={{version}} -X {{pkg}}/internal/buildinfo.Commit={{commit}} -X {{pkg}}/internal/buildinfo.Date={{date}}"

# 列出所有可用的命令
default:
    @just --list

# 构建 Golang 二进制文件
build:
    CGO_ENABLED=0 go build -ldflags '{{ldflags}}' -o {{projectname}} .

# 安装 Golang 二进制文件
install:
    go install -ldflags '{{ldflags}}' .

# 运行应用程序
run:
    go run -ldflags '{{ldflags}}' .

# 运行测试并显示覆盖率
test: clean
    CGO_ENABLED=0 go test --cover -parallel=1 -v -coverprofile=coverage.out ./...
    go tool cover -func=coverage.out | sort -rnk3

# 清理环境
clean:
    rm -rf coverage.out dist {{projectname}} {{projectname}}.exe

# 显示测试覆盖率
cover:
    CGO_ENABLED=0 go test -v -race ./... -coverprofile=coverage.out
    go tool cover -func=coverage.out

# 格式化 Go 文件
fmt:
    go tool gofumpt -l -w .
    go tool goimports -w -local {{pkg}} .
    gofmt -w .

# 检查 Go 文件格式
fmt-check:
    test -z "$(go tool gofumpt -l .)" || { echo "gofumpt: files need formatting"; exit 1; }
    test -z "$(go tool goimports -l -local {{pkg}} .)" || { echo "goimports: files need formatting"; exit 1; }
    test -z "$(gofmt -l .)" || { echo "gofmt: files need formatting"; exit 1; }

# 安装 Git hooks
hooks:
    go tool lefthook install

# 运行 go vet
vet:
    go vet ./...

# 运行 linter
lint:
    golangci-lint run -c .golangci.yml

# 运行全部检查（fmt、lint、vet、test、release config）
check: fmt-check lint vet test release-check

# 校验 goreleaser 配置
release-check:
    goreleaser check

# 测试发布
release-test:
    goreleaser release --snapshot --clean
