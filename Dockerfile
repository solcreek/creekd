# Production Dockerfile for creekd.
#
# Multi-stage build: golang for compilation, debian-slim for the
# runtime. We don't use distroless / scratch because the sandbox
# feature wraps with /usr/bin/setpriv from util-linux, and the
# netns code shells out to /sbin/ip from iproute2. A scratch image
# would compile fine and run un-sandboxed apps; once a user opts
# into --no-new-privs or --net-isolation the binary calls those
# external tools.
#
#   docker build -t creekd:dev .
#   docker run --rm --privileged --cgroupns=host \
#       -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
#       -p 9080:9080 -p 9000:9000 \
#       -e CREEKD_ADMIN_ADDR=0.0.0.0:9080 \
#       -e CREEKD_DISPATCH_ADDR=0.0.0.0:9000 \
#       -e CREEKD_ADMIN_TOKEN="$(openssl rand -hex 32)" \
#       creekd:dev
#
# Privileged + cgroupns=host is required for the cgroup v2 + namespace
# features. In environments where you don't need them (e.g. running
# creekd as a plain process supervisor without per-app caps), drop
# --privileged.

ARG GO_VERSION=1.22

FROM golang:${GO_VERSION}-bookworm AS builder
ARG VERSION=0.0.0-docker
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/creekd ./cmd/creekd && \
    go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/creekctl ./cmd/creekctl

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        iproute2 \
        iptables \
        util-linux \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/creekd   /usr/local/bin/creekd
COPY --from=builder /out/creekctl /usr/local/bin/creekctl

# Loopback by default — operator must override CREEKD_ADMIN_ADDR /
# CREEKD_DISPATCH_ADDR for the daemon to be reachable from outside
# the container. The bind matches `docker run -p` exposed ports.
EXPOSE 9080 9000

ENTRYPOINT ["/usr/local/bin/creekd"]
