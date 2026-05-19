# sandboxed-eval

Run an untrusted-but-cooperative workload — a user-submitted script, an AI tool-call evaluator, a CTF judge — inside a per-instance Linux jail: chroot to a minimal rootfs, fresh PID / mount / UTS namespaces, hard memory + pids caps via cgroup v2.

The point: get most of what `docker run` gives you, in ~200 ms instead of ~540 ms, with no container daemon.

## What it shows

- `creekctl up --chroot ... --pid-namespace --mount-namespace --uts-namespace --memory-max 64M` spawns a child that only sees `/bin/toy` (the rootfs), is its own pid 1 in its own pid table, and dies hard on memory overrun.
- The `/view` endpoint reads `/` and `/etc` from inside the chroot — proof of isolation. `/etc` does not exist; the host's `/etc/passwd` is invisible.
- The `/alloc?mb=128` endpoint allocates 128 MiB. With a 64 MiB cap, the kernel OOM-kills it. The supervisor observes the non-zero exit and re-spawns; `creekctl get eval` shows `restart_count` incremented.

## Run it (Linux)

This example is **Linux-only**: namespaces, chroot, and cgroup v2 are Linux primitives. On a Linux host:

```bash
./up.sh
```

On macOS, run it inside the privileged Linux test container (the header of `up.sh` has the one-liner).

```bash
# baseline — child is pid 1 inside its namespace
curl -H 'X-Creek-App: eval' http://127.0.0.1:9000/
# hello, I am pid=1 hostname=h4 uid=0

# chroot proof — only /bin/toy is visible
curl -H 'X-Creek-App: eval' http://127.0.0.1:9000/view
# === / ===
#   d bin
# === /etc ===
#   (cannot read /etc: open /etc: no such file or directory)

# memory cap — 64 MiB cap, alloc 128 MiB → kernel kills, supervisor restarts
curl -H 'X-Creek-App: eval' 'http://127.0.0.1:9000/alloc?mb=128'
# dispatch: upstream unavailable for app eval     (← process is dead mid-request)

./bin/creekctl get eval
# ... restart_count: 1, new pid
```

Tear down with `./down.sh`.

## How it compares to docker (measured)

| | creekd sandbox | `docker run` |
|---|---:|---:|
| Spawn → /healthz 200 (p50) | **207 ms** | 537 ms |
| Memory cap mechanism | cgroup v2 (kernel OOM, < 100 ms) | cgroup v2 (kernel OOM, < 100 ms) |
| Seccomp default | None (v0.1.0) | ~40 syscalls blocked |
| Capability drop default | None (v0.1.0) | ~30 caps dropped |
| Per-spawn daemon overhead | Same supervisor handles all | Per-call socket round-trip to `dockerd` |

Full methodology + pros/cons: [COMPARISON.md](COMPARISON.md). Reproduce: `./bench/run.sh`.

## Known limitation: `--no-new-privs` + `--chroot`

v0.1.0 implements NoNewPrivs by wrapping the command with `setpriv`. The kernel applies chroot before exec, so it looks for `setpriv` *inside* the chroot — failing if the rootfs doesn't have it. To use both, either:
- Copy `setpriv` (and its shared libraries) into the rootfs, or
- Skip `--no-new-privs` for this app and rely on the other isolation knobs.

A future release will replace the `setpriv` wrap with an inline `prctl(PR_SET_NO_NEW_PRIVS)` call, removing this constraint. See `internal/sandbox/sandbox_linux.go: WrapNoNewPrivs` for the design note.

## What this is not for

**Adversarial workloads.** creekd v0.1.0 has cgroup + namespace + chroot + (optional) NoNewPrivs. It does **not** yet have seccomp or capability drop. For genuinely hostile code — code from people who actively want to escape — pair this with gVisor / Firecracker / Kata, or just use Docker until creekd Phase 2 closes the gap.

For *cooperative-but-buggy* code (your own user input handler, AI tool runs you generated, CTF judging, internal eval endpoints), the current sandbox is enough to make a memory leak / runaway loop / accidental `rm -rf /` into a self-contained crash rather than a host-wide outage.
