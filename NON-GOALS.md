# Non-goals

This document codifies what `creekd` will NOT do. The point is to
save everyone's time: the issue funnel doesn't re-litigate the
same proposals monthly, contributors can scope work without
guessing whether a feature is welcome, and users can pick a
different tool early if their needs fall outside.

A non-goal is not a wishlist excluded by oversight. Each item below
is a deliberate decision with a stated reason. They may be revisited
at major-version boundaries (`1.x` → `2.x`); they will NOT be
revisited issue-by-issue. Reopening a non-goal requires an ADR
(see [`docs/adr/`](docs/adr/)) demonstrating that the reasoning
has materially changed.

If you opened an issue against one of these and got a polite
"closed: see NON-GOALS row N", this is the document the closer was
pointing at.

---

## Phase 2 deferrals (may revisit at `2.0`)

### N1. Multi-host orchestration / clustering / scheduler

`creekd` runs on one host. It owns the local process tree and the
local listening socket; that's the scope. Multi-host is solved one
layer up (a load balancer in front of N independent `creekd`
hosts).

**Why**: every multi-host concern — split-brain, leader election,
shared state consistency, cross-host network — adds a class of
failure modes that would consume the entire engineering budget for
several releases. The point of `creekd` is to be the boring,
predictable single-host primitive that those layers can be built
*on top of*, not the layer that re-invents them.

**Pick instead**: Kubernetes, Nomad, K3s, HashiCorp Consul. They
are designed for this. They make different trade-offs (heavier,
more operational concepts, more dependencies); accept those if you
need clustering.

### N2. Web UI dashboard

`creekd` exposes a CLI (`creekctl`) and a JSON admin API. There is
no built-in web dashboard, and there are no plans to ship one in
Phase 1 or 2.

**Why**: a dashboard is a different product with its own design,
security, accessibility, and ongoing maintenance surface. The
admin API is fully scriptable — anyone who wants a dashboard can
build one against it without affecting `creekd`'s release cadence
or threat model.

**Pick instead**: build your own thin wrapper against the admin
API, or use a generic ops dashboard (Grafana panel against
`/metrics`).

### N3. Container runtime / container images

`creekd` runs processes, not containers. It does NOT pull OCI
images, run `docker`, run a container daemon, manage container
networking overlays, or layer filesystems.

**Why**: process isolation via cgroup v2 + Linux namespaces +
chroot is sufficient for the "many small services on one host"
target. Adding a container runtime would double the binary size,
add a daemon dependency, and pull in registry / image / layer
machinery that's outside the project's scope.

**Pick instead**: Docker, Podman, containerd. They are designed
for OCI workloads. Run them; put their output in front of
`creekd` only if you specifically need both.

### N4. Active-active host pairing / split-brain handling

`creekd`'s restore model is one-host-at-a-time. There is no
support for two `creekd` instances co-owning the same app set
(e.g. "both my-app processes serving traffic from a shared
state").

**Why**: shared state across `creekd` instances would require
distributed coordination (Raft, etcd, etc.) — see N1. The
single-host model is internally consistent and provably correct;
weakening it for an active-active illusion would create silent
data loss surfaces during partitions.

**Pick instead**: run `creekd` on N independent hosts, route via
an upstream load balancer with health checks. If one host dies,
the LB routes around it; failover semantics live in the LB layer,
not in `creekd`.

### N5. Built-in TLS termination in the daemon

`creekd`'s dispatch proxy serves plain HTTP. TLS termination is
explicitly the responsibility of a frontend (Caddy / Cloudflare /
nginx / a load balancer).

**Why**: TLS is a moving target (cert renewal, OCSP, modern
ciphers, HTTP/3) with its own security threat model and operational
discipline. Bundling it would mean `creekd` releases would have to
ship in lockstep with TLS library updates. The Phase 1 boundary
is process supervision + L7 routing; TLS lives one layer up.

**Pick instead**: Caddy in front (auto-ACME, batteries included)
or nginx/HAProxy/Cloudflare for established TLS pipelines.

### N6. Built-in CI / git-push-to-deploy / build pipeline

`creekd` deploys binaries. It does NOT clone git repos, run
builds, fetch dependencies, or compile source.

**Why**: build orchestration is its own discipline with language-
specific tooling (`go build`, `bun install`, `npm ci`, `cargo`,
etc.) and security implications (build-time secrets, supply-chain
verification). Embedding it would force `creekd` to ship opinions
on every language ecosystem.

**Pick instead**: run your build pipeline elsewhere (GitHub
Actions, GitLab CI, Buildkite, Drone) and deploy the resulting
artifact via `creek deploy` or `creekctl deploy`.

### N7. `kubectl`-compatible API surface

The admin API borrows K8s wire-format CONVENTIONS (apiVersion /
kind / metadata / spec / status; conditions; resourceVersion +
If-Match optimistic concurrency). It is NOT a drop-in K8s API
endpoint; `kubectl` will not talk to it, and there are no plans
to make it.

**Why**: the K8s API surface is enormous (CRDs, RBAC, namespaces,
admission webhooks, the entire controller-runtime contract).
Adopting the conventions gives users muscle-memory portability;
adopting the full surface would mean `creekd` becomes K8s, which
defeats the simplification.

**Pick instead**: K8s itself. Use `creekd` when you want K8s-like
semantics without K8s-like operational weight.

---

## Permanent non-goals (not revisited at any version)

### P1. Hostile multi-tenant isolation suitable for arbitrary
customer code without per-tenant review

Phase 1 sandbox (cgroup v2 + Linux namespaces + chroot + NoNewPrivs)
defends against accidental cross-tenant interference and most
opportunistic attacks. It does NOT claim to be sufficient for
running untrusted attacker-controlled code from the public
internet without further review.

**Why**: full hostile-tenant isolation requires seccomp filtering,
capability dropping, gVisor-or-equivalent syscall mediation, and
ongoing kernel-CVE response. These are landing incrementally
(Phase 2 work) but `creekd` will NEVER claim to be sufficient on
its own for that workload — the responsible answer is "use a
sandbox designed for that, like gVisor or Firecracker, and treat
`creekd` as your supervisor for those sandboxed VMs".

**Pick instead**: gVisor, Firecracker, Kata Containers, AWS
Lambda. They are designed for hostile-tenant isolation. Put them
inside `creekd` if you want `creekd` to manage their lifecycle;
do NOT put hostile tenants directly into `creekd` and expect the
Phase 1 sandbox to be sufficient.

### P2. Persistent data layer / database / queue / cache

`creekd` does not bundle, run, or proxy to a database. It does not
provide a queue, cache, or pub/sub. It manages process trees;
state outside the process is the application's problem.

**Why**: stateful systems have radically different operational
profiles (backup, replication, schema migrations, version
upgrades) than stateless ones. Bundling one would expand the
operational surface by an order of magnitude. The volume-mount
primitive (`creekctl up --volume`) supports running stateful apps
ON `creekd`; the daemon itself remains stateless above its own
config + audit log.

**Pick instead**: any database / queue / cache appropriate to your
workload. Run it as a `creekd` app if you want lifecycle
management, or run it externally and connect via the network.

---

## Process

- New non-goals land here via PR with reasoning matching the
  existing rows' shape (one paragraph "why", one paragraph "pick
  instead").
- Issues that propose a non-goal feature are closed with a link
  to the relevant row + the canned reply: *"This is documented as
  a non-goal — see NON-GOALS.md row {N}. If the underlying
  reasoning has changed, please open an ADR proposing the
  revision."*
- The GitHub issue template includes an "I have read NON-GOALS.md"
  checkbox so the funnel surfaces this document before opening.
