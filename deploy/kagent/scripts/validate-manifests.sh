#!/usr/bin/env bash
# manifest 校验冒烟（PR #11 review #1 的防复发闸）。
#
# 背景：orchestrator.yaml 曾把 `---` 文档分隔符写进 Deployment 中段——
# readinessProbe/volumeMounts/resources/volumes 落进了 NetworkPolicy 文档，
# YAML 也不报错，直到 kubectl apply 才炸。本脚本对 deploy/kagent/*.yaml
# 逐一做两层校验：
#   1. YAML 全文档解析（PyYAML 或 ruby，宿主谁在用谁；都没有则跳过这层）；
#   2. 集群可达时 kubectl apply --dry-run=server（真实 schema/CRD 校验，
#      占位符先代入 dummy——Harness CRD 对 image 有 sha256 钉死形态的
#      pattern，字面 ${DSH_BYO_IMAGE} 必挂；校验的是代入后的形状，与
#      部署时 envsubst 等价）。不可达时降级 kubectl apply --dry-run=client
#      --validate=false（纯本地结构解码，不连集群）。
#
# 两类"非清单结构问题"降级为 WARN（集群现网对象漂移/CRD apply 语义差异）：
#   - "field is immutable"（现网对象与清单的 selector 等不可变字段有漂移）
#   - "metadata.resourceVersion ... must be specified"（kapply 对带 status
#     子资源的 CRD 的 apply patch 怪癖）
# 其余 server 校验失败 = 真 FAIL（`---` 切坏文档正是以未知字段/类型错冒头）。
#
# 只读：dry-run 不落任何资源。deploy-local.sh 在 apply 前调用本脚本；
# orchestrator.yaml 仍不在 apply 序列里（e2e 以本地进程跑编排器），但校验覆盖它。
#
# 已知边界：--dry-run=server 对 api.kagent.dev 资源要求目标集群已装 kagent
# CRD——对着裸集群跑，Agent/Harness 几个文件会失败，属真实信号而非误报。
set -euo pipefail
export PATH="$HOME/.local/bin:$PATH"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEPLOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

FAILURES=0
ok()   { printf '  OK   %s\n' "$*"; }
warn() { printf '  WARN %s\n' "$*"; }
fail() { printf '  FAIL %s\n' "$*"; FAILURES=$((FAILURES+1)); }
step() { printf '\n=== %s ===\n' "$*"; }

# yaml_docs <file>: 解析全部文档并逐个打印 kind；解析失败非零退出。
yaml_docs() {
  local f=$1
  if command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' >/dev/null 2>&1; then
    python3 -c '
import sys, yaml
for d in yaml.safe_load_all(open(sys.argv[1])):
    if not isinstance(d, dict) or not d.get("kind"):
        raise SystemExit("doc without kind: %r" % (d,))
    print("    doc: kind=%s" % d["kind"])
' "$f"
  elif command -v ruby >/dev/null 2>&1; then
    ruby -ryaml -e '
YAML.load_stream(File.read(ARGV[0])).each do |d|
  abort "doc without kind" unless d.is_a?(Hash) && d["kind"]
  puts "    doc: kind=#{d["kind"]}"
end
' "$f"
  else
    echo "    （宿主无 python3+PyYAML/ruby，跳过解析层，交给 dry-run 兜底）"
  fi
}

# dry-run 工作区：占位符清单的代入副本（原文件不动）。
WORKDIR=$(mktemp -d /tmp/kagent-manifest-validate.XXXXXX)
trap 'rm -rf "$WORKDIR"' EXIT
DUMMY_IMAGE=localhost:5001/kagent-dev/validate-smoke@sha256:0000000000000000000000000000000000000000000000000000000000000000
dryrun_copy() {
  sed -e "s|\${DSH_BYO_IMAGE}|$DUMMY_IMAGE|g" \
      -e "s|\${ORCH_IMAGE}|$DUMMY_IMAGE|g" \
      -e "s|\${[A-Z_][A-Z_]*}|validate-smoke|g" \
      "$1" > "$WORKDIR/$(basename "$1")"
  printf '%s' "$WORKDIR/$(basename "$1")"
}

step "YAML 全文档解析（deploy/kagent/*.yaml）"
for f in "$DEPLOY_DIR"/*.yaml; do
  if out=$(yaml_docs "$f" 2>&1); then
    ok "$(basename "$f")"
    printf '%s\n' "$out"
  else
    fail "$(basename "$f") 解析失败"
    printf '%s\n' "$out" | sed 's/^/    /'
  fi
done

step "kubectl dry-run"
DRYRUN=server
if ! kubectl get namespace >/dev/null 2>&1; then
  DRYRUN=client
  echo "  集群不可达——降级 kubectl apply --dry-run=client --validate=false（本地结构解码）"
fi
for f in "$DEPLOY_DIR"/*.yaml; do
  copy=$(dryrun_copy "$f")
  if [ "$DRYRUN" = server ]; then
    if kubectl apply --dry-run=server -f "$copy" >"$WORKDIR/err.log" 2>&1; then
      ok "$(basename "$f") server dry-run"
    elif grep -qE 'field is immutable|metadata\.resourceVersion' "$WORKDIR/err.log"; then
      warn "$(basename "$f") server dry-run（现网对象漂移/CRD apply 语义差异，非清单结构问题）"
      sed 's/^/    /' "$WORKDIR/err.log" | grep -E 'Invalid|Error' | head -4 | sed 's/^/    /'
    else
      fail "$(basename "$f") server dry-run"
      sed 's/^/    /' "$WORKDIR/err.log" | head -8
    fi
  else
    if kubectl apply --dry-run=client --validate=false -f "$copy" >/dev/null 2>&1; then
      ok "$(basename "$f") client dry-run"
    else
      fail "$(basename "$f") client dry-run"
      kubectl apply --dry-run=client --validate=false -f "$copy" 2>&1 | sed 's/^/    /' | head -8 || true
    fi
  fi
done

step "结果"
if [ "$FAILURES" -eq 0 ]; then
  echo "manifest 校验：全部通过（WARN 为非结构性提示，见上）"
else
  echo "manifest 校验：$FAILURES 项失败（见上）"
fi
exit "$FAILURES"
