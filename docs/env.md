# 本机 kagent 环境总览（env.md）

> **机器专属本地文档** —— 记录 2026-11-08 在本机实际部署并验证过的 kagent 全栈环境。已加入 `.git/info/exclude` 本地忽略，勿提交上游。
> 完整手册集：`~/kagent-manuals/`（安装/配置/使用/K8s 组件四册）；Substrate 侧集成物：`agent-data-workspace/deploy/`。

## 1. 环境栈

| 层 | 组件 | 版本 | 位置 |
|---|---|---|---|
| 宿主机 | macOS Sonoma arm64 | 14.8 | — |
| 虚拟机 | colima（vz + virtiofs，4C/8G/60G） | colima 0.10.3 / lima 2.3.0 | `~/.colima/` |
| 集群 | kind `kagent`（K8s v1.37.0） | kind 0.33.0 | VM 内 docker 容器 `kagent-control-plane` |
| 控制面 | kagent | v1.0.1（2026-11-13 自 rc3 原地 helm 升级，substrate 仍 1.0.0） | ns `kagent` |
| 运行时 | Agent Substrate | v1.0.0 | ns `ate-system` |
| 数据工作区 | AgenticFS（集群内 + macOS 直挂） | 主分支构建 | ns `kagent`，Pod `agentfs` |
| 数据源 | SeaweedFS（S3 兼容） | 3.80 | ns `kagent`，Pod `seaweedfs` |

工具链：kubectl v1.37.2、helm v3.22.1、docker 29.4.3、Go 1.27.3（`~/sdk/go1.27.3`）、pnpm/node 26。二进制统一在 `~/.local/bin`（部分在 `/opt/homebrew/bin`）。

## 2. 访问方式（NodePort 直连，零 port-forward）

kind `extraPortMappings` + k8s NodePort + colima 自动转发 → **Mac localhost 直连**（集群在跑就永远有效）：

| 服务 | 地址 | 凭据 |
|---|---|---|
| kagent Dashboard | http://localhost:8082 | 无（内置用户体系） |
| kagent API（CLI 已指向） | http://localhost:8083 | — |
| AgenticFS Web 控制台 | http://localhost:8765/?token=agentfs-in-cluster | token 即 `agentfs-in-cluster` |
| AgenticFS MCP 桥 | http://localhost:9765/mcp | 无（stdio 桥） |
| SeaweedFS S3 | http://localhost:8333（桶 `demo`） | 任意非空 key/secret |

⚠️ `helm upgrade`（kagent install）会把 chart 自建 Service 改回 ClusterIP —— 重装后重打补丁：
```bash
kubectl -n kagent patch svc kagent-controller --type merge -p '{"spec":{"type":"NodePort","ports":[{"name":"http","port":8083,"targetPort":8083,"nodePort":30883}]}}'
kubectl -n kagent patch svc kagent-ui --type merge -p '{"spec":{"type":"NodePort","ports":[{"name":"http","port":8080,"targetPort":8080,"nodePort":30882}]}}'
```

## 3. 服务清单（Pod → 职责）

| Pod | 镜像 | 职责 |
|---|---|---|
| `kagent/controller` | kagent/controller:1.0.1 | watch CRD、编译 ActorTemplate、gRPC+A2A+MCP 网关（8083）、会话管理 |
| `kagent-ui` | kagent/ui:1.0.1 | Web 控制台（30882） |
| `kagent-tools` | kagent/tools:0.3.0 | 内置 MCP 工具服务器（8084/mcp，8085/metrics） |
| `kagent-postgresql` | postgres:18-alpine + tls-reloader sidecar | 会话/任务/历史持久层（5432） |
| `kagent-default` | substrate/ateworker-gvisor:v1.0.0 | gVisor 沙箱 WorkerPool（actor 运行处） |
| `agentfs` | agentfs-incluster:0.1.0 | AgenticFS：agentfsd（8765 UI/REST）+ kagent-bridge（9765 MCP）；数据 = macOS `~/.agenticfs` 直挂 |
| `seaweedfs` | chrislusf/seaweedfs:3.80 | S3 兼容数据源（8333，PVC 2Gi），桶 `demo` |
| ate-system 全套 | substrate/*:v1.0.0 | Substrate 控制面（ate-api-server×2 / ate-controller / ate-idp×2 / atelet DS / atenet-egress+router=agentgateway / k8s-credential-provider / rustfs） |
| `podcertificate-controller` | substrate/podcertcontroller | 全集群 mTLS 证书根（K8s v1.37 PodCertificate API） |

kagent CR 现状：`Agent/workspace-agent`（READY）= DeepSeek 模型 + kagent-harness + `agenticfs-mcp` MCP 工具绑定。
1.0.1 安装另带三个内置 Harness：`kagent-default` / `codex-default` / `claude-default`（v1.0.1 onboarding 新特性）。

## 4. 配置清单（全部落盘点）

| 文件 / 资源 | 内容 |
|---|---|
| `~/Library/Application Support/kagent/config.yaml` | CLI 配置：provider、chart 版本钉住（kagent `1.0.1` / substrate、atelet `1.0.0`，**无 v 前缀**）、`bundledPostgresImage: postgres:18-alpine`、substrate/atelet valuesFiles |
| `~/Library/Application Support/kagent/overrides/substrate-images.yaml` | 去 digest 镜像覆盖（绕 DaoCloud tag 缓存漂移） |
| Secret `kagent-deepseek`（ns kagent） | `ANTHROPIC_API_KEY`（DeepSeek Anthropic 兼容网关） |
| ModelConfig `deepseek` | provider Anthropic + `baseUrl: https://api.deepseek.com/anthropic` + model `deepseek-flash` |
| Harness `kagent-harness` | `kagent: {}` + substrate：workerPool `kagent-default` + `snapshotPolicy.location: s3://ate-snapshots`（**必须 URI 形式**） |
| RemoteMCPServer `agenticfs-mcp` | `http://agentfs.kagent.svc.cluster.local:9765/mcp`（集群内 DNS；egress 拒 IP） |
| Agent `workspace-agent` | systemPrompt 声明两类文件工具分工；tools 绑定 agenticfs-mcp |
| `~/.agenticfs/datasources.json` | s3 数据源 `seaweed-demo`（endpoint 8333，凭证任意非空值） |
| `~/kagent-manuals/manifests/` | 全套 CR 存档 + `REBUILD-RUNBOOK.md` + kind-config |

## 5. 存储布局

| 路径 | 内容 |
|---|---|
| `~/.agenticfs/`（macOS，**经 extraMounts 直挂进集群**） | AgenticFS 家目录：工作区（workspace.db + upper + CAS）、审计库、datasources.json |
| PVC `agentfs-data`（弃用残留） | 形态 A 时代数据；确认无用后可删 |
| PVC `seaweedfs-data` | SeaweedFS 卷数据 |
| `~/.colima/` | VM 磁盘、docker.sock |
| kind extraMounts | `/Users/jibu/.agenticfs` → 节点 `/mnt/agenticfs`（Pod hostPath） |

数据源挂载语义：工作区 = 只读基座（数据源）+ delta 增量层（Agent 写入），checkpoint/回滚作用于 delta。

## 6. 关键操作

```bash
# 开机恢复
colima start && docker start kagent-control-plane   # VM 起后节点容器若未自启则手动 start
# 一切入口自动恢复（extraPortMappings 常驻）

# agentfs 重启（单实例：必须 scale-to-zero，rolling 会撞持久卷锁）
kubectl -n kagent scale deployment/agentfs --replicas=0 && sleep 10
rm -f ~/.agenticfs/*.lock
kubectl -n kagent scale deployment/agentfs --replicas=1

# 节点镜像重灌（kind 重建后）
bash agent-data-workspace/deploy/agentfs-incluster/rebuild-node-images.sh

# 端到端冒烟
kagent agent session create --agent workspace-agent && kagent agent invoke --session <id> --stream --task "agenticfs_list_workspaces"
```

## 7. 已知约束

- Mac → VM 直连 IP（192.168.5.1）**不通**（NAT 网络）；一切访问走 localhost 自动转发
- agentfs 单实例：重启用 scale-to-zero（锁文件在持久卷，rolling 必撞）
- NFS automount 在容器内已显式停用（`user-paused`，issue #339 fail-open）—— 工作区访问走 MCP/控制台
- kagent egress 只认 DNS 名（拒 IP）；外部服务接入 = CoreDNS hosts 或集群内部署
- ghcr 镜像走 DaoCloud 有白名单/缓存漂移问题 → 预载管线（`load-one.py` 修 index + `ctr tag` 全限定名）
