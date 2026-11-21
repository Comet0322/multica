#!/usr/bin/env bash
# Apply or check the GitLab project settings the company pipeline relies on
# (GitLab Free). Idempotent: rerunning changes nothing that already matches.
# Copy to company/ci/ with gitlab-settings.env next to it.
#
#   GITLAB_TOKEN=<Maintainer token> bash company/ci/gitlab-settings.sh apply
#   GITLAB_TOKEN=<token>            bash company/ci/gitlab-settings.sh check
#
# `check` changes nothing and exits 1 if any setting differs. The token is read
# from the environment and never printed. PG_PASSWORD is generated the first
# time and kept afterwards; a deploy token for the test environment's image
# pulls is not made here, because GitLab shows its secret only once.
set -euo pipefail
mode=${1:?usage: gitlab-settings.sh apply|check}
case "$mode" in apply|check) ;; *) echo "usage: gitlab-settings.sh apply|check" >&2; exit 2 ;; esac
: "${GITLAB_TOKEN:?set GITLAB_TOKEN to a Maintainer token}"
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=/dev/null
. "${SETTINGS_ENV:-$here/gitlab-settings.env}"
: "${GITLAB_URL:?}" "${PROJECT:?}"
command -v jq >/dev/null || { echo "needs jq" >&2; exit 2; }

API="${GITLAB_URL%/}/api/v4"
P=$(jq -rn --arg p "$PROJECT" '$p|@uri')
drift=0

api() { # api METHOD PATH [curl args...]; prints the body, fails on HTTP >= 400
  local m=$1 path=$2; shift 2
  curl -sS --fail-with-body -X "$m" -H @<(printf 'PRIVATE-TOKEN: %s\n' "$GITLAB_TOKEN") "$API$path" "$@"
}
get() { # get PATH; prints the body on 200, nothing and status 1 otherwise
  local out
  out=$(curl -sS -w '\n%{http_code}' -H @<(printf 'PRIVATE-TOKEN: %s\n' "$GITLAB_TOKEN") "$API$1") || return 1
  [ "${out##*$'\n'}" = 200 ] || return 1
  printf '%s' "${out%$'\n'*}"
}
enc() { jq -rn --arg v "$1" '$v|@uri'; }
ok()   { printf '  ok      %s\n' "$1"; }
diff_() { # diff_ WHAT HAVE WANT
  drift=1
  if [ "$mode" = check ]; then printf '  DIFFERS %s: is %s, want %s\n' "$1" "$2" "$3"
  else printf '  set     %s: %s -> %s\n' "$1" "$2" "$3"; fi
}

echo "project $PROJECT ($mode)"

# --- project settings ------------------------------------------------------
proj=$(get "/projects/$P") || { echo "cannot read project $PROJECT" >&2; exit 2; }
want_project=(
  "ci_config_path=$CI_CONFIG_PATH"
  "only_allow_merge_if_pipeline_succeeds=true"
  "allow_merge_on_skipped_pipeline=false"
  "container_registry_enabled=true"
)
[ -n "${DEFAULT_BRANCH:-}" ] && want_project+=("default_branch=$DEFAULT_BRANCH")
for kv in "${want_project[@]}"; do
  k=${kv%%=*} v=${kv#*=}
  have=$(jq -r --arg k "$k" '.[$k] | tostring' <<<"$proj")
  if [ "$have" = "$v" ]; then ok "$k"; continue; fi
  diff_ "$k" "$have" "$v"
  [ "$mode" = apply ] && api PUT "/projects/$P" --data-urlencode "$k=$v" >/dev/null
done

# --- protected branches (Maintainers push and merge, no force push) -----------
for b in $PROTECTED_BRANCHES; do
  have=$(get "/projects/$P/protected_branches/$(enc "$b")" \
    | jq -r '"push=\(.push_access_levels|map(.access_level)|max) merge=\(.merge_access_levels|map(.access_level)|max) force=\(.allow_force_push)"' \
    || echo none)
  want="push=40 merge=40 force=false"
  if [ "$have" = "$want" ]; then ok "protected branch $b"; continue; fi
  diff_ "protected branch $b" "$have" "$want"
  if [ "$mode" = apply ]; then
    [ "$have" != none ] && api DELETE "/projects/$P/protected_branches/$(enc "$b")" >/dev/null
    api POST "/projects/$P/protected_branches" --data-urlencode "name=$b" \
      --data push_access_level=40 --data merge_access_level=40 --data allow_force_push=false >/dev/null
  fi
done

# --- protected tags (CLI releases) ------------------------------------------
for t in $PROTECTED_TAGS; do
  have=$(get "/projects/$P/protected_tags/$(enc "$t")" \
    | jq -r '"create=\(.create_access_levels|map(.access_level)|max)"' || echo none)
  want="create=40"
  if [ "$have" = "$want" ]; then ok "protected tag $t"; continue; fi
  diff_ "protected tag $t" "$have" "$want"
  if [ "$mode" = apply ]; then
    [ "$have" != none ] && api DELETE "/projects/$P/protected_tags/$(enc "$t")" >/dev/null
    api POST "/projects/$P/protected_tags" --data-urlencode "name=$t" --data create_access_level=40 >/dev/null
  fi
done

# --- CI/CD variables -------------------------------------------------------
setvar() { # setvar KEY VALUE MASKED
  local k=$1 v=$2 masked=$3 have
  have=$(get "/projects/$P/variables/$k" || true)
  if [ -z "$have" ]; then
    diff_ "variable $k" missing "$([ "$masked" = true ] && echo '(masked)' || echo "$v")"
    [ "$mode" = apply ] && api POST "/projects/$P/variables" --data "key=$k" --data-urlencode "value=$v" \
      --data "masked=$masked" --data protected=false >/dev/null
    return
  fi
  local hv hm
  hv=$(jq -r '.value' <<<"$have"); hm=$(jq -r '.masked' <<<"$have")
  if [ "$masked" = true ]; then # keep an existing secret; only its masking is checked
    if [ "$hm" = true ]; then ok "variable $k (masked)"; return; fi
    diff_ "variable $k masked" "$hm" true
    [ "$mode" = apply ] && api PUT "/projects/$P/variables/$k" --data masked=true >/dev/null
    return
  fi
  if [ "$hv" = "$v" ]; then ok "variable $k"; return; fi
  diff_ "variable $k" "$hv" "$v"
  [ "$mode" = apply ] && api PUT "/projects/$P/variables/$k" --data-urlencode "value=$v" --data masked=false >/dev/null
}
setvar PG_PASSWORD "$(openssl rand -hex 16)" true
required="GOPROXY npm_config_registry COREPACK_NPM_REGISTRY"
optional="APT_MIRROR APK_MIRROR BUILD_PROXY NO_PROXY_HOSTS IMAGE_REPO PUSH_REGISTRY PUSH_USER KANIKO_EXTRA GO_TEST_P VITEST_WORKERS"
for k in $required $optional; do
  v=${!k-}
  if [ -z "$v" ]; then
    case " $required " in *" $k "*) echo "  MISSING $k in gitlab-settings.env" >&2; drift=1 ;; esac
    continue
  fi
  setvar "$k" "$v" false
done
# PUSH_PASSWORD (a registry robot account's secret) is set by hand as a masked variable.

# --- the agent account must stay a Developer --------------------------------
if [ -n "${AGENT_USER:-}" ]; then
  uid=$(get "/users?username=$(enc "$AGENT_USER")" | jq -r '.[0].id // empty' || true)
  [ -n "$uid" ] || { echo "  MISSING user $AGENT_USER" >&2; drift=1; }
  lvl=$( { [ -n "$uid" ] && get "/projects/$P/members/all/$uid"; } | jq -r '.access_level' || true)
  [ -n "$lvl" ] || lvl=none
  if [ "$lvl" = 30 ]; then ok "member $AGENT_USER is Developer"
  else
    diff_ "member $AGENT_USER access level" "$lvl" 30
    if [ "$mode" = apply ] && [ -n "$uid" ]; then
      if [ "$lvl" = none ]; then api POST "/projects/$P/members" --data "user_id=$uid" --data access_level=30 >/dev/null
      else api PUT "/projects/$P/members/$uid" --data access_level=30 >/dev/null; fi
    fi
  fi
fi

if [ "$mode" = check ]; then
  [ $drift = 0 ] && echo "all settings match" || { echo "settings differ (run apply)"; exit 1; }
else
  [ $drift = 0 ] && echo "nothing to change" || echo "applied"
fi
