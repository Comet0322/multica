# Allowlisting HTTP(S) forward proxy: the only way out for sandboxes on the
# internal network. Build from this directory:
#   docker build -t multica-sandbox-egress deploy/sandbox/egress
FROM alpine:3.21
RUN apk add --no-cache tinyproxy
COPY entrypoint.sh /usr/local/bin/egress-entrypoint
USER tinyproxy
EXPOSE 3128
HEALTHCHECK --interval=30s --timeout=5s CMD ["nc", "-z", "127.0.0.1", "3128"]
ENTRYPOINT ["/usr/local/bin/egress-entrypoint"]
