# Probe image for the sandbox service conformance suite:
#   docker build -t <registry>/multica-sandbox-probe:<tag> deploy/sandbox/conformance
FROM alpine:3.21
RUN adduser -D -u 1000 probe && mkdir -p /data && chown 1000:1000 /data
COPY probe.sh /usr/local/bin/probe
USER 1000
VOLUME /data
ENTRYPOINT ["/usr/local/bin/probe"]
