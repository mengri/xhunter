#!/usr/bin/env bash
# 构建 xhunter 二进制，版本号经 -ldflags 注入 cmd/xhunter 的 version 变量。
#
# 用法：
#   scripts/build.sh                  本机平台构建 → bin/xhunter
#   scripts/build.sh v0.1.0           显式指定版本号
#   GOOS=linux GOARCH=amd64 scripts/build.sh   交叉编译（一期目标平台）
#
# 版本号优先级：命令行参数 > $VERSION 环境变量 > git describe > "devel"。
# 这个版本号同时决定两件事：`xhunter version` 的输出、以及发给上游的
# User-Agent 标识里的版本段（即客户端标识 xhunter/<version>）。
#
# 交叉编译通过 GOOS/GOARCH/CGO_ENABLED 环境变量透传给 go build（纯 Go、无 cgo
# 依赖，故交叉编译零成本）；脚本本身不预设平台，平台由调用方（如 Makefile）决定。
set -euo pipefail

cd "$(dirname "$0")/.."

version="${1:-${VERSION:-}}"
if [[ -z "$version" ]]; then
    version="$(git describe --tags --always --dirty 2>/dev/null || true)"
fi
version="${version:-devel}"

ldflags="-X xhunter/cmd/xhunter.version=${version}"
out="bin/xhunter"
platform="${GOOS:-$(go env GOOS)}/${GOARCH:-$(go env GOARCH)}"

mkdir -p bin
echo "构建 xhunter ${version}（${platform}）→ ${out}"
go build -trimpath -ldflags "$ldflags" -o "$out" ./cmd/xhunter
echo "完成：${out}"
