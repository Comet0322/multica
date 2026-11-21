#!/bin/sh
# Write a build copy of an upstream Dockerfile for the company network; the
# repo's file stays untouched. Runs in Kaniko's busybox shell.
#   sh company/ci/prepare-dockerfile.sh Dockerfile .ci-Dockerfile
# 1. FROM <upstream> -> FROM <company registry path>, using the
#    UPSTREAM_IMG_X / IMG_X pairs from company/ci/images.yml.
# 2. After every FROM, declare GOPROXY, npm_config_registry and
#    COREPACK_NPM_REGISTRY as ARGs, so the RUN steps that fetch Go modules,
#    npm packages and pnpm use the mirrors; ARGs do not end up in the image.
# 3. With APK_MIRROR set, point apk at it in every Alpine stage.
# Fails if a FROM is left that images.yml does not map: a new upstream version
# changed a base image, and required-images.sh needs rerunning.
set -eu
src=$1 dst=$2
cp "$src" "$dst"
env | grep '^UPSTREAM_IMG_' | while IFS='=' read -r key up; do
  img=$(eval "printf '%s' \"\${${key#UPSTREAM_}:-}\"")
  [ -n "$img" ] || continue
  awk -v up="$up" -v img="$img" '$1=="FROM" && $2==up {$2=img} {print}' "$dst" > "$dst.tmp" && mv "$dst.tmp" "$dst"
done
awk '{print} $1=="FROM" {
  print "ARG GOPROXY"; print "ARG npm_config_registry"; print "ARG COREPACK_NPM_REGISTRY"; print "ARG APK_MIRROR"
  print "RUN if [ -n \"$APK_MIRROR\" ] && [ -f /etc/apk/repositories ]; then sed -i \"s#https\\?://dl-cdn.alpinelinux.org/alpine#${APK_MIRROR%/}#\" /etc/apk/repositories; fi"
}' "$dst" > "$dst.tmp" && mv "$dst.tmp" "$dst"
bad=
for i in $(awk '$1=="FROM"{print $2}' "$dst"); do
  env | grep -q "^IMG_[A-Z_]*=$i\$" || bad="$bad $i"
done
if [ -n "$bad" ]; then echo "$src: FROM not mapped in company/ci/images.yml:$bad" >&2; exit 1; fi
echo "$dst: $(awk '$1=="FROM"{printf "%s ", $2}' "$dst")"
