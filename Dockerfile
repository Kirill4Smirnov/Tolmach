# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/tolmach-bot ./cmd/bot

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 tolmach \
    && adduser -S -D -H -u 10001 -G tolmach tolmach \
    && install -d -o tolmach -g tolmach -m 0700 /app/data /app/logs

COPY --from=build --chown=tolmach:tolmach /out/tolmach-bot /app/tolmach-bot

USER 10001:10001
WORKDIR /app
VOLUME ["/app/data", "/app/logs"]
ENTRYPOINT ["/app/tolmach-bot"]
CMD ["--env-file", "/dev/null", "--database", "/app/data/tolmach.db"]
