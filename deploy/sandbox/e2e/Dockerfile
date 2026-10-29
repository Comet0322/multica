# End-to-end test image: a prebuilt multica binary and a fake Claude Code.
FROM node:22-slim
COPY multica /usr/local/bin/multica
COPY fake-claude /usr/local/bin/claude
COPY entrypoint.sh /usr/local/bin/multica-sandbox-entrypoint
COPY --chown=node:node skill/ /home/node/
# A fresh named volume inherits this ownership.
RUN mkdir -p /data && chown node:node /data
USER node
ENV HOME=/home/node
VOLUME /data
ENTRYPOINT ["/usr/local/bin/multica-sandbox-entrypoint"]
