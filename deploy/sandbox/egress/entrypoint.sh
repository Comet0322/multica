#!/bin/sh
# Writes a deny-by-default tinyproxy config from SANDBOX_EGRESS_ALLOW, a
# comma-separated list of domains; each also allows its subdomains.
set -eu

: "${SANDBOX_EGRESS_ALLOW:?list the domains sandboxes may reach}"
conf=/tmp/tinyproxy.conf
filter=/tmp/allow.txt

: >"$filter"
for domain in $(printf '%s' "$SANDBOX_EGRESS_ALLOW" | tr ',' ' '); do
	escaped="$(printf '%s' "$domain" | sed 's/[.]/\\./g')"
	printf '(^|\\.)%s$\n' "$escaped" >>"$filter"
done

cat >"$conf" <<CONF
Port 3128
Listen 0.0.0.0
Timeout 600
MaxClients 200
LogLevel ${SANDBOX_EGRESS_LOG_LEVEL:-Notice}
DisableViaHeader Yes
# Match the destination host only, as an extended regex.
Filter "$filter"
FilterType ere
FilterURLs Off
FilterDefaultDeny Yes
# CONNECT (HTTPS tunnels) only to the TLS port.
ConnectPort 443
CONF

echo "egress allowlist: $SANDBOX_EGRESS_ALLOW"
exec tinyproxy -d -c "$conf"
