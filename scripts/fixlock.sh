#!/usr/bin/env bash
# Regenerate pnpm-lock.yaml, pinning trust-downgraded or too-young versions
# to the newest version in the same major that has provenance and is 7+ days old.
pick() { # name major exclude-version
  npm view "$1" time --json 2>/dev/null | python3 -c "
import json,sys,datetime,re,subprocess
t=json.load(sys.stdin);cut=datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(days=7,hours=6)
c=[k for k,v in t.items() if re.fullmatch(r'$2\.\d+\.\d+',k) and k!='$3' and datetime.datetime.fromisoformat(v.replace('Z','+00:00'))<cut]
c.sort(key=lambda s:tuple(map(int,s.split('.'))),reverse=True)
need_prov='$4'=='prov'
for v in c[:15]:
    if not need_prov: print(v);break
    p=subprocess.run(['npm','view','$1@'+v,'dist.attestations.provenance.predicateType'],capture_output=True,text=True).stdout.strip()
    if p: print(v);break
"
}
for i in $(seq 40); do
  out=$(pnpm install --lockfile-only 2>&1 | grep -v '^Progress')
  if echo "$out" | grep -q TRUST_DOWNGRADE; then
    set -- $(echo "$out" | sed -n 's/.*trust downgrade for "\(.*\)@\([0-9][^"]*\)".*/\1 \2/p'); mode=prov
  elif echo "$out" | grep -q NO_MATURE; then
    set -- $(echo "$out" | sed -n 's/.*Version \([^ ]*\) (released.*) of \([^ ]*\) does not.*/\2 \1/p'); mode=age
  else echo "$out" | grep -E 'ERR_|Done' | head -3; exit 0; fi
  name=$1; ver=$2; maj=${ver%%.*}
  to=$(pick "$name" "$maj" "$ver" "$mode")
  echo "$mode: $name@$ver -> ${to:-NONE}"
  if [ -z "$to" ] && [ "$mode" = prov ]; then
    grep -q '^trustPolicyExclude:' pnpm-workspace.yaml || printf 'trustPolicyExclude:\n' >> pnpm-workspace.yaml
    grep -q "^  - '$name@$ver'$" pnpm-workspace.yaml || sed -i "/^trustPolicyExclude:/a\\  - '$name@$ver'" pnpm-workspace.yaml
    echo "  excluded $name@$ver from the trust policy"; continue
  fi
  [ -z "$to" ] && { echo "$out" | head -8; exit 1; }
  node -e "const fs=require('fs');const p=JSON.parse(fs.readFileSync('package.json'));p.pnpm.overrides['$name@$ver']='$to';fs.writeFileSync('package.json',JSON.stringify(p,null,2)+'\n')"
done
