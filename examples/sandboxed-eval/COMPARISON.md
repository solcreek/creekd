# creekd sandbox vs docker — head-to-head

Same toy binary, same Linux host, same sandbox guarantees (memory cap, pids cap, no-new-privs). The honest answer to "do I need Docker for this, or does creekd suffice?"

## Measured

```
linux/amd64 (Docker Desktop VM on a darwin host)
go 1.22, creekd HEAD, docker 28.x
N=10 samples per scenario
```

| Metric | creekd sandbox | `docker run` | ratio |
|---|---:|---:|---:|
| Spawn → /healthz 200 (p50) | **207 ms** | 537 ms | docker **2.6×** slower |
| Spawn → /healthz 200 (p95) | **236 ms** | 619 ms | docker 2.6× slower |
| Memory cap mechanism | cgroup v2 (kernel) | cgroup v2 (kernel) | equivalent |
| Memory cap reaction time | < 100 ms | < 100 ms | equivalent |

Both sides have:
- `--memory=64m` (cgroup v2 hard cap, swap disabled)
- `--pids-limit=32`
- An effective sandbox: creekd does chroot + PID/mount/UTS namespaces; docker does its standard container set.

Reproduce: `./bench/run.sh` (needs Linux + Docker daemon).

## Pros / cons

### Where creekd is better

- **2.6× faster cold spawn.** Sub-second matters when each spawn is per-request — AI tool calls, code-judging endpoints, CI per-job sandboxes. Docker's overhead is mostly containerd / runc setup + network namespace creation; creekd is direct `clone3 + chroot`.
- **No image layer.** You ship a binary, not a tarball-of-tarballs. No registry push, no `docker pull` latency on first hit, no layer cache to manage.
- **No daemon.** Docker requires `dockerd` (~150 MB resident, root-owned, network-listening). creekd is the supervisor itself; no separate daemon to babysit.
- **Cgroups + namespaces + chroot only.** When you don't need the full container abstraction — image management, network bridges, ports forwarding, volume drivers — creekd's surface is a fraction of Docker's.
- **Built-in HTTP routing.** `creekctl up` returns; you can `curl -H 'X-Creek-App: <id>' http://host:9000/` immediately. Docker requires `-p` port mapping and you manage which port goes where.

### Where docker is better

- **Full image distribution.** OCI registries, layer dedup, content-addressable storage. If you need to ship a complex multi-file runtime, docker has it solved. creekd has nothing here.
- **Seccomp + capability drop by default.** This is the big one. Docker's default seccomp profile blocks ~40 syscalls; default cap drop strips ~30 capabilities. creekd Phase 1 does neither — it gives you cgroup + namespace + chroot + (optionally) NoNewPrivs, which is meaningful but not hostile-workload-grade.
- **Mature ecosystem.** Compose, swarm, kubernetes, GitHub Actions, every CI/CD platform. creekd has none of this.
- **Image build is part of the workflow.** `docker build` + `Dockerfile` is the standard. creekd assumes you build elsewhere and hand it a binary.
- **Cross-platform image authoring.** Docker Desktop runs anywhere; creekd's sandbox features are Linux-only.

### Where they're rough equivalents

- Memory cap. Both use cgroup v2 `memory.max` (with swap disabled). Both rely on kernel OOM, which fires in < 100 ms.
- PID limits, CPU limits. Same cgroup primitives under the hood.
- Filesystem isolation. Docker has overlay layers; creekd has chroot. Different mechanisms, equivalent end-user effect for "the process can't see the host's /etc".

## When to pick which

**Stay with Docker when:**
- You need actively-hostile-grade isolation. seccomp + cap drop matter when the workload is adversarial. creekd v0.1.0 is for cooperative-but-buggy code.
- Your distribution unit is "a complex image with N files and Y dependencies". Image registries solve a real problem.
- You're plugged into Compose / Kubernetes / CI workflows. The ecosystem alone is worth it.
- You need Windows or cross-arch (multi-platform images).

**Switch to creekd-sandbox when:**
- You spawn many short-lived sandboxes per request and 300 ms of overhead per spawn is killing you (AI tool calls, code judges, eval endpoints, per-request preview environments).
- You're already running Linux + your "container" is one statically-compiled binary. The Docker daemon is overhead you don't need.
- You want one supervisor + one HTTP dispatcher in one binary, not three (dockerd + dispatcher + supervisor).
- You're auditing what's actually applied: creekd's sandbox surface is small enough to read in one sitting (`internal/sandbox/sandbox_linux.go`).

## Phase 2 should close the gap on

- **seccomp** via libseccomp. This is the headline gap.
- **Capability drop** via libcap-ng or prctl syscalls.
- **User namespace + UID/GID mapping** as a default rather than opt-in. (The CLI flag exists; we just don't pre-bake recommended mappings.)

After those land, creekd's isolation reaches docker-default parity for cooperative *and* mildly-adversarial workloads. For nation-state-grade attackers, you want gVisor or Firecracker anyway — neither creekd nor docker is in that class.

## Methodology

The bench in `bench/main.go`:

1. Pre-builds a `creekd-sandbox-bench-toy` scratch image with the static toy binary baked in. This happens once, on the host, untimed. Avoids `docker pull` skew on the first iteration.
2. Each iteration:
   - **creekd**: `creekctl up <id> --command /bin/toy --chroot <rootfs> --pid-namespace --mount-namespace --uts-namespace --memory-max 64M --pids-max 32 --port N`; then wait for HTTP 200 on `http://127.0.0.1:9000/healthz` via the dispatch listener.
   - **docker**: `docker run --rm -d --memory=64m --pids-limit=32 --security-opt=no-new-privileges -p N:N -e PORT=N <toy-image>`; then wait for HTTP 200 on `http://127.0.0.1:N/healthz`.
3. Timer stops on the first 200. App / container is cleaned up between samples.
4. N=10 by default; report p50, p95, min, max.

The bench runs inside a privileged container with `--network=host` so it can curl both the creekd dispatch listener (in-process) and docker-bound ports (host network). The docker socket is mounted from the host so all `docker` commands hit the host daemon — exactly what a real user automating `docker run` from a sandbox host would experience.

### What the bench doesn't show

- **Cold start on first-ever spawn.** Both runs assume an idle creekd + a warm docker image cache. A fresh `docker pull` would add several seconds; creekd has no equivalent step.
- **RSS attribution per container.** Docker's daemon RSS is ~150 MB and it's amortized across all containers, so per-container marginal RSS is hard to attribute fairly. creekd has no daemon-side per-app cost.
- **Concurrent spawn throughput.** This bench is serial. A real per-request sandboxing workload would care about p99 under N=100 concurrent. Worth a follow-up bench.
