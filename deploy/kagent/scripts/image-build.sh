#!/usr/bin/env bash
# 构建 kagent-bridge 镜像（真 agentfs 引擎 + 桥，双二进制）并按 digest 钉死
# deploy/kagent/agentfs.yaml。
#
# 用法: deploy/kagent/scripts/image-build.sh [agentfs 源码仓库路径]
#   默认 ~/src/github.com/jibudata/agent-data-workspace
#
# 坑位记录（每条都真实踩过）:
#   - buildx 的 COPY 层按内容哈希缓存——宿主二进制必须是 freshly built 的，
#     否则推上去的镜像是旧代码（曾致 3 轮 e2e 假失败）。
#   - 推送后必须 curl 校验 digest 可拉取（曾出现 buildx DONE 但 registry
#     未落盘；校验时 Accept 头必须带 OCI index 类型，否则 404 假象）。
#   - 镜像引用必须 digest 钉死: :tag 引用会被 kind 节点 containerd 的
#     IfNotPresent 缓存旧镜像。
set -euo pipefail

AGENTFS_SRC=${1:-$HOME/src/github.com/jibudata/agent-data-workspace}
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BRIDGE_DIR="$SCRIPT_DIR/../../kagent-bridge"
BUILDER=kagent-builder-v0.23.0
IMAGE=localhost:5001/kagent-dev/kagent-bridge

# 1. 双二进制（宿主构建；agentfs 引擎来自其源仓库）
echo "=== build agentfs engine (from $AGENTFS_SRC) ==="
( cd "$AGENTFS_SRC" && CGO_ENABLED=0 go build -o /tmp/agentfs ./cmd/agentfs )
echo "=== build kagent-bridge ==="
( cd "$BRIDGE_DIR" && CGO_ENABLED=0 go build -o /tmp/kagent-bridge . )

# 2. 构建并推送
cp /tmp/agentfs /tmp/kagent-bridge "$BRIDGE_DIR/"
( cd "$BRIDGE_DIR" && docker buildx build --push --platform linux/amd64 \
    --builder "$BUILDER" -t "$IMAGE:dev" . ) > /tmp/bridge-push.log 2>&1
rm -f "$BRIDGE_DIR/kagent-bridge" "$BRIDGE_DIR/agentfs"

# 3. 取 digest 并校验 registry 可服务（Accept 头必须带 OCI index 类型！）
NEWDIG=$(grep -oE 'sha256:[a-f0-9]{64}' /tmp/bridge-push.log | tail -1)
CODE=$(curl -s -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json' \
  "http://127.0.0.1:5001/v2/kagent-dev/kagent-bridge/manifests/$NEWDIG" -o /dev/null -w '%{http_code}')
if [ "$CODE" != "200" ]; then
  echo "FAIL: registry 不服务新 digest $NEWDIG (HTTP $CODE)——重推或查 registry 日志" >&2
  exit 1
fi
echo "pushed+verified: $IMAGE@$NEWDIG"

# 4. 钉死 agentfs.yaml 并生效（rollout restart 防止 containerd 缓存旧镜像）
sed -i "s|kagent-bridge@sha256:[a-f0-9]*|kagent-bridge@$NEWDIG|" "$SCRIPT_DIR/../agentfs.yaml"
grep -n 'image:' "$SCRIPT_DIR/../agentfs.yaml"
echo "下一步: kubectl apply -f deploy/kagent/agentfs.yaml && kubectl -n kagent rollout restart deploy/agentfs"
