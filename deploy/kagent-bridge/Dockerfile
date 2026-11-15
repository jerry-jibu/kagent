# kagent-bridge runtime — the bridge binary plus the real agentfs engine.
# Both binaries are built on the host (kind-env build pattern: out-of-tree
# builds with GOPROXY; the agentfs engine builds from
# ~/src/github.com/jibudata/agent-data-workspace):
#   cd <agent-data-workspace> && CGO_ENABLED=0 go build -o /tmp/agentfs ./cmd/agentfs
#   cd <kagent>/deploy/kagent-bridge && CGO_ENABLED=0 go build -o /tmp/kagent-bridge .
#   docker buildx build --push --platform linux/amd64 \
#     --build-arg GOPROXY=http://127.0.0.1:8000,https://goproxy.cn,direct \
#     -t localhost:5001/kagent-dev/kagent-bridge:dev .
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY kagent-bridge /usr/local/bin/kagent-bridge
COPY agentfs /usr/local/bin/agentfs
# The bridge spawns `agentfs mcp` per session; agentfs needs its home dir.
RUN mkdir -p /data/.agenticfs
EXPOSE 9765
ENTRYPOINT ["/usr/local/bin/kagent-bridge"]
