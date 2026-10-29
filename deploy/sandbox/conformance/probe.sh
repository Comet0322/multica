#!/bin/sh
# Conformance probe: stands in for the sandbox image, reports what the
# sandbox looks like from inside to the conformance suite, then idles until
# stopped. See deploy/sandbox/CONTRACT.md.
set -u

marker=/data/conformance-marker
previous="$(cat "$marker" 2>/dev/null || echo none)"
volume=fail
echo "$CONFORMANCE_NONCE" >"$marker" 2>/dev/null && volume=ok
tmp=fail
touch /tmp/conformance 2>/dev/null && tmp=ok
rootfs=read-only
touch /conformance-probe 2>/dev/null && rootfs=writable
egress=not-checked
if [ -n "${CONFORMANCE_EGRESS_URL:-}" ]; then
	egress=blocked
	wget -q -T 5 -O /dev/null "$CONFORMANCE_EGRESS_URL" 2>/dev/null && egress=open
fi
# The echoed variable has a name the service cannot know in advance, which
# proves env is passed through without filtering by name.
echo_value="$(eval "printf '%s' \"\${$CONFORMANCE_ECHO_VAR:-}\"")"
status_field() { awk -v k="$1:" '$1 == k { print $2 }' /proc/self/status; }

body=$(printf '{"key":"%s","nonce":"%s","echo":"%s","previous":"%s","uid":"%s","volume":"%s","tmp":"%s","rootfs":"%s","egress":"%s","cap_bnd":"%s","no_new_privs":"%s"}' \
	"$CONFORMANCE_KEY" "$CONFORMANCE_NONCE" "$echo_value" "$previous" "$(id -u)" "$volume" "$tmp" "$rootfs" "$egress" \
	"$(status_field CapBnd)" "$(status_field NoNewPrivs)")
until wget -q -T 10 -O /dev/null --header 'Content-Type: application/json' --post-data "$body" "$CONFORMANCE_CALLBACK/report"; do
	sleep 2
done

trap 'exit 0' TERM INT
while :; do
	sleep 3600 &
	wait $!
done
