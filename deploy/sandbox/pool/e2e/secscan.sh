#!/bin/bash
# secscan.sh <pod-ip> : scans env, args, and files of a running pod for the long-lived secrets. Prints names / counts only.
. "$(dirname "$0")/lib.sh"
umask 077
IP=$1; OUTD=${EV:-/tmp/pool21-ev}/scan-$IP; rm -rf $OUTD; mkdir -p $OUTD
PN=$(K get pod -o json | jq -r --arg ip $IP '.items[]|select(.status.podIP==$ip)|.metadata.name')
# secrets (values never printed)
printf '%s\n' "$POOL21_LITELLM_MASTER_KEY" "$BOT_PAT" "$(tr -d '\n' <$P21/anthropic.key)" "$(cat $P21/gitlab.token)" "$POOL21_DB_PASSWORD" "$POOL21_JWT_SECRET" "$JWT_ALICE" "$JWT_BOB" "$JWT_BOT" | grep -v '^$' > $OUTD/.pats
names=(master-key bot-pat anthropic-key gitlab-token db-password jwt-secret jwt-alice jwt-bob jwt-bot)
# node-side process dump: all processes of the pod's cgroup
CID=$(K get pod $PN -o jsonpath='{.status.containerStatuses[0].containerID}' | sed 's|.*://||')
minikube -p pool21 ssh -- "sudo sh -c 'for p in \$(cat \$(find /sys/fs/cgroup -path \"*$CID*\" -name cgroup.procs | head -1)); do echo \"=== \$p \$(stat -c %U /proc/\$p) \$(tr \"\\0\" \" \" </proc/\$p/cmdline | cut -c1-120)\"; echo ENV; tr \"\\0\" \"\\n\" </proc/\$p/environ; echo ARGS; tr \"\\0\" \"\\n\" </proc/\$p/cmdline; done'" 2>/dev/null | tr -d '\r' > $OUTD/procs.txt
grep '^=== ' $OUTD/procs.txt | cut -c1-150
echo "--- env var NAMES per process"
awk '/^=== /{h=$2" "$4" "$5} /^ENV$/{m=1;next} /^ARGS$/{m=0} m&&/=/{split($0,a,"="); n[h]=n[h]" "a[1]} END{for(k in n) print k": "n[k]}' $OUTD/procs.txt | cut -c1-700
# files
K exec $PN -- tar -C / -cf - work home/agent tmp etc/multica-sandbox 2>/dev/null > $OUTD/files.tar; mkdir -p $OUTD/files; tar -xf $OUTD/files.tar -C $OUTD/files 2>/dev/null
echo "--- file count: $(find $OUTD/files -type f | wc -l)"
i=0; while read -r pat; do n=${names[$i]}; ce=$(grep -cF -- "$pat" $OUTD/procs.txt); cf=$(grep -rlF -- "$pat" $OUTD/files 2>/dev/null | wc -l); echo "secret $n: in process env/args=$ce lines, in files=$cf"; i=$((i+1)); done < <(grep -v '^$' $OUTD/.pats)
