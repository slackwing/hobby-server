# Multi-stage build for rv-server.

# Stage 1: Build the Go binaries.
FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git
WORKDIR /app

# Cache go.mod first so dependency download is cached when only source changes.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# Two binaries: the long-running server and the one-shot add-user CLI.
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o rv-server cmd/server/main.go && \
    CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o add-user cmd/add-user/main.go

# Stage 2: Runtime.
FROM alpine:3.19

RUN apk --no-cache add ca-certificates

# Non-root user.
RUN addgroup -g 1000 rv && \
    adduser -D -u 1000 -G rv rv

RUN mkdir -p /config && chown -R rv:rv /config

COPY --from=builder /app/rv-server /usr/local/bin/rv-server
COPY --from=builder /app/add-user /usr/local/bin/add-user

# Reference: liquibase changelog mounted by install.sh for migrations.
COPY --from=builder /app/liquibase /app/liquibase

USER rv
WORKDIR /app
EXPOSE 5002

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:5002/healthz || exit 1

# Default command is the server; override with `add-user` etc. when invoking
# `docker run --rm rv-server add-user <username> <password>`.
CMD ["rv-server"]
