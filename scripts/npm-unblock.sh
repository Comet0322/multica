#!/usr/bin/env bash
# Inside the company: make the lockfile installable from a registry whose
# security policy blocks some package versions (HTTP 403 on the tarball).
# For each blocked name@version, pins the nearest fetchable version of the same
# major with a pnpm override, regenerates the lockfile, and tries again.
#
#   npm_config_registry=<company npm mirror> bash npm-unblock.sh [record-file]
#
# Run from the repo root with a fresh store (pnpm reads cached tarballs
# without asking the registry, which hides blocks). Changes package.json
# (pnpm.overrides) and pnpm-lock.yaml, and appends each pin to the record file
# (default company/npm-pins.txt) as `name@blocked -> replacement`.
set -euo pipefail
record=${1:-company/npm-pins.txt}
: "${npm_config_registry:?set npm_config_registry to the company npm mirror}"
export npm_config_store_dir=${npm_config_store_dir:-$(mktemp -d)/store}
mkdir -p "$(dirname "$record")"

semver_key() { # 1.2.3[-pre] -> sortable key; pre-releases are skipped by the caller
  awk -F. '{printf "%06d%06d%06d\n", $1, $2, $3}' <<<"$1"
}

pick() { # pick NAME BLOCKED -> nearest fetchable version, same major
  local name=$1 bad=$2 maj=${2%%.*} min
  min=$(cut -d. -f2 <<<"$bad")
  local all
  all=$(npm view "$name" versions --json 2>/dev/null | tr -d '[]", ' | tr '\n' ' ')
  # Candidates: same major, no pre-release, not the blocked one; ordered
  # same minor upward, same minor downward, newer minors, older minors.
  local cands
  cands=$(for v in $all; do
      [[ $v =~ ^$maj\.[0-9]+\.[0-9]+$ ]] && [ "$v" != "$bad" ] || continue
      m=$(cut -d. -f2 <<<"$v")
      if   [ "$m" = "$min" ] && [ "$(semver_key "$v")" \> "$(semver_key "$bad")" ]; then g=1
      elif [ "$m" = "$min" ]; then g=2
      elif [ "$m" -gt "$min" ]; then g=3
      else g=4; fi
      k=$(semver_key "$v"); [ $g = 2 ] || [ $g = 4 ] && k=$(printf '%018d' $((999999999999999999 - 10#$k)))
      echo "$g $k $v"
    done | sort | awk '{print $3}')
  for v in $cands; do
    if npm pack --dry-run --silent "$name@$v" >/dev/null 2>&1; then echo "$v"; return; fi
  done
}

override() { # override KEY VALUE; empty VALUE removes the key
  node -e "const fs=require('fs');const p=JSON.parse(fs.readFileSync('package.json'));
    p.pnpm=p.pnpm||{};p.pnpm.overrides=p.pnpm.overrides||{};
    if (process.argv[2]) p.pnpm.overrides[process.argv[1]]=process.argv[2]; else delete p.pnpm.overrides[process.argv[1]];
    fs.writeFileSync('package.json',JSON.stringify(p,null,2)+'\n')" "$1" "$2"
}
in_lock() { grep -qE "^  '?$1@$2(\(|'?:)" pnpm-lock.yaml; }
seen=$(mktemp)

for i in $(seq 40); do
  log=$(mktemp)
  if pnpm install --frozen-lockfile --ignore-scripts >"$log" 2>&1; then
    echo "Done: lockfile installs from $npm_config_registry"; exit 0
  fi
  # ERR_PNPM_FETCH_403  GET <registry>/<name>/-/<file>-<version>.tgz: Forbidden - 403
  url=$(grep 'FETCH_403' "$log" | grep -oE 'https?://[^ ]+\.tgz' | head -1 || true)
  if [ -z "$url" ]; then
    echo "install failed for another reason:"; grep -E 'ERR_PNPM|ERR!' "$log" | head -5; exit 1
  fi
  path=${url#"${npm_config_registry%/}"/}
  name=${path%%/-/*}; name=${name//%2[fF]//}
  file=${path##*/}; ver=${file%.tgz}; ver=${ver##*-}
  [[ $ver =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || ver=$(sed -E 's/.*-([0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?)\.tgz$/\1/' <<<"$file")
  to=$(pick "$name" "$ver")
  if [ -z "$to" ]; then echo "blocked: $name@$ver, and no other $name@${ver%%.*}.x is fetchable"; exit 1; fi
  echo "blocked: $name@$ver -> $to"
  if grep -qxF "$name@$ver -> $to" "$seen" 2>/dev/null; then
    echo "the pin for $name@$ver did not take effect; stopping"; exit 1
  fi
  echo "$name@$ver -> $to" >>"$seen"
  override "$name@$ver" "$to"
  pnpm install --lockfile-only >/dev/null
  if in_lock "$name" "$ver"; then
    # A catalog entry (pnpm-workspace.yaml) is resolved apart from overrides
    # keyed by version; a bare-name override reaches it. Only when every
    # resolved version of the package has the same major, so nothing else moves
    # across a major.
    majors=$(grep -oE "^  '?${name//\//\\/}@[0-9]+" pnpm-lock.yaml | sed -E 's/.*@//' | sort -u)
    if [ "$(wc -l <<<"$majors")" != 1 ]; then
      echo "$name@$ver is still resolved and $name is used in several majors ($(echo $majors)); a person decides"; exit 1
    fi
    override "$name@$ver" ""
    override "$name" "$to"
    pnpm install --lockfile-only >/dev/null
    in_lock "$name" "$ver" && { echo "$name@$ver is still resolved after a bare-name override; a person decides"; exit 1; }
    echo "$name@$ver -> $to (bare-name override)" >>"$record"
  else
    echo "$name@$ver -> $to" >>"$record"
  fi
done
echo "gave up after 40 pins"; exit 1
