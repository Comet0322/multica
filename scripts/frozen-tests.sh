#!/usr/bin/env bash
# Frozen acceptance tests: fingerprint them, then check an MR left them alone
# and actually ran and passed them.
#
#   frozen-tests.sh gen   <repo> [out-dir, default <repo>/company/ci]
#       Reads <repo>/company/customizations.yaml (needs yq), writes
#       frozen-files.txt and frozen-tests.sha256. A person runs it after
#       reviewing a test change, with MULTICA_FROZEN_UNLOCK=1 if an agent
#       session does it; committing the new fingerprints is the approval.
#
#   frozen-tests.sh check <dir-with-fingerprints> <reports-dir>
#       Run from the repo root in CI. Fails if a frozen file changed, or if a
#       frozen test did not run and pass. Expects in <reports-dir>:
#       go.json (go test -json / gotestsum --jsonfile) and *.vitest.json
#       (vitest --reporter=json).
set -euo pipefail
mode=${1:?usage: frozen-tests.sh gen|check ...}

case "$mode" in
gen)
  repo=$(cd "${2:?repo}" && pwd); out=${3:-$repo/company/ci}
  mkdir -p "$out"; out=$(cd "$out" && pwd)
  list=$(cd "$repo"
   { yq -r '.features[].acceptance[]' company/customizations.yaml
     echo company/customizations.yaml
     ls packages/*/vitest.config.* apps/*/vitest.config.* playwright*.config.ts 2>/dev/null || true
   } | sort -u)
  # An acceptance test listed in the inventory but absent from the tree is a
  # carry-over mistake (IMPLEMENT.md step 0); fail before touching the files.
  missing=$(cd "$repo" && while read -r f; do [ -f "$f" ] || echo "$f"; done <<<"$list")
  if [ -n "$missing" ]; then
    echo "listed in customizations.yaml but missing from the tree:"; echo "$missing"; exit 1
  fi
  sums=$(cd "$repo" && xargs sha256sum <<<"$list")
  printf '%s\n' "$list" > "$out/frozen-files.txt"
  printf '%s\n' "$sums" > "$out/frozen-tests.sha256"
  echo "wrote $(wc -l < "$out/frozen-files.txt") entries to $out"
  ;;
check)
  fp=${2:?fingerprint dir}; reports=${3:?reports dir}
  if ! sha256sum -c --quiet "$fp/frozen-tests.sha256"; then
    echo "frozen tests changed: needs approval (a person regenerates company/ci/frozen-tests.sha256 with frozen-tests.sh gen)"; exit 1
  fi
  bad=0
  for f in $(grep '_test\.go$' "$fp/frozen-files.txt" || true); do
    for t in $(grep -oP '^func \KTest\w+' "$f" | grep -vx TestMain); do
      jq -e --arg t "$t" 'select(.Test==$t and .Action=="pass")' "$reports/go.json" >/dev/null \
        || { echo "did not run or pass: $f $t"; bad=1; }
    done
  done
  for f in $(grep -E '\.(test|spec)\.tsx?$' "$fp/frozen-files.txt" | grep -v '^e2e/\|/e2e/' || true); do
    jq -se --arg f "$PWD/$f" \
      '[.[].testResults[] | select(.name==$f)] | length>0 and all(.[];
         .status=="passed" and (.assertionResults|length>0) and all(.assertionResults[]; .status=="passed"))' \
      "$reports"/*.vitest.json >/dev/null || { echo "did not run or pass: $f"; bad=1; }
  done
  [ $bad = 0 ] && echo "frozen tests: unchanged, ran and passed"
  exit $bad
  ;;
*) echo "unknown mode $mode"; exit 2 ;;
esac
