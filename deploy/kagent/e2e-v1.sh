#!/usr/bin/env bash
# e2e-v1.sh — superagent-on-kagent P0 V1 全链路验收脚本（docs/designs/0003 V1）。
#
# 链路（0002 §Overview 时序 ①–⑦）：
#   ① issue @agent（fake GitHub 种子评论）
#   ② Source 轮询摄入 → Gate（本地 policy）
#   ③ evaluator invoke（真 A2A Gateway → kagent harness agent → DeepSeek）→ accept
#   ④ Engine 建单 + 确认评论（fake GitHub 收到 ack）
#   ⑤ 物化器浅克隆 bare git 仓 → agentfs 工作区（accept 事务提交后、派工前）
#   ⑥ 派工（真 Gateway → DSH BYO worker actor，gVisor 沙箱）→ worker 改文件 → Report ready
#   ⑦ 编排器取回工作区状态 → 计算 patch → push 分支 → 建 PR → 结果评论
#
# GitHub 面全程用 fake（可重复）；真 GitHub 走查需用户 token，是协调者步骤（README.md）。
# 编排器以本地进程运行（git 走 file:// 本地 bare 仓），worker actor / Gateway / agentfs
# 全部跑在 kind 集群里。前置：CUJ1 完成（superagent-worker Ready）、t1-pg 运行中。
#
# 用法：deploy/kagent/e2e-v1.sh [工作目录]   （默认 /tmp/kagent-v1-e2e）
set -euo pipefail

WORK=${1:-/tmp/kagent-v1-e2e}
REPO=acme/api
ISSUE=7
SENDER=maintainer-login
COMMENT_ID=900001
GATEWAY=http://localhost:8083
AGENTFS=http://localhost:9765/mcp
PG_URL='postgres://postgres:p0@localhost:55432/postgres?sslmode=disable'
SCHEMA=v1e2e
ORC_URL=http://localhost:8080
HOST_DIAL=192.168.5.2   # pod → 宿主编排器 push 回调（probe 可达；失败由 Pass 对账兜底）
TIMEOUT_CHAIN=900       # 全链路超时（秒）

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ORC_BIN="$WORK/orchestrator"
FAILURES=0

step() { printf '\n===== %s =====\n' "$*"; }
ok()   { printf '  OK  %s\n' "$*"; }
fail() { printf '  FAIL %s\n' "$*"; FAILURES=$((FAILURES+1)); }

step "0. 准备工作区与本地 bare git 仓（编排器物化/交付的 clone/push 对象）"
rm -rf "$WORK"; mkdir -p "$WORK/policy" "$WORK/gitbase/$REPO.git" "$WORK/logs"
WORK=$WORK   # normalize
export PATH="$HOME/sdk/go1.27.3/bin:$PATH"
command -v git >/dev/null || { echo "git required on PATH"; exit 2; }
git init --bare -q -b main "$WORK/gitbase/$REPO.git"
git -C "$WORK/gitbase/$REPO.git" config uploadpack.allowAnySHA1InWant true
git -C "$WORK/gitbase/$REPO.git" config uploadpack.allowReachableSHA1InWant true
SEED="$WORK/seed"
git init -q -b main "$SEED"
cat > "$SEED/README.md" <<'MD'
# demo repo
MD
mkdir -p "$SEED/src"
cat > "$SEED/src/greeting.go" <<'GO'
package main

import "fmt"

func greeting() string {
	return "hello"
}

func main() { fmt.Println(greeting()) }
GO
git -C "$SEED" add . && git -C "$SEED" -c user.email=seed@test -c user.name=seed commit -qm "initial"
git -C "$SEED" push -q "$WORK/gitbase/$REPO.git" main
BASE_COMMIT=$(git -C "$SEED" rev-parse main)
ok "bare repo seeded at $WORK/gitbase/$REPO.git (main @ $BASE_COMMIT)"

step "1. 编排器 policy（Gate 名单）"
cat > "$WORK/policy/repos.json" <<EOF
{"repositories": ["$REPO"]}
EOF
cat > "$WORK/policy/admission.json" <<EOF
{"senders": [{"id": "$SENDER", "strength": "explicit"}]}
EOF
cat > "$WORK/policy/bots.json" <<'EOF'
{"bots": ["kagent-bot[bot]"]}
EOF
ok "policy written"

step "2. 构建编排器与 fake GitHub"
cd "$REPO_ROOT/orchestrator"
go build -o "$ORC_BIN" ./cmd/orchestrator
go build -o "$WORK/fakegithub" ./cmd/fakegithub
ok "binaries built"

step "3. 启动 fake GitHub（种子：issue @$ISSUE 评论 @agent）"
REQUEST_BODY="@kagent-bot[bot] please change src/greeting.go so greeting() returns \"hello, superagent\" instead of \"hello\""
"$WORK/fakegithub" --repo "$REPO" --issue "$ISSUE" --author "$SENDER" \
  --comment-id "$COMMENT_ID" --body "$REQUEST_BODY" --expect-token e2e-dummy \
  > "$WORK/logs/fake.out" 2> "$WORK/logs/fake.err" &
FAKE_PID=$!
FAKE_URL=""
for _ in $(seq 1 20); do
  FAKE_URL=$( { grep -oE 'http://(127\.0\.0\.1|\[::1\]):[0-9]+' "$WORK/logs/fake.out" 2>/dev/null || true; } | head -1)
  [ -n "$FAKE_URL" ] && break
  sleep 0.5
done
[ -n "$FAKE_URL" ] || { cat "$WORK/logs/fake.out" "$WORK/logs/fake.err"; exit 2; }
ok "fake github at $FAKE_URL"

step "4. 启动编排器（本地进程；真 Gateway/agentfs/worker actor 在集群）"
# 上一次运行的判定是终态（trigger id 确定）；重跑必须从全新 schema 开始
docker exec t1-pg psql -U postgres -d postgres -c "DROP SCHEMA IF EXISTS $SCHEMA CASCADE" >/dev/null
export DATABASE_URL="$PG_URL"
"$ORC_BIN" \
  --database-url "$PG_URL" \
  --schema "$SCHEMA" \
  --policy-dir "$WORK/policy" \
  --listen-addr ":8080" \
  --github-source "$REPO" \
  --github-api-url "$FAKE_URL" \
  --github-poll-interval 5s \
  --github-token e2e-dummy \
  --a2a-gateway-url "$GATEWAY" \
  --evaluator-agent superagent-evaluator \
  --worker-agent superagent-worker \
  --push-base-url "http://$HOST_DIAL:8080" \
  --pass-interval 2s \
  --workspace-mcp-endpoint "$AGENTFS" \
  --workspace-base-branch main \
  --github-git-url "file://$WORK/gitbase" \
  > "$WORK/logs/orchestrator.out" 2> "$WORK/logs/orchestrator.err" &
ORC_PID=$!
sleep 2
curl -sf "$ORC_URL/healthz" >/dev/null && ok "orchestrator healthz green" || { echo "orchestrator not healthy"; tail "$WORK/logs/orchestrator.err"; exit 2; }

cleanup() { kill "$ORC_PID" "$FAKE_PID" 2>/dev/null || true; }
trap cleanup EXIT

step "5. 等待全链路（确认评论 → 物化基线 → 派工 → worker ready → PR → 结果评论）"
deadline=$(( $(date +%s) + TIMEOUT_CHAIN ))
pr_line=""
ack_line=""
result_line=""
while [ "$(date +%s)" -lt "$deadline" ]; do
  ack_line=$(grep -c "posted comment" "$WORK/logs/fake.err" 2>/dev/null || true)
  pr_line=$(grep "pull request" "$WORK/logs/fake.err" 2>/dev/null | tail -1 || true)
  result_line=$( { grep "superagent:effect:result:" "$WORK/logs/fake.err" 2>/dev/null || true; } | tail -1)
  if [ -n "$pr_line" ] && [ -n "$result_line" ]; then
    break
  fi
  sleep 5
done

grep -q "posted comment" "$WORK/logs/fake.err" && ok "确认评论已投递（fake GitHub 收到 ack）" || fail "确认评论未出现"
[ -n "$pr_line" ] && ok "PR 建立：$pr_line" || fail "PR 未建立"
[ -n "$result_line" ] && ok "结果评论已投递（含 PR 链接与 marker）" || fail "结果评论未出现"

step "6. 编排器状态断言（store 是权威视图）"
psqlq() { docker exec t1-pg psql -U postgres -d postgres -Atc "$1" 2>/dev/null; }
TICKET_ID=$(psqlq "SELECT id FROM \"$SCHEMA\".tickets ORDER BY created_at LIMIT 1")
if [ -n "$TICKET_ID" ]; then
  ok "ticket 建立并发派工：$TICKET_ID"
  STATUS=$(psqlq "SELECT status FROM \"$SCHEMA\".tickets WHERE id='$TICKET_ID'")
  DELIVERY=$(psqlq "SELECT delivery_state FROM \"$SCHEMA\".tickets WHERE id='$TICKET_ID'")
  BASELINE=$(psqlq "SELECT base_commit FROM \"$SCHEMA\".tickets WHERE id='$TICKET_ID'")
  TASKID=$(psqlq "SELECT task_id FROM \"$SCHEMA\".assignments WHERE ticket_id='$TICKET_ID' AND task_id IS NOT NULL LIMIT 1")
  [ "$STATUS" = "ended" ] && ok "ticket 状态 = ended（worker ready 后交付完成）" || fail "ticket 状态 = $STATUS（want ended）"
  [ "$DELIVERY" = "delivered" ] && ok "delivery_state = delivered" || fail "delivery_state = $DELIVERY（want delivered）"
  [ -n "$BASELINE" ] && [ "$BASELINE" != "" ] && ok "物化基线已落库（accept 后、派工前）：$BASELINE" || fail "无物化基线"
  [ -n "$TASKID" ] && ok "assignment 持有真 Gateway 派发的 task id：$TASKID" || fail "assignment 无 task id"
else
  fail "store 里没有 ticket（psql 查询失败或链路未推进）"
fi

step "7. 交付物断言：patch 内容 = worker 改动（编排器侧 diff，0003 Alternative E）"
DELIVERY_BRANCH="$TICKET_ID"
CHANGED=$(git -C "$WORK/gitbase/$REPO.git" show "refs/heads/$DELIVERY_BRANCH:src/greeting.go" 2>/dev/null | grep -c "hello, superagent" || true)
[ "$CHANGED" -ge 1 ] && ok "ticket 分支包含 worker 的改动（greeting → hello, superagent）" || fail "交付分支不含 worker 改动"
PR_BODY=$(curl -s "$FAKE_URL/repos/$REPO/pulls?state=all" | head -c 2000)
echo "$PR_BODY" | grep -q "\"head\"" && ok "fake GitHub PR 可查（head/base 齐全）" || fail "PR 查询失败"

step "8. worker 沙箱 egress 白名单断言（零 github.com 出站）"
# egress 是编译期白名单（byo compiler.go:77-79）：worker 的出站目的地 = 模型 provider
# + control plane + agentfs MCP，github.com 不在名单内。运行时证据从 egress 网关
# （atenet-egress，worker 唯一出站路径）的 DNS/连接日志读取：
#   ① api.deepseek.com 有命中 → worker 流量确实经过 egress（断言非空转）；
#   ② github.com 零命中 → 沙箱全程无 github.com 出站。
GATEWAY_SINCE=${GATEWAY_SINCE:-10m}
DEEPSEEK_HITS=$(kubectl -n ate-system logs deploy/atenet-egress --since="$GATEWAY_SINCE" 2>/dev/null | grep -ci "api.deepseek.com" || true)
# github.com 零命中 = 零真实出站：需排除 DeepSeek CLI 的 User-Agent 串
# （"(+https://github.com/deepseek-ai/deepseek-harness)" 随每个模型请求出现在日志里）。
# 任何非 deepseek 行出现 github.com = 真实出站（请求 URI/host），判失败。
GITHUB_HITS=$(kubectl -n ate-system logs deploy/atenet-egress --since="$GATEWAY_SINCE" 2>/dev/null | grep -i "github.com" | grep -vicE "deepseek" || true)
[ "$DEEPSEEK_HITS" -ge 1 ] && ok "egress 网关有 DeepSeek 命中（$DEEPSEEK_HITS 次）——worker 流量经 egress，断言非空转" || fail "egress 网关无 DeepSeek 命中（流量未经过网关？）"
[ "$GITHUB_HITS" = "0" ] && ok "egress 网关零 github.com 命中（近 ${GATEWAY_SINCE}）——沙箱无 github.com 出站" || fail "egress 网关出现 github.com 命中 $GITHUB_HITS 次"
# 编排器凭证单调性佐证：worker env 只含模型凭证与平台注入（atelet Run 日志），无 GitHub token
WORKER_ENV=$(kubectl -n ate-system logs ds/atelet --since="$GATEWAY_SINCE" 2>/dev/null | grep '"method":"/atelet.Atelet/Run"' | grep superagent-worker | tail -1 | grep -c "GITHUB_TOKEN\|github" || true)
[ "$WORKER_ENV" = "0" ] && ok "worker Run 请求 env 无 GitHub 凭证（凭证单调，编排器持唯一凭证）" || fail "worker env 出现 github 相关凭证"

step "9. 结果评论内容"
grep "posted comment" "$WORK/logs/fake.err" | sed 's/^/    /' | tail -3

step "结果"
if [ "$FAILURES" -eq 0 ]; then
  echo "V1 全链路验收：全部通过"
else
  echo "V1 全链路验收：$FAILURES 项失败（见上）"
  echo "编排器日志：$WORK/logs/orchestrator.err"
  echo "fake 日志：$WORK/logs/fake.err"
fi
exit "$FAILURES"
