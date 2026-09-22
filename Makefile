# Xhunter 构建与验证入口。
#
# 一期目标平台 linux/amd64（纯 Go、无 cgo 依赖）。版本号经 -ldflags 注入
# cmd/xhunter 的 version 变量——它既是 `xhunter version` 的输出，也是发给上游的
# User-Agent 标识里的版本段；推导逻辑在 scripts/build.sh（命令行参数 > VERSION
# 环境变量 > git describe > "devel"）。
#
# 常用：
#   make build               本机构建 → bin/xhunter
#   make cross               交叉编译目标平台 linux/amd64 → bin/xhunter
#   make test / vet / check  单步验证（check 复用 scripts/check.py）
#   make clean               清理产物
#
# 显式版本号：make build VERSION=v0.1.0

GOOS       ?= linux
GOARCH     ?= amd64
CGO_ENABLED ?= 0
VERSION    ?=

.PHONY: all build cross test vet check clean

all: build

build:
	bash scripts/build.sh $(VERSION)

cross:
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) bash scripts/build.sh $(VERSION)

test:
	go test ./... -count=1

vet:
	go vet ./...

check:
	python scripts/check.py

clean:
	rm -rf bin
