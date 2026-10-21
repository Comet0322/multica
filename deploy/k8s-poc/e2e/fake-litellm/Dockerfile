FROM golang:1.26-alpine AS b
WORKDIR /s
COPY main.go .
RUN go mod init fakelitellm >/dev/null 2>&1 && CGO_ENABLED=0 go build -o /out/fake-litellm .
FROM alpine:3.21
COPY --from=b /out/fake-litellm /fake-litellm
USER 10001
ENTRYPOINT ["/fake-litellm"]
