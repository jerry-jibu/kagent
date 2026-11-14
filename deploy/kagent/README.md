# deploy/kagent — superagent-on-kagent P0 部署清单

设计依据：[docs/designs/0002-deepseek-harness-落地与对外使用面设计.md](../docs/designs/0002-deepseek-harness-落地与对外使用面设计.md)。
基线：kagent v1.0.1 集群（env.md），`exp/deepseek-superagent` 分支。

## 清单

| 文件 | 资源 | 说明 |
|---|---|---|
| `modelconfig-deepseek.yaml` | ModelConfig `deepseek` | Anthropic 兼容网关；复用既有 Secret `kagent-deepseek` |
| `harness-dsh-byo.yaml` | Harness `dsh-byo` | BYO 契约；`${DSH_BYO_IMAGE}`、`${SNAPSHOT_LOCATION}` 需代入 |
| `agent-worker.yaml` | Agent `superagent-worker` | inline template + agentfs MCP 绑定 |
| `agent-evaluator.yaml` | Agent `superagent-evaluator` | 内置 `kagent-default` harness + `outputSchema` 结构化 Decision |

## 前置

1. **Secret**（已存在，env.md）：`kubectl -n kagent get secret kagent-deepseek`。
2. **DSH BYO 镜像**：源码 = `kagent-dev/byo-ext` PR #2（未合入 main）：

   ```bash
   git -C /path/to/byo-ext fetch origin pull/2/head:pr-2 && git -C /path/to/byo-ext checkout pr-2
   # 记录源 SHA（当前 19771c8）
   docker build -f harnesses/deepseek/Dockerfile -t <registry>/kagent-dsh-byo .
   docker push <registry>/kagent-dsh-byo   # 取 digest
   ```

   把 `<registry>/kagent-dsh-byo@sha256:<64hex>` 记入构建台账（源 SHA → digest）。

3. **SNAPSHOT_LOCATION**：与集群既有 Harness 的快照位置约定对齐
   （参照：`kubectl -n kagent get harness -o yaml | grep -A2 snapshotPolicy`）。

## 应用

```bash
export DSH_BYO_IMAGE='<registry>/kagent-dsh-byo@sha256:<64hex>'
export SNAPSHOT_LOCATION='<cluster-convention>'
for f in modelconfig-deepseek harness-dsh-byo agent-worker agent-evaluator; do
  envsubst < $f.yaml | kubectl apply -f -
done
```

（macOS：`brew install gettext` 提供 envsubst。）

## 验证（CUJ1/CUJ2，0002）

```bash
kubectl -n kagent get modelconfig deepseek harness dsh-byo agent superagent-worker superagent-evaluator
```

- UI `/agents` Harnesses tab 列出 `dsh-byo`（引用 agent 只出现在删除确认文案）。
- 开 `superagent-worker` 对话：流式应答走 DeepSeek 模型；`kubectl -n kagent logs deploy/kagent-controller` 无编译错误。
- bridge 沙箱零凭证：actor 日志无 API key passthrough（PR #2 启动即拒）。
