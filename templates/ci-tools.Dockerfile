# CI image for the Multica pipeline; copy to company/ci/ci-tools.Dockerfile.
# The OS (IMG_CI_BASE) is any Debian or Ubuntu release whose packages the
# company's apt mirror carries and whose git is >= 2.40 (Debian trixie,
# Ubuntu 24.04 or newer); Go, Node and the tools are copied in from their own
# images (IMG_* build args, from company/ci/images.yml). Building it needs the
# image registry, an apt mirror or proxy, and the Go and npm mirrors.
# Upstream tests need a non-root user (Claude Code refuses bypassPermissions as
# root; file-permission tests pass as root) and git >= 2.40 (execenv uses
# merge-tree --merge-base). The checkout is cloned by another user, so it is
# marked safe.
ARG IMG_CI_BASE=ubuntu:24.04
ARG IMG_GO_CI=golang:1.26.11-trixie
ARG IMG_NODE_CI=node:22.23.4-bookworm-slim
ARG IMG_HELM=alpine/helm:3.18.4
ARG IMG_YQ=mikefarah/yq:4.47.1
ARG IMG_COMPOSE=docker/compose-bin:v2.39.4
ARG IMG_DOCKER_CLI=docker:28.3.3-cli
FROM ${IMG_GO_CI} AS go
FROM ${IMG_NODE_CI} AS node
FROM ${IMG_HELM} AS helm
FROM ${IMG_YQ} AS yq
FROM ${IMG_COMPOSE} AS compose
FROM ${IMG_DOCKER_CLI} AS dockercli

FROM ${IMG_CI_BASE}
# APT_MIRROR: base URL that replaces the distribution's own hosts
# (deb.debian.org, security.debian.org, archive/security.ubuntu.com); the
# mirror keeps the upstream paths below it (…/debian, …/ubuntu). Empty = as is.
ARG APT_MIRROR=
ARG GOPROXY
ARG npm_config_registry
ARG PNPM=10.28.2
ARG CI_UID=1000
ENV DEBIAN_FRONTEND=noninteractive
RUN if [ -n "$APT_MIRROR" ]; then \
      sed -i -E "s#https?://(deb\.debian\.org|security\.debian\.org|([a-z]+\.)?archive\.ubuntu\.com|security\.ubuntu\.com|ports\.ubuntu\.com)#${APT_MIRROR%/}#g" \
        /etc/apt/sources.list /etc/apt/sources.list.d/* 2>/dev/null || true; \
    fi \
 && apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git jq make postgresql-client python3 xz-utils \
 && rm -rf /var/lib/apt/lists/*
COPY --from=go /usr/local/go /usr/local/go
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=yq /usr/bin/yq /usr/local/bin/yq
COPY --from=compose /docker-compose /usr/local/lib/docker/cli-plugins/docker-compose
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
ENV PATH=/usr/local/go/bin:$PATH
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
 && ln -s ../lib/node_modules/corepack/dist/corepack.js /usr/local/bin/corepack \
 && npm install -g pnpm@$PNPM
RUN GOBIN=/usr/local/bin go install gotest.tools/gotestsum@v1.13.0 && go clean -cache -modcache
# Fail here, not in the tests, when the base OS is too old.
RUN v=$(git --version | awk '{print $3}'); \
    [ "$(printf '2.40.0\n%s\n' "$v" | sort -V | head -1)" = 2.40.0 ] \
    || { echo "git $v < 2.40: pick a newer IMG_CI_BASE"; exit 1; }
# Some bases (Ubuntu 24.04) ship a user with the wanted uid; reuse the uid.
RUN git config --system --add safe.directory '*' \
 && if getent passwd "$CI_UID" >/dev/null; then userdel -r "$(getent passwd "$CI_UID" | cut -d: -f1)"; fi \
 && useradd -m -u "$CI_UID" ci && mkdir -p /cache && chown ci:ci /cache && chmod 0777 /cache
USER ci
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0 GOPATH=/home/ci/go
