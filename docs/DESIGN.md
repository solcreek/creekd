# Design

This document records the *why* behind creekd's shape. The README explains what it is and how to run it; this is for people who want to extend, fork, or argue with the design.

## Goal

Run many independent application processes on one Linux host, with HTTP traffic routed to the right one, while bounding what each can do to the others. The unit is the *app*: a single program (Bun / Node / Deno / arbitrary binary) listening on a TCP port.

Concretely: one $30 VPS, hundreds of apps, ~3 MB resident overhead per app, sub-second spawn, hard memory caps that actually kill on overrun, log capture, zero-downtime swap on redeploy.

## Non-goals

What creekd deliberately is **not**:

- **Not a container runtime.** No image format, no layered filesystem, no registry. We use the same primitives containers do (cgroup v2, namespaces, chroot) à la carte.
- **Not a scheduler.** One host. No bin-packing across machines, no autoscaling decisions. Multi-host is solved one layer up.
- **Not a build system.** Apps arrive as already-runnable processes; how to compile them is the runtime's problem (Bun, Node, etc.) and the user's problem.
- **Not Kubernetes-shaped.** No CRDs, no declarative reconciliation loop, no controller pattern. The admin API is imperative HTTP/JSON (spawn this, stop that).
- **Not a service mesh.** Per-app netns is for isolation, not for sidecars or mTLS-everywhere.

These are choices, not omissions. If you need any of the above, creekd is the wrong layer.

## Process model

Each app is one direct child of creekd. We considered, and rejected, three alternatives:

1. **Embedded runtime (PoC approach).** All apps run inside one Bun process via dynamic `import()`. Crashes are shared, no per-app cgroup, single runtime, no namespace isolation. Fine for a demo; impossible for production.
2. **Container-per-app.** Either rope in containerd / runc or wrap Docker. Solves isolation but drags in image management, layered storage, a separate daemon, and ~20 MB resident per app for the runtime alone.
3. **Fork-then-exec helper.** A small C/Rust shim per app. Pure overhead — Go's `exec.Cmd` already does what we need.

Direct child + cgroup + namespaces gives us the same isolation as a container without the container infrastructure. The trade is: we can't ship pre-built images, so the user (or the Creek runtime above us) supplies the binary.

## Why Go

- Single static binary, cross-compiles to Linux from macOS dev hosts.
- Stdlib has everything we need: `os/exec`, `net/http`, `syscall.SysProcAttr` for cgroup-fd + namespace flags, `io.Copy` for log forwarding.
- Goroutine-per-watch is the natural model for supervising N children — no event-loop gymnastics.
- The few things the stdlib lacks (`PR_SET_NO_NEW_PRIVS`, libseccomp, libcap-ng) are wrapped via `setpriv` etc. rather than CGO. Phase 1 ships with zero CGO.

Rust was the obvious alternative. We chose Go for development velocity; the supervisor is not the hot path of the platform (HTTP requests go straight to app processes, not through Go).

## Control plane / data plane split

Two listeners, two purposes:

- **Admin (control plane):** HTTP/JSON, bearer-auth'd, defaults to loopback. `creekctl` and operator tooling talk here. Operations: spawn, stop, deploy, ps, logs, stats, restart, reset. Idempotent where it can be.
- **Dispatch (data plane):** Public HTTP. Routes incoming requests to the right app by the `X-Creek-App` header (or `?app=<id>` query fallback for tooling that can't set headers). Reverse-proxy in Go's `net/http/httputil` — no extra dependency. Hostname-style routing (`Host: pr-123.example.com` → app `pr-123`) is not built in; put Caddy / nginx / Cloudflare in front and have it copy the relevant `Host` substring into `X-Creek-App` before forwarding.

The split exists because the two have very different threat models. Admin is a high-privilege surface that should never face the public internet; dispatch is a high-volume surface that should never trust its caller. Putting them on one listener with path-based gating ("`/admin/*` is privileged") collapses both into one auth boundary, and it's exactly the kind of thing that ends up CVE'd.

The dispatch listener is independently disable-able (`CREEKD_DISPATCH_ADDR=`) for admin-only deployments where some upstream router (Caddy, Nginx, Cloudflare) owns the public surface.

## State model

State has two halves:

- **Process state** (PIDs, watch channels, cgroup paths, netns names) — in-memory, lost on creekd restart. By design.
- **Declared state** (the set of apps that *should* exist, with their configs) — `state.json`, atomic rename on every mutation. Persisted iff `CREEKD_STATE_DIR` is set.

On creekd startup, the supervisor replays every declared app through `Spawn` before listeners open. Apps come back as fresh processes (new PID, new cgroup, new netns), but their configurations — port, runtime, sandbox spec, cgroup limits — are preserved.

We deliberately did **not** try to re-attach to surviving child processes across a supervisor restart. That would require either:
- pidfd + careful re-adoption (Linux 5.3+, fragile cgroup re-attach), or
- a separate process-1 init shim that holds the children.

Phase 1's stance: creekd restart kills its apps; they come back within seconds. If your app can't tolerate that, you have a redundancy problem one layer up, not a supervisor problem. Phase 2 may revisit ("hard" re-attach for true zero-downtime supervisor upgrades).

## Isolation model

Each spawned process can opt into, independently:

1. **Cgroup v2 limits** — memory.max, memory.swap.max=0, pids.max, cpu.max. Memory cap is hard: kernel OOM kills on overrun. CPU is bandwidth (quota / period), not pinning.
2. **Linux namespaces** — PID, UTS, IPC, mount, user, network. Mix-and-match. User namespace + mappings if you want unprivileged uid 0 inside.
3. **Chroot** — set on the same SysProcAttr; composes with mount namespace.
4. **NoNewPrivs** — `setpriv --no-new-privs --` wrapper, since Go's stdlib doesn't expose `PR_SET_NO_NEW_PRIVS` directly.

These are orthogonal flags on a `sandbox.Spec`. Zero values mean "share with host". The supervisor does not enforce a minimum sandbox — the deployment policy decides what to require, not the runtime.

**Not in Phase 1:** seccomp (needs libseccomp / CGO), capability drop (needs libcap-ng / CGO), and SELinux / AppArmor profiles. These belong to Phase 2 once the CGO build pipeline is set up.

## Networking

Two modes:

- **Shared host network.** App listens on `127.0.0.1:<port>`; dispatch proxies to it directly. Lowest overhead, no isolation between apps at L4.
- **Per-app network namespace.** App lives in its own netns with a veth pair into the host. Dispatch proxies through the host end. Apps can't see each other's loopback or each other's sockets.

Per-app netns is on for hostile / multi-tenant workloads; off for trusted-cohort hosting where the cost (veth setup, iptables rule, the `ip netns exec` wrapper) isn't worth it.

iptables rules are added at spawn and removed at stop. We do not try to be idempotent across creekd restarts here — `make test-linux` includes the cleanup path.

## Restart and deploy

Restart is supervisor-internal: same config, new PID. Used by health probes (`watch` goroutine sees the child exit, applies the backoff policy, re-spawns) and by `creekctl restart`.

Deploy is the zero-downtime path: spawn a new instance on a different port, health-probe it, atomically swap the dispatch route, then stop the old instance. The state file is updated only after the swap succeeds. Failure modes:

- New instance fails to start → state untouched, old instance still serving.
- Health probe times out → new instance is killed, state untouched.
- Dispatch swap fails → both instances are running, but only the old is reachable; reset returns to known state.

The supervisor does not try to do canary or weighted routing; that's a higher-layer concern.

## Operational surface

The minimum production setup:

- `CREEKD_ADMIN_TOKEN` set, admin bound to a private interface.
- `CREEKD_DISPATCH_ADDR` either on the public interface or behind a reverse proxy.
- `CREEKD_LOG_DIR` and `CREEKD_CGROUP_PARENT` set.
- `CREEKD_STATE_DIR` set, so a restart of the daemon doesn't drop the fleet.
- Run as root (or a systemd unit with `Delegate=yes`) so cgroup writes succeed.
- `CREEKD_DEBUG_PPROF` left **unset**.

This is intentionally small. Phase 1 explicitly does not include: TLS termination (use a reverse proxy or `autocert` outside creekd), metrics export (add later via OpenTelemetry), audit logging (the structured logs are append-only and JSON; pipe them).

## Testing strategy

Two suites:

- **Unit / portable** — `go test ./...` runs everywhere. Tests that need privileged Linux primitives (cgroup writes, netns, chroot) self-skip via a runtime probe.
- **Privileged Linux** — `make test-linux` builds a Dockerfile.test image and runs the suite in `--privileged` with `cgroupns=host`. This is the only way to actually exercise the M5.5 / sandbox / netns paths on a macOS dev host or a stock CI runner.

Benchmarks (`make bench`) are CI smoke-only — runner CPU is too noisy for regression gating. Local benches on a quiet host are the source of truth.

## Known weak points (Phase 1)

- **No seccomp, no capability drop.** Apps still have whatever caps the parent has. Namespace + cgroup + NoNewPrivs is meaningful but not full sandbox.
- **`--no-new-privs` + `--chroot` don't compose** on rootfs without `setpriv`. v0.1.0 implements NoNewPrivs via a `setpriv` wrap (Go stdlib doesn't expose `PR_SET_NO_NEW_PRIVS` on `SysProcAttr`, and there's no child-setup hook to inject one). Phase 2's CGO work for seccomp + cap drop will inline the prctl in the same C function, removing this constraint.
- **No supervisor-survive-restart re-attach.** Documented above. Phase 2.
- **Single binary, single host.** Multi-host = run more creekd hosts and put an LB in front. There is no clustering in creekd itself, and won't be.
- **Log retention is size-only.** No time-based retention, no remote shipping. The structured JSON output makes shipping easy from any sidecar, but creekd doesn't do it itself.
