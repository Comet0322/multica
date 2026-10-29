# Sandbox image: the stock Multica daemon plus agent CLIs. Build from the
# repository root with the same commit as the broker:
#   docker build -f deploy/sandbox/Dockerfile -t multica-sandbox:<tag> .
# The default target installs real agent CLIs; --target fake uses a fake
# Claude Code for credential-free testing.
ARG GO_IMAGE=golang:1.27.3-alpine
ARG BASE_IMAGE=node:22-slim

FROM ${GO_IMAGE} AS builder
WORKDIR /src/server
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/multica ./cmd/multica

FROM ${BASE_IMAGE} AS base
RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates git openssh-client \
	&& rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/multica /usr/local/bin/multica
COPY deploy/sandbox/entrypoint.sh /usr/local/bin/multica-sandbox-entrypoint
# Runtime-private skills, MCP servers and CLI settings. Agents load them from
# their provider home directories; the broker reports them unsupported so the
# Multica UI never lists them.
COPY --chown=node:node deploy/sandbox/home/ /home/node/
# A fresh named volume inherits this ownership.
RUN mkdir -p /data && chown node:node /data
ENV HOME=/home/node
VOLUME /data
ENTRYPOINT ["/usr/local/bin/multica-sandbox-entrypoint"]

FROM base AS fake
COPY deploy/sandbox/e2e/fake-claude /usr/local/bin/claude
COPY --chown=node:node deploy/sandbox/e2e/skill/ /home/node/
USER node

FROM base AS agents
# Space-separated npm packages providing the agent CLIs to expose.
ARG AGENT_PACKAGES="@anthropic-ai/claude-code"
RUN if [ -n "$AGENT_PACKAGES" ]; then npm install -g $AGENT_PACKAGES && npm cache clean --force; fi
USER node
