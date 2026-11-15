#!/usr/bin/env bash
# 构建编排器与 fake GitHub 二进制（e2e 用）。
#
# 用法: deploy/kagent/scripts/build.sh [输出目录]   （默认 /tmp/kagent-v1-e2e）
#
# 需要: go ≥1.27（go.mod 声明；宿主版本低时 GOTOOLCHAIN=auto 自动拉取）。
set -euo pipefail

WORK=${1:-/tmp/kagent-v1-e2e}
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$WORK"

cd "$REPO_ROOT/orchestrator"
go build -o "$WORK/orchestrator" ./cmd/orchestrator
go build -o "$WORK/fakegithub" ./cmd/fakegithub
echo "built: $WORK/orchestrator"
echo "built: $WORK/fakegithub"
