# Orchestrator image (T9 deployment): the control-plane binary plus the system
# git binary — the Cloner and the delivery patcher shell out to git (T5
# contract), so the runtime stage must carry it. Mirrors for CN networks;
# swap the FROM lines for your own registry mirrors when rebuilding.
FROM docker.m.daocloud.io/library/golang:1.27.3-bookworm AS builder
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /orchestrator ./cmd/orchestrator

FROM docker.m.daocloud.io/library/debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 65532 --no-create-home --home-dir /nonexistent orchestrator
COPY --from=builder /orchestrator /usr/local/bin/orchestrator
# git runs only inside the orchestrator container (clone/push with the
# orchestrator-held token; the credential helper reads it from the
# environment — docs/designs/0003 credential monotonicity).
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/orchestrator"]
