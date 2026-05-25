# creekd Makefile.
#
# Most workflows run `go test ./...` directly on whatever host you're
# on. The Linux-only cgroup tests (M5.5) require a privileged Linux
# environment; the targets below provide repeatable ways to run them
# from any host, including macOS.

GO ?= go
DOCKER ?= docker
DOCKER_IMAGE_TAG ?= creekd-test:dev

# Pin to the version the committed generated files were produced with
# (see the header of internal/apitypes/*.gen.go). `go run` resolves the
# module from the cache or downloads it on first invocation — no need
# to `go install` first, and no dependency on GOBIN/GOPATH/PATH.
OAPI_CODEGEN_VERSION ?= v2.7.0
OAPI_CODEGEN ?= go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION)

# generate runs oapi-codegen to produce Go types + server interface
# from the OpenAPI spec. Re-run after editing api/openapi.yaml.
.PHONY: generate
generate:
	cd api && $(OAPI_CODEGEN) --config cfg/types.yaml openapi.yaml
	cd api && $(OAPI_CODEGEN) --config cfg/server.yaml openapi.yaml

# build produces the production binaries at ./creekd + ./creekctl.
# CGO_ENABLED=0 + -trimpath match the goreleaser config so a local
# build has the same shape as a release artifact (CGO-free, no
# host-path leakage). Use build-dev for the sandbox-enabled variant
# used by the cgroup / lima tests.
.PHONY: build
build:
	CGO_ENABLED=0 $(GO) build -trimpath -o creekd ./cmd/creekd
	CGO_ENABLED=0 $(GO) build -trimpath -o creekctl ./cmd/creekctl

.PHONY: build-dev
build-dev:
	$(GO) build -tags creekd_sandbox -o creekd ./cmd/creekd
	$(GO) build -tags creekd_sandbox -o creekctl ./cmd/creekctl

.PHONY: test
test:
	$(GO) test -race -count=1 -timeout 120s ./...

.PHONY: test-dev
test-dev:
	$(GO) test -tags creekd_sandbox -race -count=1 -timeout 120s ./...

.PHONY: test-cover
test-cover:
	$(GO) test -race -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -20

# bench runs every Benchmark* across the module with allocation
# tracking and a 3-second per-bench budget. Race is OFF (race
# detector skews wall clock and is not what bench measures).
# Capture stdout to compare runs:
#   make bench > bench-before.txt
#   ... change something ...
#   make bench > bench-after.txt
#   benchstat bench-before.txt bench-after.txt
.PHONY: bench
bench:
	$(GO) test -bench=. -benchmem -run=^$$ -benchtime=3s -short ./...

# bench-smoke runs every Benchmark* exactly once — proves they
# compile and execute end-to-end but does not produce statistically
# meaningful numbers. Used by CI where the 7 GB ubuntu-latest runner
# OOMs trying to hold 3s worth of spawned child processes across
# every bench. Local devs should use `make bench` instead.
.PHONY: bench-smoke
bench-smoke:
	$(GO) test -bench=. -benchmem -run=^$$ -benchtime=1x -short ./...

# bench-cpu drops a cpu.pprof next to each bench package so you can
# `go tool pprof -top cpu.pprof` to find hot paths.
.PHONY: bench-cpu
bench-cpu:
	$(GO) test -bench=. -benchmem -run=^$$ -benchtime=3s -short \
		-cpuprofile=cpu.pprof -memprofile=mem.pprof \
		./internal/dispatch/ ./internal/supervisor/ ./internal/logs/

# test-linux builds Dockerfile.test and runs the full suite inside a
# privileged container with cgroupfs mounted rw. This is the only
# practical way to run the M5.5 cgroup tests on a non-Linux host.
#
# --privileged + cgroupns=host gives the test code unrestricted
# /sys/fs/cgroup access; without these, sub-cgroup creation fails
# with EACCES and the cgroup tests skip themselves rather than fail.
# DISTRO selects which Dockerfile to use for Linux tests.
# Default: Debian bookworm (Dockerfile.test).
# Override: make test-linux DISTRO=ubuntu
DISTRO ?= debian

.PHONY: test-linux
test-linux:
ifeq ($(DISTRO),ubuntu)
	$(DOCKER) build -f Dockerfile.test.ubuntu -t $(DOCKER_IMAGE_TAG)-ubuntu .
	$(DOCKER) run --rm \
		--privileged \
		--cgroupns=host \
		-v /sys/fs/cgroup:/sys/fs/cgroup:rw \
		$(DOCKER_IMAGE_TAG)-ubuntu
else
	$(DOCKER) build -f Dockerfile.test -t $(DOCKER_IMAGE_TAG) .
	$(DOCKER) run --rm \
		--privileged \
		--cgroupns=host \
		-v /sys/fs/cgroup:/sys/fs/cgroup:rw \
		$(DOCKER_IMAGE_TAG)
endif

# test-linux-matrix runs privileged tests on both Debian and Ubuntu.
.PHONY: test-linux-matrix
test-linux-matrix:
	$(MAKE) test-linux DISTRO=debian
	$(MAKE) test-linux DISTRO=ubuntu

# test-cgroup runs just the M5.5 cgroup-related suites — faster
# feedback than the full test-linux when iterating on cgroup code.
.PHONY: test-cgroup
test-cgroup:
	$(DOCKER) build -f Dockerfile.test -t $(DOCKER_IMAGE_TAG) .
	$(DOCKER) run --rm \
		--privileged \
		--cgroupns=host \
		-v /sys/fs/cgroup:/sys/fs/cgroup:rw \
		$(DOCKER_IMAGE_TAG) \
		go test -race -count=1 -timeout 120s -v \
			./internal/cgroup/... ./internal/supervisor/...
