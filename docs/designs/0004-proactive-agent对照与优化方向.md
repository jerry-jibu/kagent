# 0004 — Proactive/Managed Agent 对照与优化方向（Anthropic always-on × 火山 MA × kagent/DSH/superagent）

*   **Author:** jerry-jibu × ZCode
*   **Status:** Draft
*   **Date:** 2026-11-14

--------------------------------------------------------------------------------

## Overview

三篇材料（两篇为同文异译）给出"主动式/持续在线 agent"的两种形态：Anthropic 官方架构指南（五组件）与火山引擎 Managed Agents 的托管实践。本文把它们对照我们的栈（kagent v1.0.1 + DSH BYO + superagent-on-kagent 设计 0001–0003），产出：互证点、缺口清单（按优先级）、刻意差异声明，以及回馈到现有设计文档的具体条目。

**材料**：

1. Anthropic《Building always-on agents》（claude.com/resources/articles，两个中文译本）——五组件架构：事件收件箱 / 计时器 / 记忆 / 权限 / 发件箱。
2. 火山引擎《基于 Managed Agents 搭建主动式个人 Agent（Open Muse）》——托管 AaaS：长 loop 断点续跑、session 隔离弹性、File API+对象存储文件流转、trace 重放；上层补设备连接器、主动触达、账号托管、审批卡片。

**核心命题（两份材料一致）**："始终在线"是使用体验，不是常驻进程——大部分时间休眠、事件/计时器唤醒、run 短、循环长。这与 0009 §7.2 的三层长跑对照（编排层常驻、会话/进程层按需）完全同构。

## 互证点：五组件 × 我们的栈（独立收敛的证据）

| Anthropic 组件 | superagent 语义（原型） | 我们的方案（0001–0003） | kagent/DSH 现成 | 判定 |
|---|---|---|---|---|
| 事件收件箱（持久事件表 + 确定性去重 + 单 worker + 合并待处理） | Sources/Intake（触发不可变、确定性 ID、cursor -5s 重叠去重） | Store.triggers + Source 插件；evaluator 批量研判（0001 P2）= "合并待处理" | kagent 无（ScheduledRun 只是 cron 原料） | ✅ 已设计，语义一致 |
| 计时器（follow-up 一次性 + schedule 周期，独立存储，调度器扫到期→入收件箱） | Passes 节拍 + Ticket 到期/重派（evaluateDue） | Pass 2s 对账含到期处理（0001 D2） | ScheduledRun（cron） | 🟡 **部分**——见缺口 G1 |
| 记忆（四类文件：用户指令/用户笔记/已告知/任务进度；"只存查不到的"；dreaming 整理） | Thread 快照/Ticket 历史按值注入 | session(TaskStore) + agentfs workspace 持久 | DSH 原生会话日志；**kagent harness 有 Memory+Compaction 字段但 BYO 型没有**（`harness_types.go:25-72`） | 🟡 **部分**——见缺口 G2 |
| 权限（凭证代理注入 / auto mode 分类器 / **延后工具调用**） | Gate（名单+admission+strength）+ HITL | 0001 §7 门控；0002 A3 通道 2 | **egress 凭证注入（1006）**；**通道 2 受保护 MCP = 延后调用同构**（扣住调用→存档→停进程→卡片问人→答案作为新事件→新 turn 续跑，几乎逐字对应） | ✅ 强互证 |
| 发件箱（待发消息表 + dispatcher + 频率调校"先少说话"） | Outbox（事务内入队、receipt 幂等）+ 最小打扰原则 | Outbox + Channel 适配 | 无（编排器自持） | ✅ 已设计 |
| 权限-凭证托管（凭据在运行环境之外，代理注入） | Source 凭证只在编排器 | 凭证单调（0001 §7）+ D10 编排器代 push | egress 注入模型凭证（bridge 零凭证，1006） | ✅ 强互证（Open Muse 亦同：refresh token 只在服务端，云端电脑拿短期令牌） |
| **presence/cognition split**（agent-substrate/always-on-agent 参考实现：常驻网关持渠道连接，agent=可挂起 actor；webhook 类渠道无需 presence 层） | Sources/Pass/Engine 常驻 + 进程层按需 | 0001 §2.1 三层模型 + §2.1.1 红线（Source 住编排器、worker 永可挂起、渠道两族分类） | substrate 挂起/唤醒（snapshotPolicy） | ✅ 强互证（G10：socket 渠道扩展 = 编排器加 Source 插件，零架构变化） |
| **poll&ack + lease hand-off + 事件队列**（claude-managed-agents：Valkey stream、reclaim 窗口、lease heartbeat） | —（无对应机制） | A2A push + Pass 对账 + 事务性 Outbox（0001 §6） | — | ✅ 反向互证（CMA Improvements 自认双队列漂移，正在移除）+ **G7 真增量**：受理与执行之间必须有期限与显式释放者（滞留检测已落 T6 实码） |
| **状态放置三态**（Hermes：turn 间一切在 /data 磁盘 + VOLUMES；CMA：per-session actor 快照；OpenClaw：FULL 内存快照） | — | DSH state.json + DATA + VOLUMES（0003 V2） | substrate DATA 快照 | ✅ 强互证（OpenClaw FULL 快照为**反例**：55–61MiB/actor、restore 3.3–4.7s、模板重指丢全部会话态——其 roadmap 正向我们位置移动） |
| **busy-signal + 模板不可变 + 调度外置**（hermes turn-signal"deliberately not a heartbeat"、Templates immutable、scheduled work outside sleeping actors） | Ticket follow-up（Passes 节拍） | G1 timers（0001 §6）+ 通道 2 requireApproval（0002 A3）+ digest 台账（A4） | — | ✅ 互证（暴露 **G8 盲点**：worker 后台在途保护——P0 Report 约定，P2 信号机制） |

**MA 平台能力对照**：长 loop 断点续跑/异常重试 ≈ substrate 快照 + DATA 恢复 + completion barrier（0009）；session 隔离弹性 ≈ substrate actor 按需/挂起；File API+对象存储 ≈ agentfs + SeaweedFS（我们还多 checkpoint）；trace 重放 ≈ TaskStore 历史 + OTel（全状态重放是我们的弱项，见 G5）。

## 缺口与优化清单（后续方向）

*   **G1（P1）计时器显式化**：0001 的 evaluateDue 覆盖"编排器判定到期"，但缺"**worker 自己请求 follow-up**"的入账路径。补法：Store 增 `timers` 表（一次性 follow-up + 周期两种，字段对齐 Anthropic 结构），worker Report 新增 `follow_up_at` 字段 → Engine 事务内入 timers → Pass 扫描到期 → 转 attention/重派/入收件箱。改动小（一张表 + Report 一字段 + Pass 一分支），是五组件里我们最薄的一块。
*   **G2（P2）记忆文件约定**：worker workspace 内落四类文件约定（至少 `progress.md`/任务进度 + `reported.md`/已告知，跨 run 续接），编排器侧 per-Thread 记忆（用户偏好/指令，Store 一行 JSON）随派工消息注入。依赖 agentfs 读写已在 worker 工具集内，成本低；跨工单记忆是"越用越准"的关键。注意 kagent harness 型才有 Memory/Compaction 字段，BYO worker 靠这套文件约定补齐——这也解释了为什么 evaluator（kagent harness）与 worker（BYO）的记忆能力不同构。
*   **G3（P2）运行中风险分类器**（auto mode 类比）：我们的 Gate 是事前判定 + requireApproval 事中扣留；Anthropic 在运行中用分类器实时拦危险调用并让模型换路。可在通道 2 扣留点上加一层轻量分类（规则先行），拦截后给 worker 回注"改用更安全路径"的输入。
*   **G4（P2）发送频率评测**：最小打扰原则目前是静态规则（只在 attention/result 发）；按 Anthropic 建议，对通知建立评测集，从少发逐步放宽。
*   **G5（P2）trace/重放强化**：编排器侧把 Trigger→Decision→Assignment→Outbox effect 全链路事件化（现有 Store 已存全部中间态，缺统一 trace 视图/重放），对齐 MA 的调试体验。
*   **G6（P2）记忆整理（dreaming）**：离线任务回看历史会话、清理过时条目、提炼跨 run 模式——P2 之后再议。
*   **G7（P1 实码+文档）滞留检测**：assignments.deadline 字段 + Pass 滞留分支（超期非终态 → needs-attention，不自动重放；重派防抖间隔下限）——**已落 T6 实码**（0006_stuck 迁移）；0001 §6/§8 文档行已并入。来源：CMA lease 教训。
*   **G8（P0 约定 + P2 信号）worker 后台在途保护**：P0 = Report 约定"ready 前无在途后台任务"（0001 §3）；P2 = hermes busy-signal 类信号机制。
*   **G9（P2）WorkerPool 容量方法论**：OpenClaw duty-cycle 实测（0.72%、1000 agent 投影 47:1、"Size a pool on the peak"）+ CMA benchmarks 工具；**适用性警告**：编码 agent duty cycle ≠ 个人助理，数字只借方法不借结论。
*   **G10（P1 文档）presence/cognition 红线与渠道两族分类**：已并入 0001 §2.1.1。
*   **G11（观察项）substrate 官方 GC/挂起能力**：就绪即弃自管逻辑（对齐 CMA Improvements 姿态）。

## 刻意差异（红线，不跟从）

*   **调用级自动重试**：MA/always-on 默认"异常重试"；我们**不确定结局不自动重放**（0009 §4 / 0001 §8）——远程副作用不可判定时重试可能双花。我们的重试是 task 级确定性重派（人可核），不是调用级盲重试。
*   **独立 presence 网关不造**：always-on-agent 的 gateway 模式仅适用于"presence 层职责单一"的形态；我们的编排器已经是 presence 且职责更多（Gate/Engine/Outbox）——socket 渠道扩展 = 加 Source 插件，不是第二网关。openclaw gateway-driven suspend 的动机（沙箱无控制面凭证）在我们不成立（snapshotPolicy 已把挂起下沉给 substrate）。
*   **per-session actor 与 FULL 内存快照不跟从**：CMA per-session actor 由 Anthropic session 语义驱动（不可移植，工单节拍下会爆 actor 数）；我们 per-agent actor 复用 + per-Ticket session/workspace 隔离是刻意差异。OpenClaw FULL 内存快照证据见互证表。
*   **失败自动 requeue 不引入**：CMA 队列重投与其 work item 可重跑（沙箱内工具调用）匹配；我们的红线防的是外部副作用双花——注意细微差别：P0 的离线 git + receipt 化副作用其实幂等，红线表述不因 P0 安全而松动。
*   **webhook 深度检查后置**：与 Anthropic 一致（intake 快 ack、提示注入检查放 run 内）——已纳入 0001 飞书 Source 设计。

## 回馈到现有文档

*   0001 §6（Store）：P1 增 `timers` 表 + Report `follow_up_at` 约定（G1）。
*   0002 §3：无需改——延后调用/审批单例与 Anthropic deferred call 同构，互证不改设计。
*   0003：G2 的记忆文件约定落 workspace 时并入 0003 的文件契约节。

## Additional Resources

*   https://claude.com/resources/articles/building-always-on-agents （及两个中文译本，即用户提供的前后两篇链接）
*   火山引擎 Managed Agents / Open Muse：https://mp.weixin.qq.com/s/7sA7rSfsGMWz4vMPQsQFag
*   **agent-substrate/always-on-agent**（presence/cognition split 参考实现：hermes 舰队 + openclaw，含实测数字）：https://github.com/agent-substrate/always-on-agent
*   **agent-substrate/claude-managed-agents**（Claude MA 自托管沙箱 × substrate actor：poll&ack/queue/reconcile/lease heartbeat 生产化实现 + benchmarks）：https://github.com/agent-substrate/claude-managed-agents
*   Anthropic 关联文：Building effective agents、Claude Code auto mode、components of agent memory（记忆细读候选）

--------------------------------------------------------------------------------

*Drafted with EGM*
