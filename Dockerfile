# --- Build stage ---
FROM golang:1.27.3-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src/server

# Cache dependencies
COPY server/go.mod server/go.sum ./
RUN go mod download

# Copy server source
COPY server/ ./

# Build binaries
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o bin/server ./cmd/server
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" -o bin/multica ./cmd/multica
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/migrate ./cmd/migrate

# --- Runtime stage ---
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /src/server/bin/server .
COPY --from=builder /src/server/bin/multica .
COPY --from=builder /src/server/bin/migrate .
COPY server/migrations/ ./migrations/
COPY LICENSE NOTICE ./
COPY docker/entrypoint.sh .
RUN sed -i 's/\r$//' entrypoint.sh && chmod +x entrypoint.sh

# Run unprivileged. The uploads dir is created here so a fresh named volume
# mounted at /app/data/uploads inherits this ownership.
RUN addgroup -S -g 1001 multica && adduser -S -u 1001 -G multica multica && \
    mkdir -p /app/data/uploads && chown -R multica:multica /app
USER multica

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["./entrypoint.sh"]
