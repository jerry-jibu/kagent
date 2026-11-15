# kagent-bridge — agentfs MCP 的 streamable-HTTP 面

真实组件（2026-11-15 入库——原实现只在构建机上，从未入库；PR #11 e2e 前补齐）：

- 上游 = **真 agentfs 引擎**（`agentfs mcp`，stdio，来自 jibudata/agent-data-workspace）：
  workspace 数据、checkpoint/rollback、审计账本全部由引擎持有；
- 本进程只做传输适配：HTTP MCP 会话 ↔ stdio 子进程（initialize 本地合成、
  notifications 202 无响应、tools/call 按 id 关联转发、DELETE/空闲回收会话）；
- 部署形态见 deploy/kagent/agentfs.yaml（Pod + Service + RemoteMCPServer
  agenticfs-mcp），编排器（localhost:9765 端口转发）与 worker actor（集群内
  DNS）经同一 Pod 访问同一份数据。

构建（双 host 二进制 + 最小运行时镜像）：

```bash
cd ~/src/github.com/jibudata/agent-data-workspace && CGO_ENABLED=0 go build -o /tmp/agentfs ./cmd/agentfs
cd <kagent>/deploy/kagent-bridge && CGO_ENABLED=0 go build -o /tmp/kagent-bridge .
cp /tmp/agentfs /tmp/kagent-bridge .
docker buildx build --push --platform linux/amd64 \
  --builder kagent-builder-v0.23.0 \
  -t localhost:5001/kagent-dev/kagent-bridge:dev .
```
