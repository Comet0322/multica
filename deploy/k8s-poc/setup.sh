#!/usr/bin/env bash
# Rebuilds the whole PoC from scratch: kind cluster, images, Agent Sandbox,
# server + Postgres, fixtures, and one daemon Sandbox. Never touches other clusters:
# every kubectl call pins --context kind-multica-mr-test.
set -euo pipefail
cd "$(dirname "$0")"
REPO=$(cd ../.. && pwd)
CLUSTER=multica-mr-test
export KCTX=kind-$CLUSTER
SANDBOX_VERSION=${SANDBOX_VERSION:-v1.0.7}
. ./lib.sh

echo "== images"
# Build only when missing (FORCE_BUILD=1 rebuilds); the base images come from Docker Hub.
if [ -n "${FORCE_BUILD:-}" ] || ! docker image inspect mr-poc/multica:v0.6.1 >/dev/null 2>&1; then
  docker build -q -t mr-poc/multica:v0.6.1 --build-arg VERSION=v0.6.1 -f "$REPO/Dockerfile" "$REPO" >/dev/null
fi
if [ -n "${FORCE_BUILD:-}" ] || ! docker image inspect mr-poc/daemon:v0.6.1 >/dev/null 2>&1; then
  docker build -q -t mr-poc/daemon:v0.6.1 -f Dockerfile.daemon . >/dev/null
fi

echo "== cluster"
kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s
for i in mr-poc/multica:v0.6.1 mr-poc/daemon:v0.6.1 pgvector/pgvector:pg17; do
  kind load docker-image "$i" --name "$CLUSTER" >/dev/null
done

echo "== agent-sandbox $SANDBOX_VERSION"
mkdir -p vendor
[ -f vendor/sandbox-with-extensions.yaml ] || gh release download "$SANDBOX_VERSION" -R kubernetes-sigs/agent-sandbox \
  -p sandbox-with-extensions.yaml -D vendor
k apply --server-side -f vendor/sandbox-with-extensions.yaml >/dev/null
k -n agent-sandbox-system rollout status deploy/agent-sandbox-controller --timeout=120s

echo "== server + postgres"
k apply -f base.yaml >/dev/null
k -n mr-poc wait --for=condition=available deploy/postgres deploy/server --timeout=180s

echo "== port-forward :18900 + fixtures"
pkill -f "port-forward svc/server 18900" 2>/dev/null || true
nohup kubectl --context "$KCTX" -n mr-poc port-forward svc/server 18900:8080 >/tmp/mr-poc-pf.log 2>&1 &
sleep 3
./bootstrap.sh >/dev/null
. ./state.env
k -n mr-poc create secret generic daemon-a --from-literal=token="$PAT_admin_a" --dry-run=client -o yaml | k apply -f - >/dev/null

echo "== daemon sandbox (project X, PVC-backed)"
k apply -f sandbox-t4.yaml >/dev/null
echo "done. state in deploy/k8s-poc/state.env; API on $API"
