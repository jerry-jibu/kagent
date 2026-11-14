# deploy/kagent — superagent-on-kagent P0 部署清单

设计依据：[docs/designs/0002-deepseek-harness-落地与对外使用面设计.md](../../docs/designs/0002-deepseek-harness-落地与对外使用面设计.md)、[docs/designs/0003-工作区物化与上下文可达性.md](../../docs/designs/0003-工作区物化与上下文可达性.md)。
基线：kagent v1.0.1 fork（int/superagent-p0）+ substrate v1.0.0（ate-system），kind 集群 `kagent`。

## 清单

| 文件 | 资源 | 说明 |
|---|---|---|
| `modelconfig-deepseek.yaml` | ModelConfig `deepseek` | Anthropic 兼容网关（evaluator 用）；复用既有 Secret `kagent-deepseek` |
| `modelconfig-deepseek-openai.yaml` | ModelConfig `deepseek-openai` | OpenAI 兼容网关（worker 专用配对：DSH BYO 桥的 config guard 只接受 `model.openai`）；同一把 DeepSeek key 双协议通用 |
| `harness-dsh-byo.yaml` | Harness `dsh-byo` | BYO 契约；`${DSH_BYO_IMAGE}`、`${SNAPSHOT_LOCATION}` 需代入 |
| `agent-worker.yaml` | Agent `superagent-worker` | inline template + agentfs MCP 绑定 + `deepseek-openai` |
| `agent-evaluator.yaml` | Agent `superagent-evaluator` | 内置 `kagent-default` harness + `outputSchema` 结构化 Decision + `deepseek` |
| `orchestrator.yaml` | ConfigMap `superagent-policy` + Deployment `superagent-orchestrator` | 编排器单副本（实例锁）+ policy 热加载 + PG Pod 证书投影 + T9 全链路 flags |
| `e2e-v1.sh` | 验收脚本 | V1 全链路（fake GitHub 面可重复；见下） |

## 镜像构建台账（源 SHA → digest，显式重同步，不自动漂移）

| 镜像 | 源 | 记录 |
|---|---|---|
| `kagent-dsh-byo` | kagent-dev/byo-ext **PR #2**（未合入 main）分支 `pr-2` @ `19771c8` | `localhost:5000/kagent-dsh-byo@sha256:5a71928b0aaad081b06ee9600190d967857d22a209bae85a49a631dabe79b9eb`（T9 前基线，旧 env 契约） |
| `kagent-dsh-byo`（T9） | 同 PR 分支 `pr-2` @ `8632d5e`（`7383f7b` bootstrap 契约 + `14af3d4` durable 子目录 + `8632d5e` chmod EPERM 容忍） | `localhost:5000/kagent-dsh-byo@sha256:6dd2a0076d306d14751a714d69fccec5b6dc85c46936888cbf60cea02ed841ac` |
| `superagent-orchestrator` | 本仓库 `orchestrator/` 模块 | `localhost:5000/superagent-orchestrator@sha256:ef37c412ce04f2a347bd5afdf77fa99453d09a9126bd34bc7885006c302dea30` |

台账纪律（0002 风险表）：镜像重构建/上游重同步时，先记录 源SHA → digest 对应关系，再改 Harness/Deployment 的 digest；升级 kagent 时按 0009 upgrade 契约重验（本 T9 已验证：bridge 升级跟进 bootstrap-bundle 契约 = 预测的升级契约风险的实做）。

### DSH BYO 镜像构建（T9 形态）

```bash
git -C /path/to/byo-ext fetch origin pull/2/head:pr-2 && git -C /path/to/byo-ext checkout pr-2
# 桥已适配 bootstrap-bundle 契约（kagent go module 经本地 replace + vendor 具体化）
cd /path/to/byo-ext/harnesses/deepseek
export PATH=$HOME/sdk/go1.27.3/bin:$PATH
go mod tidy && go mod vendor   # 本地 replace 的 kagent 树打入 vendor/，容器内零网络构建
# 构建文件（CN 网络镜像 + vendor 构建）：kagent-dev/.superagent-notes/build/Dockerfile.dsh
docker build -f /path/to/Dockerfile.dsh --target production -t localhost:5000/kagent-dsh-byo:bootstrap .
docker push localhost:5000/kagent-dsh-byo:bootstrap
curl -sI http://localhost:5000/v2/kagent-dsh-byo/manifests/bootstrap \
  -H 'Accept: application/vnd.oci.image.index.v1+json' | grep -i docker-content-digest   # 记入台账
```

### 编排器镜像构建

```bash
cd orchestrator   # 仓库根下的独立 Go module（github.com/jerry-jibu/kagent/orchestrator）
docker build -f Dockerfile -t localhost:5000/superagent-orchestrator:t9 .
docker push localhost:5000/superagent-orchestrator:t9   # 运行时含系统 git（clone/push 用）
```

## kind ↔ 本地 registry（本机 e2e 前置）

本地 registry 容器 `local-registry`（registry:2 @ localhost:5000）需两条打通：

1. **节点侧 containerd（CRI 拉取路径）**：kind 节点写
   `/etc/containerd/certs.d/localhost:5000/hosts.toml`
   （`[host."http://<registry-addr>:5000]"`，`capabilities=["pull","resolve"]`，`skip_verify=true`）
   并在 `/etc/containerd/config.toml` 的 `[plugins."io.containerd.grpc.v1.cri".containerd]`
   加 `config_path = "/etc/containerd/certs.d"`，重启 containerd。
   ⚠️ china-network 已知坑：`kind load docker-image`（docker 29）会因 attestation blob 缺失而坏——走 registry 路线。
2. **substrate 侧（actor 镜像拉取路径）**：atelet 的 imageCache 自己走 go-containerregistry
   （`imageCache.EnsureImage`），对 localhost/loopback 引用先 https 后 http 回退。注册表必须从
   atelet Pod 可达，且 digest 形态的 Harness 镜像在 atelet 的视角可达：

   ```bash
   docker network connect kind local-registry          # registry 进 kind 网络
   kubectl -n ate-system patch ds atelet --type=json \
     -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--localhost-registry-replacement=172.18.0.3:5000"}]'
   # 172.18.0.3 = registry 容器在 kind 网络上的 IP（docker inspect 可查）；Pod 均可达
   ```

   两者都就绪后 `crictl pull` 与 golden build 均可取 `localhost:5000/kagent-dsh-byo@sha256:<digest>`。

3. **kind 网关 push 回调（本机 e2e）**：编排器 push-base-url 指向宿主时，网关默认拒绝
   http/私网 push URL——kind 环境需在 `kagent-controller` ConfigMap 置
   `push.allowHttp: true`、`push.allowPrivateNetworks: true` 后重启 controller（生产保持 false）。

## 数据库与 Pod 证书（编排器接集群 PG）

集群 PostgreSQL 为 TLS-only + 证书认证（`hostssl ... cert`，`hostssl all postgres all reject`）：

1. 编排器 Pod 复用 controller 同款 Pod 证书投影（signer `postgres.podcert.ate.dev/identity`，
   annotation `postgres.podcert.ate.dev/username: kagent_owner_user`）——已写进 `orchestrator.yaml`。
2. **signer 白名单**：podcertificate-controller 的
   `--postgres-client=kagent/superagent-orchestrator=kagent_owner_user` 参数必须存在
   （本机已 patch；helm 重装后需重加）。
3. schema 预建（owner 用户无数据库级 CREATE 权限；编排器的 store.Open 探测到 schema 存在即跳过创建）：

   ```bash
   kubectl -n kagent exec deploy/kagent-postgresql -- \
     psql -U postgres -d kagent -c "CREATE SCHEMA IF NOT EXISTS orchestrator AUTHORIZATION kagent_owner_user;"
   ```

## 应用

```bash
export DSH_BYO_IMAGE='localhost:5000/kagent-dsh-byo@sha256:<64hex>'   # 台账 digest
export SNAPSHOT_LOCATION='s3://ate-snapshots/kagent/'                 # 与集群既有 Harness 一致
export POLICY_REPO='example/repo'          # Gate 仓库名单
export POLICY_SENDER='maintainer-login'    # Gate admission 名单
export GITHUB_SOURCE_REPO='example/repo'   # Source 轮询仓库；空串 = Source 关闭（空转模式）
export WORKSPACE_BASE_BRANCH='main'
export GITHUB_GIT_URL='https://github.com'
for f in modelconfig-deepseek modelconfig-deepseek-openai harness-dsh-byo agent-worker agent-evaluator; do
  sed -e "s|\${DSH_BYO_IMAGE}|$DSH_BYO_IMAGE|" -e "s|\${SNAPSHOT_LOCATION}|$SNAPSHOT_LOCATION|" $f.yaml | kubectl apply -f -
done
sed -e "s|${ORCH_IMAGE}|localhost:5000/superagent-orchestrator@sha256:<64hex>|" \
    -e "s|${POLICY_REPO}|$POLICY_REPO|" -e "s|${POLICY_SENDER}|$POLICY_SENDER|" \
    -e "s|${GITHUB_SOURCE_REPO}|$GITHUB_SOURCE_REPO|" \
    -e "s|${WORKSPACE_BASE_BRANCH}|$WORKSPACE_BASE_BRANCH|" \
    -e "s|${GITHUB_GIT_URL}|$GITHUB_GIT_URL|" orchestrator.yaml | kubectl apply -f -
```

## 编排器 flags 全集（cmd/orchestrator）

| flag | 语义 | 空缺时 |
|---|---|---|
| `--database-url` | PostgreSQL 连接串（必需） | 拒绝启动 |
| `--schema` | 编排器自有 PG schema（存在则跳过创建） | `orchestrator` |
| `--policy-dir` | policy ConfigMap 挂载目录 | `/etc/orchestrator/policy` |
| `--listen-addr` | healthz/admin + push receiver 监听 | `:8080` |
| `--policy-poll-interval` | policy 热加载轮询 | `5s` |
| `--github-source` | Source 轮询仓库（owner/name） | 空 = Source 关闭（下游全关） |
| `--github-api-url` | GitHub REST base（fake GitHub 指这里） | `https://api.github.com` |
| `--github-token` | 编排器持有的 GitHub 凭证（clone/push/PR/评论；凭证单调） | 空 = 匿名（公开仓库） |
| `--github-poll-interval` | Source 轮询节拍 | `60s` |
| `--github-start-cursor` | 全新 Source 起始 RFC3339 位点 | 仓库原点 |
| `--a2a-gateway-url` | kagent A2A Gateway origin（例 `http://kagent-controller.kagent.svc:8083`） | eval/Pass 链关闭 |
| `--evaluator-agent` | 研判 Agent 路由（`name` 或 `ns/name`） | eval 链关闭 |
| `--eval-interval` | eval 循环节拍 | `5s` |
| `--worker-agent` | worker Agent 路由（派工目标） | Pass 节拍关闭 |
| `--push-base-url` | 网关可达的编排器 origin（push 回调） | 由 listen-addr 推导（仅单机） |
| `--pass-interval` | Pass 对账节拍（派工/对账/waitsForPerson/follow-up/outbox/物化扫描） | `2s` |
| `--dispatch-start-deadline` | G7 卡死窗口（派工后无产出即 needs-attention） | `10m`（负值关闭） |
| `--workspace-mcp-endpoint` | agentfs MCP 桥端点（例 `http://agentfs.kagent.svc:9765/mcp`）；**为空时交付链关闭，且派工不再等待物化基线** | 物化/交付链关闭 |
| `--workspace-base-branch` | 物化器逐工单克隆的基分支（P0 单一部署级取值） | `main` |
| `--github-git-url` | git remote 基址（mandate 仓库名拼 `.git`） | `https://github.com` |

链路序（accept 事务提交后）：物化扫描（accept→物化→基线落库）→ 派工扫描（`RequireWorkspace`：基线未落库不派工）→ worker Report ready → 交付循环（工作区读回 → 编排器侧 diff → push 分支 → PR → 结果评论 effect）。

## 验证（CUJ1/CUJ2，0002）

```bash
kubectl -n kagent get modelconfig deepseek deepseek-openai harness dsh-byo \
  agent superagent-worker superagent-evaluator
kubectl -n kagent get agent superagent-worker -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'
```

- UI `/agents` Harnesses tab 列出 `dsh-byo`（引用 agent 只出现在删除确认文案）。
- 对话验证（CUJ2，网关直呼；注意 `A2A-Version` 头必需）：

  ```bash
  curl -s http://localhost:8083/agents/kagent/superagent-worker -X POST \
    -H 'Content-Type: application/json' -H 'A2A-Version: 1.0' \
    -d '{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"role":"ROLE_USER","parts":[{"text":"请自我介绍"}],"messageId":"c1"}}}'
  ```

- 流式应答走 DeepSeek 模型（经 egress）；`kubectl -n kagent logs deploy/kagent-controller` 无编译错误。
- bridge 沙箱零凭证：atelet Run 日志的 env 只含模型凭证占位 + 平台注入（无 GitHub token）。

## V1 全链路 e2e（fake GitHub 面，可重复）

```bash
deploy/kagent/e2e-v1.sh /tmp/kagent-v1-e2e
```

脚本走通 0003 V1 全链（①issue @bot →②摄入/Gate →③evaluator accept →④建单+确认评论 →⑤物化基线 →⑥真 Gateway 派工 → 真 DSH BYO worker actor（gVisor）改文件 → Report ready →⑦编排器读回/diff/push/PR/结果评论），并在 store（权威视图）、fake GitHub（评论 marker + PR）、bare git 仓（交付分支内容）、egress 网关日志（DeepSeek 命中存在 + github.com 零命中）四处留证据。GitHub 面全程 fake（`--github-api-url` 指向 `cmd/fakegithub`）。

**真 GitHub 走查（协调者步骤）**：需要用户 GitHub token（Secret `superagent-orchestrator-github`）+
`--github-source`/`--github-api-url`/`--github-git-url` 指向真实仓库（`https://github.com`）；
编排器 flags 全集见上表。
