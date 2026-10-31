#!/bin/bash
# gitpolicy.sh <pod-ip> <task-id> : from a leased pod, use the task's gateway token (read on the node, never printed) to try pushes
. "$(dirname "$0")/lib.sh"
IP=$1; TID=$2
PN=$(K get pod -o json | jq -r --arg ip $IP '.items[]|select(.status.podIP==$ip)|.metadata.name')
CID=$(K get pod $PN -o jsonpath='{.status.containerStatuses[0].containerID}' | sed 's|.*://||')
TOK=$(minikube -p pool21 ssh -- "sudo sh -c 'for p in \$(cat \$(find /sys/fs/cgroup -path \"*$CID*\" -name cgroup.procs | head -1)); do tr \"\\0\" \"\\n\" </proc/\$p/environ 2>/dev/null | grep ^GIT_GATEWAY_TOKEN= ; done | head -1'" 2>/dev/null | tr -d '\r' | cut -d= -f2-)
[ -n "$TOK" ] || { echo "no token"; exit 1; }
echo "token length ${#TOK} (not printed)"
GW=http://git-gateway.multica-sandbox.svc.cluster.local:8081
K exec -i $PN -- sh -s "$GW" "$TID" <<<'' >/dev/null 2>&1
printf '%s' "$TOK" | K exec -i $PN -- sh -c '
read -r T; GW='"$GW"'; TID='"$TID"'
rm -rf /tmp/gp && mkdir /tmp/gp && cd /tmp/gp
H="Authorization: Basic $(printf agent:%s "$T" | base64 -w0)"
git -c http.extraHeader="$H" clone -q $GW/demo/app.git app 2>&1 | tail -2; cd app
git -c user.name=a -c user.email=a@x commit -q --allow-empty -m policy-test
for ref in refs/heads/main refs/heads/agent/not-my-task/x refs/heads/agent/$TID/policy-test; do echo "== push HEAD:$ref"; git -c http.extraHeader="$H" push origin HEAD:$ref 2>&1 | tail -4 | sed "s/$T/<tok>/g"; done
echo "== clone readonly (bob only; alice must be refused)"; cd /tmp/gp; git -c http.extraHeader="$H" clone -q $GW/demo/readonly.git ro 2>&1 | tail -2
echo "== no token"; git clone -q $GW/demo/app.git noauth 2>&1 | tail -2
rm -rf /tmp/gp'
