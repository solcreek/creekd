# Bench harness image. Same base as Dockerfile.test plus docker-cli
# so the bench can call `docker run` from inside the container,
# back through a mounted /var/run/docker.sock to the host daemon.
FROM golang:1.22-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        coreutils \
        busybox-static \
        iproute2 \
        iptables \
        util-linux \
        docker.io \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /work
ENV GOFLAGS=-buildvcs=false
