#!/usr/bin/env bash
# superagent e2e 环境准备与体检（PR #11）。
#
# 用法: deploy/kagent/scripts/env-setup.sh
#
# 做的事（幂等，可重复跑）:
#   1. kind 集群 kagent 可用性检查（宕机则提示启动命令）
#   2. t1-pg 测试 PG 容器（55432，编排器 e2e 的 DATABASE_URL；缺则启动）
#   3. kagent-deepseek Secret（从集群既有 deepseek-api-key 复制；缺则报错）
#   4. controller 配置: push.allowHttp/allowPrivateNetworks = true
#      （编排器 push 回调是集群私网 HTTP——P0 设计形态；缺配置派发会被拒）
#   5. DeepSeek API 余额连通性（402 = 账户欠费，e2e 必挂）
#
# 不做的事: 镜像构建（build-images.sh）、manifest 部署（deploy-local.sh）、
# 端口转发（e2e.sh 运行期自管）。
set -euo pipefail
export PATH="$HOME/.local/bin:$PATH"

FAILURES=0
ok()   { printf '  OK   %s\n' "$*"; }
fail() { printf '  FAIL %s\n' "$*"; FAILURES=$((FAILURES+1)); }
step() { printf '\n=== %s ===\n' "$*"; }

step "1. kind 集群 kagent"
if ! kubectl -n kagent get deploy kagent-controller >/dev/null 2>&1; then
  fail "集群不可达或 kagent 未安装（docker start kagent-control-plane 后重试）"
else
  ok "集群可达，controller 在位"
fi

step "2. t1-pg 测试 PG（编排器 e2e 的 DATABASE_URL，端口 55432）"
if ! docker ps --format '{{.Names}}' | grep -qx t1-pg; then
  echo "  t1-pg 不在运行——启动新容器"
  docker run -d --name t1-pg -e POSTGRES_PASSWORD=p0 -e POSTGRES_USER=postgres \
    -p 127.0.0.1:55432:5432 postgres:18-alpine >/dev/null
  sleep 4
fi
docker exec t1-pg pg_isready -U postgres >/dev/null 2>&1 \
  && ok "t1-pg accepting connections (localhost:55432)" \
  || fail "t1-pg 未就绪"

step "3. Secret kagent-deepseek（evaluator/worker 模型凭证）"
if kubectl -n kagent get secret kagent-deepseek >/dev/null 2>&1; then
  ok "secret 已存在"
elif kubectl -n kagent get secret deepseek-api-key >/dev/null 2>&1; then
  kubectl -n kagent create secret generic kagent-deepseek \
    --from-literal=ANTHROPIC_API_KEY="$(kubectl -n kagent get secret deepseek-api-key -o jsonpath='{.data.ANTHROPIC_API_KEY}' | base64 -d)" \
    --from-literal=OPENAI_API_KEY="$(kubectl -n kagent get secret deepseek-api-key -o jsonpath='{.data.OPENAI_API_KEY}' | base64 -d)" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  ok "从 deepseek-api-key 复制创建"
else
  fail "无 deepseek-api-key 源 Secret——手工创建 kagent-deepseek（ANTHROPIC_API_KEY/OPENAI_API_KEY）"
fi

step "4. controller 配置: push.allowHttp + allowPrivateNetworks"
NEED_PATCH=$(kubectl -n kagent get cm kagent-controller -o json 2>/dev/null | python3 -c "
import sys, yaml
doc = yaml.safe_load(sys.stdin.read())
cfg = yaml.safe_load(doc['data']['controller.yaml'])
print('yes' if not (cfg.get('push',{}).get('allowHttp') and cfg.get('push',{}).get('allowPrivateNetworks')) else 'no')")
if [ "$NEED_PATCH" = "yes" ]; then
  echo "  打补丁并重启 controller（~1 分钟）"
  kubectl -n kagent get cm kagent-controller -o yaml > /tmp/cm-ctrl-patch.yaml
  python3 - <<'PY'
import pathlib, yaml
p = pathlib.Path('/tmp/cm-ctrl-patch.yaml')
doc = yaml.safe_load(p.read_text())
cm = doc['items'][0] if 'items' in doc else doc
cfg = yaml.safe_load(cm['data']['controller.yaml'])
cfg.setdefault('push', {})['allowHttp'] = True
cfg['push']['allowPrivateNetworks'] = True
cm['data']['controller.yaml'] = yaml.safe_dump(cfg, sort_keys=False)
p.write_text(yaml.safe_dump(doc, sort_keys=False))
PY
  kubectl apply -f /tmp/cm-ctrl-patch.yaml >/dev/null
  kubectl -n kagent rollout restart deploy/kagent-controller >/dev/null
  kubectl -n kagent rollout status deploy/kagent-controller --timeout=180s >/dev/null
  ok "allowHttp/allowPrivateNetworks 已开启"
else
  ok "已是开启状态"
fi

step "5. DeepSeek API 连通性（402 = 欠费，e2e 必挂）"
KEY=$(kubectl -n kagent get secret kagent-deepseek -o jsonpath='{.data.OPENAI_API_KEY}' | base64 -d)
CODE=$(curl -s -m 10 -o /dev/null -w '%{http_code}' https://api.deepseek.com/v1/models -H "Authorization: Bearer $KEY" || echo 000)
if [ "$CODE" = "200" ]; then
  ok "DeepSeek API 可用 (200)"
else
  fail "DeepSeek API 返回 $CODE（000=网络问题；402=账户欠费——充值后重跑）"
fi

step "结果"
if [ "$FAILURES" -eq 0 ]; then
  echo "环境就绪。下一步: scripts/image-build.sh → scripts/deploy-local.sh → scripts/e2e.sh"
else
  echo "$FAILURES 项未就绪——按上方 FAIL 项处理后重跑本脚本"
fi
exit "$FAILURES"
