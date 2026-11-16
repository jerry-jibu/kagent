#!/usr/bin/env bash
# e2e 运行器：预清理 → 端口转发 → V1 全链路验收 → 收尾。
#
# 用法: deploy/kagent/scripts/e2e.sh [工作目录]   （默认 /tmp/kagent-v1-e2e）
#
# 预清理的理由（每条都真实卡过）:
#   - 陈旧编排器进程持有 PG 实例锁 → 新编排器拒启动（instance lock held）
#   - 陈旧端口转发进程使连接时通时断（探针假失败）——定向清理只杀监听本
#     套件端口的 kubectl port-forward；全机 pkill 的旧做法会误杀无关转发
#     （PR #11 review）
#
# 端口转发在本脚本进程树内生命周期管理：跨工具调用边界后台进程会被回收，
# 所以转发必须与 e2e 脚本同一批启动（$! 入 FW_PIDS）、同一批收尾。
set -uo pipefail
export PATH="$HOME/.local/bin:$PATH"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

pkill -f 'orchestrator --database-url' 2>/dev/null; sleep 2

# 本套件自己的转发用 PID 跟踪、只杀自己的（PR #11 review：pkill -f
# 'port-forward' 会把机器上所有 port-forward 一锅端——别的终端/别的项目的
# 转发也遭殃）。
FW_PIDS=()
start_forward() {   # start_forward <svc> <port> <logfile>
  kubectl -n kagent port-forward "svc/$1" "$2:$2" >"$3" 2>&1 &
  FW_PIDS+=("$!")
}
kill_own_forwards() {
  for pid in "${FW_PIDS[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null
  done
}
trap kill_own_forwards EXIT

# 崩溃残留的旧转发会占住端口（预清理原来靠全机 pkill 的理由）。定向清理：
# 只杀"kubectl port-forward 且监听本套件端口"的进程；清不掉的占用者报错
# 退出并给出排查命令，绝不盲杀。
for port in 8083 9765; do
  for pid in $(lsof -ti tcp:"$port" -sTCP:LISTEN 2>/dev/null || true); do
    if ps -p "$pid" -o args= 2>/dev/null | grep -q "port-forward.*$port:$port"; then
      kill "$pid" 2>/dev/null
    fi
  done
done
sleep 1
for port in 8083 9765; do
  if lsof -ti tcp:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "端口 $port 仍被非本套件进程占用：lsof -i tcp:$port 查占用者后手工处置" >&2
    exit 2
  fi
done

start_forward kagent-controller 8083 /tmp/fw-gateway.log
start_forward agentfs 9765 /tmp/fw-agentfs.log
sleep 4
curl -s -m 3 -o /dev/null -w 'agentfs forward: %{http_code}\n' http://127.0.0.1:9765/healthz

bash "$SCRIPT_DIR/../e2e-v1.sh" "${1:-/tmp/kagent-v1-e2e}"
RC=$?
echo "=== e2e exit: $RC ==="
kill_own_forwards
exit $RC
