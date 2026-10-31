P21=${P21:?directory with env, state, gitlab.token, anthropic.key (outside the repository)}
set -a; . $P21/env; . $P21/state; set +a
API=http://127.0.0.1:18121

api() { local jwt=$1 m=$2 p=$3 ws=$4; shift 4; curl -sS -X $m "$API$p" -H "Authorization: Bearer $jwt" -H "X-Workspace-ID: $ws" -H 'Content-Type: application/json' "$@"; }
LL() { docker exec pool21-litellm-1 python3 -c "
import urllib.request,sys,os,json
req=urllib.request.Request('http://localhost:4000'+sys.argv[2],method=sys.argv[1],headers={'Authorization':'Bearer '+os.environ['LITELLM_MASTER_KEY'],'Content-Type':'application/json'},data=(sys.argv[3].encode() if len(sys.argv)>3 else None))
print(urllib.request.urlopen(req).read().decode())" "$@"; }
K() { kubectl --context pool21 -n multica-sandbox "$@"; }
