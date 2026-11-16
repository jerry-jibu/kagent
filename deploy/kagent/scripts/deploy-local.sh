#!/usr/bin/env bash
# 部署 superagent-on-kagent 全套 manifest 到本地 kind 集群（e2e 形态）。
#
# 用法: deploy/kagent/scripts/deploy-local.sh
#
# 部署顺序（依赖序）:
#   校验冒烟 → agentfs（桥+引擎+入站策略）→ EgressPolicy → modelconfigs → harness×2 → agents
# orchestrator.yaml 本身不在本脚本部署（e2e 以本地进程跑编排器；集群部署
# 形态按 README 的 sed 流程）——但 validate-manifests.sh 的校验覆盖它
# （PR #11 review #1 的防复发闸：`---` 分隔符切坏文档这种错，apply 前先拦下）。
#
# 坑位记录:
#   - egressPolicyRefs 在 Agent spec 级（不在 template.inline）——放错层
#     会被 CEL 拒绝。
#   - evaluator 引用 kagent-harness（本集群 v1.0.0-rc3 无 1.0.1 的
#     kagent-default 内置 harness）。
#   - digest 钉死值以 agentfs.yaml 当前内容为准（image-build.sh 维护）。
set -euo pipefail
export PATH="$HOME/.local/bin:$PATH"
SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
DSH_BYO_IMAGE=${DSH_BYO_IMAGE:-localhost:5001/kagent-dev/dsh-byo@sha256:defb23dcb798627bb37fe0ad2a4c3c20d6dbfb9ba9dc7a7804eb71b422a0882d}
SNAPSHOT_LOCATION=${SNAPSHOT_LOCATION:-s3://ate-snapshots/kagent/}

echo "=== manifest 校验冒烟（validate-manifests.sh，含未部署的 orchestrator.yaml）==="
"$SCRIPT_DIR/scripts/validate-manifests.sh"

echo "=== agentfs 桥+引擎 ==="
kubectl apply -f "$SCRIPT_DIR/agentfs.yaml"

echo "=== EgressPolicy: worker 沙箱 → agentfs 出站授权 ==="
kubectl apply -f "$SCRIPT_DIR/egresspolicy-agentfs.yaml"

echo "=== NetworkPolicy: agentfs 入站默认拒绝（编排器 + ate-system worker 之外全拒）==="
kubectl apply -f "$SCRIPT_DIR/networkpolicy-agentfs.yaml"

echo "=== modelconfigs ==="
for f in modelconfig-deepseek modelconfig-deepseek-openai; do
  sed -e "s|\${DSH_BYO_IMAGE}|$DSH_BYO_IMAGE|" -e "s|\${SNAPSHOT_LOCATION}|$SNAPSHOT_LOCATION|" \
    "$SCRIPT_DIR/$f.yaml" | kubectl apply -f -
done

echo "=== harnesses ==="
kubectl apply -f "$SCRIPT_DIR/harness-kagent.yaml"
sed -e "s|\${DSH_BYO_IMAGE}|$DSH_BYO_IMAGE|" -e "s|\${SNAPSHOT_LOCATION}|$SNAPSHOT_LOCATION|" \
  "$SCRIPT_DIR/harness-dsh-byo.yaml" | kubectl apply -f -

echo "=== agents（等待 Ready，~40s）==="
sed -e "s|\${DSH_BYO_IMAGE}|$DSH_BYO_IMAGE|" "$SCRIPT_DIR/agent-worker.yaml" | kubectl apply -f -
sed -e "s|\${DSH_BYO_IMAGE}|$DSH_BYO_IMAGE|" "$SCRIPT_DIR/agent-evaluator.yaml" | kubectl apply -f -

DEADLINE=$(( $(date +%s) + 120 ))
while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  READY=$(kubectl -n kagent get agents superagent-worker superagent-evaluator \
    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' \
    2>/dev/null | grep -c True || true)
  [ "$READY" = "2" ] && break
  sleep 5
done
kubectl -n kagent get agents superagent-worker superagent-evaluator \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'

echo "=== 完成 ==="
echo "编排器以本地进程运行（scripts/e2e.sh 内部处理）；集群部署形态按 README 的 orchestrator.yaml sed 流程。"
