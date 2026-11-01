projectname := napm
PKG := github.com/nerdneilsfield/newapi-multi-website-manager
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X $(PKG)/internal/buildinfo.Version=$(VERSION) -X $(PKG)/internal/buildinfo.Commit=$(COMMIT) -X $(PKG)/internal/buildinfo.Date=$(DATE)

.DEFAULT_GOAL := help
.PHONY: help build install run test clean cover fmt fmt-check vet lint check release-check release-test hooks

help: ## list makefile targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

build: ## build golang binary
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(projectname) .

# go install would name the binary after the module path, so build it by name.
install: ## install the app
	dir=$$(go env GOBIN); dir=$${dir:-$$(go env GOPATH)/bin}; go build -ldflags '$(LDFLAGS)' -o "$$dir/$(projectname)" .

run: ## run the app
	go run -ldflags '$(LDFLAGS)' .

test: clean ## display test coverage
	CGO_ENABLED=0 go test --cover -parallel=1 -v -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | sort -rnk3

clean: ## clean up environment
	rm -rf coverage.out dist/ $(projectname) $(projectname).exe

cover: ## display test coverage
	CGO_ENABLED=0 go test -v -race ./... -coverprofile=coverage.out
	go tool cover -func=coverage.out

fmt: ## format go files
	go tool gofumpt -l -w .
	go tool goimports -w -local $(PKG) .
	gofmt -w .

fmt-check: ## check go formatting
	@test -z "$$(go tool gofumpt -l .)" || { echo "gofumpt: files need formatting"; exit 1; }
	@test -z "$$(go tool goimports -l -local $(PKG) .)" || { echo "goimports: files need formatting"; exit 1; }
	@test -z "$$(gofmt -l .)" || { echo "gofmt: files need formatting"; exit 1; }

hooks: ## install git hooks
	go tool lefthook install

vet: ## run go vet
	go vet ./...

lint: ## run golangci-lint
	golangci-lint run -c .golangci.yml

check: fmt-check lint vet test release-check ## run all checks

release-check: ## validate goreleaser config
	goreleaser check

release-test: ## test release
	goreleaser release --snapshot --clean
