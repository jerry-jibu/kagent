#!/usr/bin/env bash
# e2e 运行器：预清理 → 端口转发 → V1 全链路验收 → 收尾。
#
# 用法: deploy/kagent/scripts/e2e.sh [工作目录]   （默认 /tmp/kagent-v1-e2e）
#
# 预清理的理由（每条都真实卡过）:
#   - 陈旧编排器进程持有 PG 实例锁 → 新编排器拒启动（instance lock held）
#   - 陈旧端口转发进程使连接时通时断（探针假失败）
#
# 端口转发在本脚本进程树内生命周期管理：跨工具调用边界后台进程会被回收，
# 所以转发必须与 e2e 脚本同一批启动、同一批收尾。
set -uo pipefail
export PATH="$HOME/.local/bin:$PATH"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

pkill -f 'orchestrator --database-url' 2>/dev/null; sleep 2
pkill -f 'port-forward' 2>/dev/null; sleep 1
kubectl -n kagent port-forward svc/kagent-controller 8083:8083 >/tmp/fw-gateway.log 2>&1 &
kubectl -n kagent port-forward svc/agentfs 9765:9765 >/tmp/fw-agentfs.log 2>&1 &
sleep 4
curl -s -m 3 -o /dev/null -w 'agentfs forward: %{http_code}\n' http://127.0.0.1:9765/healthz

bash "$SCRIPT_DIR/../e2e-v1.sh" "${1:-/tmp/kagent-v1-e2e}"
RC=$?
echo "=== e2e exit: $RC ==="
pkill -f 'orchestrator --database-url' 2>/dev/null
pkill -f 'port-forward' 2>/dev/null
exit $RC
