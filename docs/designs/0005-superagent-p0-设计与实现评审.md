# 0005 — superagent-on-kagent P0：设计与实现评审文档

*   **Author:** jerry-jibu × ZCode
*   **Status:** Review-ready（2026-11-16；供 PR #11 设计评审使用的合并文档——0001–0004 是设计正典，本文是"设计→实现→验证"的桥接综述，所有结论以正典为准）
*   **Date:** 2026-11-16

--------------------------------------------------------------------------------

## 1. 评审范围与入口

| 评审对象 | 位置 | 说明 |
|---|---|---|
| 设计正典 | `docs/designs/0001`–`0004` | 编排层 / DeepSeek harness 与使用面 / 工作区物化 / proactive 对照。已经过 2026-11-15 三轮 EGM 修订（含改道决策、配对更正、P0 现状注） |
| 实现 | `orchestrator/`（独立 Go 模块，101 个 .go 文件：46 非测试 + 55 测试；9 个迁移） | PR #11 全部产品代码 |
| 部署 | `deploy/kagent/`（16 个文件：manifests ×10、e2e-v1.sh、scripts ×5、README） | kind 集群 e2e 形态 + 集群部署形态 |
| 评审记录 | PR #11 五轴评审（pullrequestreview-5908383785，15 条 inline，全部对应修复 commit） | 逐条对照 |

**一句话**：superagent 的领域模型照搬（Trigger/Thread/Gate/Decision/Mandate/Ticket/Assignment/Outbox），执行层全部换成 kagent 实体（A2A task/session/HITL/substrate actor/agentfs workspace/DSH BYO harness）；只新建一个编排层进程，不动 agent 运行时（上游 go/ui/helm/proto 零改动）。

## 2. 架构与数据流（V1 全链路，已真集群验证）

```
GitHub（fake/真）→ Sources 轮询(60s) → Intake(去重·14天视线) → Gate(纯函数)
  → evaluator invoke（A2A→kagent harness→DeepSeek，结构化 Decision）→ Engine accept（单事务建单+确认评论 effect）
  → 物化扫描（编排器浅克隆→真 agentfs 引擎 workspace）→ 派工扫描（base_commit 门控）
  → Dispatcher（A2A SendMessage+PushConfig，write-ahead intent + 确定性 MessageID）
  → worker actor（gVisor 沙箱，DSH BYO 桥 + agentfs MCP 工具）→ Report ready
  → 交付循环（读回全工作区→确定性 commit→push→PR→Outbox 结果评论）
  → Pass 2s 对账兜底（push 丢失/滞留/等待人/续跑）→ Outbox drain
```

关键组件与代码落点：

| 组件 | 包 | 职责（评审关注点） |
|---|---|---|
| 持久层 | `internal/store` | 9 迁移；全部状态迁移单事务、状态+审计原子；参数化 SQL（schema 名 `pgx.Identifier.Sanitize`）；实例锁 `pg_try_advisory_lock` |
| 摄入 | `internal/intake` | `(source, deterministic_id)` 幂等；Thread 归属；14 天视线 |
| 门控 | `internal/gate` | 纯函数，deny-by-default；evaluator 无权凭空 accept（Decision 携带 strength+policyVersion，代码校验） |
| 研判 | `internal/eval` | 独立 5s 循环；决策终态一次；毒 payload 落终态 rejected（不头阻塞） |
| 推进 | `internal/dispatch` | Pass 2s（派发/对账/滞留/等待人/续跑，逐条失败计数不断链）；write-ahead 派发意图；push 接收器（4MiB 上限、可选 JWT 验证） |
| 工作区 | `internal/workspace` | clone/物化/读回/patch；路径校验拒 `.git`；symlink 安全写；MaxFiles/MaxBytes；MCP 客户端可重试 dial+会话重拨 |
| 交付 | `internal/delivery` | 确定性 commit（重放同 SHA）；空变更集拒绝；幂等 PR；结果评论走 Outbox effect |
| 出站 | `internal/outbox`、`internal/github` | receipt 幂等；GitHub 限流分类（403/429→defer） |
| 策略 | `internal/policy` | 热更新 watcher（serve-then-deliver，两阶段） |
| 部署 | `deploy/kagent/` | 见 §5 |

## 3. 设计决策与实施状态（D1–D10 + 关键机制）

| 决策 | 内容 | 状态 |
|---|---|---|
| D1 | Assessment = agent invoke（evaluator CR + 结构化 Decision） | ✅；P0 wire = 单 Trigger，批输入/快照列 P2 |
| D2 | push 为主 + TaskStore 对账兜底 | ✅；push 于终态及 input-required 触发（平台事实，D9 修正确认） |
| D3 | agentfs workspace per ticket | ✅ 演进为 0003 预物化模型（见 §4 偏差 1） |
| D4 | worker = DSH BYO | ✅ superagent-worker（dsh-byo），Digest 钉死 |
| D5 | 复用集群 PG，独立 schema | ✅ 迁移 0001–0009 |
| D6 | 单副本 Deployment + 实例锁 | ✅ Recreate + `TryAcquireInstanceLock` |
| D7 | BYO 起步，一等 deepseek 类型设触发条件 | ✅ |
| D8 | ModelConfig 配对由桥约束决定 | ✅ 双 ModelConfig（worker 绑 `deepseek-openai`——桥 guard 只吃 OpenAI 型，原断言已证伪更正） |
| D9 | input-required 检测 = push 优先 + Pass 对账兜底 | ✅（按平台事实更正：push 于 input-required 即触发，`push_delivery.go:47`） |
| D10 | 编排器预物化 | ✅ 见 §4 偏差 1 |

关键机制（评审重点核对）：

1. **事务纪律**：无网络调用在事务内；状态+审计同事务原子提交；replay 守卫 `RowsAffected==0` 一致（`FailTicket` 已补齐终态守卫+assignment 结算）。
2. **派发 write-ahead intent**：send 前计数（`BeginDispatchAttempt`）；mid-flight 死亡→**收养**孤儿任务（经 gateway 验证才绑定）；无 observation→隔离窗；已记录失败→即时重试；绑定失败→孤儿入账+`dispatch.unbound` 告警审计。MessageID 确定性（per-attempt）+ 进程 salt（防 gateway messageId 幂等跨运行碰撞）。
3. **交付确定性**：author/committer 日期钉基线 commit 的 committer date → 重放同 SHA → push 幂等；空变更集永久失败不建空 PR。
4. **凭证单调**：GitHub token 只在编排器（env/Secret 注入，不进 argv/pod spec）；worker 零外部凭证（egress 日志断言）。
5. **沙箱边界**：物化白名单（文本≤4MiB、拒 `.git`、跳 symlink）+ 交付侧对称路径校验（拒 `.git` 段、symlink 安全写）。

## 4. 实施期偏差与更正记录（设计评审必读）

每条都有证据链与文档回写；详见 0001–0004 各"更正/现状注"块。

| # | 偏差 | 理由 | 文档回写 |
|---|---|---|---|
| 1 | 交付路线 A（worker 离线 git→format-patch）→ **Alternative E**（worker 纯文件编辑，编排器读回算 patch） | agentfs 物化通道仅承载 UTF-8 文本，`.git` 二进制 pack 无法物化——路线 A 前提在通道层不可满足（非沙箱行为问题，无需 V4/V5 验证） | 0003 决策记录 + 新时序图 |
| 2 | 单 Anthropic ModelConfig → **双 ModelConfig**（worker 绑 OpenAI 变体） | 桥 config guard 硬性只收 OpenAI 型（byo-ext `config.go:52-53`），原"桥不挑配对"被证伪 | 0002 A1 更正块 |
| 3 | requireApproval/Tools 圈定 → **Report `state=waiting` 终态** | 现桥拒绝非空 `Tools` 绑定（`config.go:92-93`）；CUJ4 标 P0 不可达、P1 激活 | 0002 A3 更正 |
| 4 | "push 只承诺终态"→**push 优先检测** | 平台 push 于 `INPUT_REQUIRED/AUTH_REQUIRED` 即触发（`push_delivery.go:47`；编排器 `dispatcher.go:357` 已实现） | 0002 §3/D9、0001 §5 |
| 5 | `kagent-default` → **`kagent-harness`**（显式 CR） | 前者是 1.0.1 onboarding 内置，本集群 v1.0.0-rc3 无此项 | harness-kagent.yaml + 0002 |
| 6 | 沙箱→agentfs 出站需**显式 EgressPolicy** | 本版本 BYO 编译器白名单未含 MCP 绑定目的地（0003 记载与构建不符） | egresspolicy-agentfs.yaml（原生扩展点，Agent spec 级 `egressPolicyRefs`） |
| 7 | Outbox 纪律收窄：仅结果评论入队 | git push/建 PR 为交付器幂等直调（确定性 commit + 同 head 分支复用）；已接受偏离，P1 多通道时统一入队 | 0001 §6 |
| 8 | BYO env 契约现状=bootstrap（非双 env） | PR 树 go/ 已注入 `KAGENT_BOOTSTRAP_JSON`；桥 pr-2 HEAD `8632d5e` 同走 bootstrap | 0002 A4 更正 |

## 5. 部署清单与集群形态

| Manifest | 内容 |
|---|---|
| `agentfs.yaml` | 真组件 Deployment+Service+RemoteMCPServer `agenticfs-mcp`（digest 钉死 `1476dee6…`）；Pod= kagent-bridge（9765 streamable-HTTP）包装真 agentfs 引擎（`agentfs mcp` stdio 子进程） |
| `kagent-bridge/` | 桥源码（stdlib-only；真引擎为上游，数据/checkpoint/审计在引擎侧）。MCP 形态适配四处：`agenticfs_*` 命名空间双向映射、响应 id 回写客户端 id、jsonrpc 版本标签归一、GET SSE hold-open、schema-less 工具补 inputSchema |
| `egresspolicy-agentfs.yaml` | worker 沙箱→agentfs 出站授权（原生 `EgressPolicyRefs` 扩展点） |
| `modelconfig-deepseek{,-openai}.yaml`、`harness-{dsh-byo,kagent}.yaml`、`agent-{worker,evaluator}.yaml` | 运行时 CR（digest 钉死） |
| `orchestrator.yaml` | 集群部署形态：SA、安全基线（runAsNonRoot 65532/drop ALL/只读根 fs+/tmp emptyDir）、NetworkPolicy、push JWT/admin token 可选注入 |
| `scripts/` ×5 | env-setup / build / image-build / deploy-local / e2e（全流程脚本化，踩坑注释在脚本内） |

## 6. 验证证据

| 层 | 命令/方式 | 结果 |
|---|---|---|
| 单元 | `go build/vet`、`go test ./...` | 13 包全绿（PG 语义测试 `DATABASE_URL` 驱动） |
| 真库 | `DATABASE_URL` 指向真实 PostgreSQL | 全绿（曾暴露并修复 cursor 微秒精度存量 bug） |
| 竞态 | `go test -race ./internal/...` | 全绿（TestWatcher 两阶段竞态已修） |
| E2E | `deploy/kagent/scripts/e2e.sh`（真 kind 集群：真 Gateway、真 agentfs、真 DSH worker actor、真 DeepSeek） | **V1 全链路验收：全部通过（20 项断言）**，含 kill -9 中途恢复（恰 1 ticket / 1 PR）、交付分支内容=worker 改动、egress 零 github.com 出站（167 次 DeepSeek 命中证非空转）、worker env 零凭证 |

**修复轨迹（透明记录）**：e2e 共 8 轮迭代，逐轮修掉真实问题——DeepSeek 402 欠费（外部）、controller allowHttp 未开（部署配置）、DSH go-sdk 四项 MCP 契约（桥传输适配）、messageId 跨运行碰撞（gateway 幂等）、恢复窗口不足。每轮的失败→定位→修复均有日志与代码证据。

## 7. 已知限制与 P1 预留（评审知悉即可）

- 入站面 JWT/admin token/NetworkPolicy 均以**可选开关**交付，空缺=dev 模式（启动 WARNING）；非 dev 集群部署前必须设置（0002 风险表有记录）。
- 0003 V2–V5 场景未执行（V4/V5 前提被通道能力否定；V2/V3 未排期）。
- 重派防抖下限、Outbox dead-letter、`delivery.retried` 事件名、GIT_CONFIG 隔离、eval 退避等 13 项改进列 P1（`PR11_REMEDIATION_PLAN.md` §4）。
- 集群部署形态的 `orchestrator.yaml` 尚未在本集群整体验证（e2e 用本地进程编排器）；干净集群从零 apply 演练为合并后动作。

## 8. 评审建议路径

1. 读 §2/§3 把握架构与决策 → 对照 `docs/designs/0001` §2–§8 细节。
2. 按 §4 偏差表逐条核对"文档更正块 ↔ 代码证据"（每条带 file:line）。
3. 代码抽查优先级：`store`（事务纪律）→ `dispatch/dispatcher.go`（write-ahead intent + 收养）→ `workspace/{contract,patcher}.go`（沙箱边界）→ `delivery/deliverer.go`（确定性交付）。
4. 验证复现：`scripts/env-setup.sh && scripts/build.sh && scripts/e2e.sh`（真集群）；单元层 `go test ./...` + `-race`。

--------------------------------------------------------------------------------

*本文档随 PR #11 演进；设计争议以 0001–0004 正典与行内证据为准。*
