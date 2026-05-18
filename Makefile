# creekd Makefile.
#
# Most workflows run `go test ./...` directly on whatever host you're
# on. The Linux-only cgroup tests (M5.5) require a privileged Linux
# environment; the targets below provide repeatable ways to run them
# from any host, including macOS.

GO ?= go
DOCKER ?= docker
DOCKER_IMAGE_TAG ?= creekd-test:dev

.PHONY: build
build:
	$(GO) build ./...

.PHONY: test
test:
	$(GO) test -race -count=1 -timeout 120s ./...

.PHONY: test-cover
test-cover:
	$(GO) test -race -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -20

# test-linux builds Dockerfile.test and runs the full suite inside a
# privileged container with cgroupfs mounted rw. This is the only
# practical way to run the M5.5 cgroup tests on a non-Linux host.
#
# --privileged + cgroupns=host gives the test code unrestricted
# /sys/fs/cgroup access; without these, sub-cgroup creation fails
# with EACCES and the cgroup tests skip themselves rather than fail.
.PHONY: test-linux
test-linux:
	$(DOCKER) build -f Dockerfile.test -t $(DOCKER_IMAGE_TAG) .
	$(DOCKER) run --rm \
		--privileged \
		--cgroupns=host \
		-v /sys/fs/cgroup:/sys/fs/cgroup:rw \
		$(DOCKER_IMAGE_TAG)

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
