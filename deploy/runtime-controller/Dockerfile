# Build from the repository root:
#   docker build -f deploy/runtime-controller/Dockerfile -t multica-runtime-controller .
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY server/go.mod server/go.sum ./server/
RUN cd server && go mod download
COPY server/ ./server/
RUN cd server && CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/runtime-controller ./cmd/runtime-controller

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 controller
COPY --from=builder /out/runtime-controller /usr/local/bin/runtime-controller
USER 10001
ENTRYPOINT ["/usr/local/bin/runtime-controller"]
