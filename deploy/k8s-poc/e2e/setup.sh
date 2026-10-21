#!/usr/bin/env bash
# Builds the images from this checkout, creates the kind cluster, installs Agent
# Sandbox and deploys server + Postgres + fake LiteLLM + runtime controller.
# Every kubectl call pins the kind-multica-mr-e2e context; other clusters are never touched.
set -euo pipefail
cd "$(dirname "$0")"
REPO=$(cd ../../.. && pwd)
CLUSTER=multica-mr-e2e
KCTX=kind-$CLUSTER
NODE_IMAGE=${KIND_NODE_IMAGE:-kindest/node:local}
SANDBOX_VERSION=${SANDBOX_VERSION:-v1.0.7}
k() { kubectl --context "$KCTX" "$@"; }

echo "== images"
docker build -q -t mr-e2e/multica:test --build-arg VERSION=e2e -f "$REPO/Dockerfile" "$REPO" >/dev/null
docker build -q -t mr-e2e/daemon:test -f ../Dockerfile.e2e-daemon .. >/dev/null
docker build -q -t mr-e2e/controller:test -f "$REPO/deploy/runtime-controller/Dockerfile" "$REPO" >/dev/null
docker build -q -t mr-e2e/fake-litellm:test fake-litellm >/dev/null

echo "== cluster"
kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --wait 180s
for i in mr-e2e/multica:test mr-e2e/daemon:test mr-e2e/controller:test mr-e2e/fake-litellm:test pgvector/pgvector:pg17; do
  kind load docker-image "$i" --name "$CLUSTER" >/dev/null
done

echo "== agent-sandbox $SANDBOX_VERSION"
k apply --server-side -f ../vendor/sandbox-with-extensions.yaml >/dev/null
k -n agent-sandbox-system rollout status deploy/agent-sandbox-controller --timeout=180s

echo "== platform"
k apply -f e2e.yaml >/dev/null
k -n mr-e2e wait --for=condition=available deploy/postgres deploy/server deploy/litellm --timeout=240s
k apply -f controller.yaml >/dev/null
k -n mr-e2e wait --for=condition=available deploy/runtime-controller --timeout=120s
echo "ready"
