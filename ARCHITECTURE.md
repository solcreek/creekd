# Architecture principles

This document records the rules creekd uses to evaluate what gets *added*. [`docs/DESIGN.md`](docs/DESIGN.md) covers the shape of the system that already exists — process model, isolation primitives, dispatch wiring. This one is about future change: when contributors (or maintainers) propose new code, new dependencies, or new features, these are the tests they're judged against.

Two principles. Both have load-bearing real examples in the codebase.

---

## 1. Stdlib-first

**Prefer the Go standard library. When that's not enough, prefer one well-maintained, ubiquitous dependency over many small ones. When in doubt, write the small thing yourself.**

### Why

creekd shipped its first ten months — through `v0.4.0`, 730+ tests, multi-runtime support, cgroup v2, network namespaces, sandboxing, log rotation, blue-green deploy, full HTTP reverse proxy — with **zero third-party dependencies**. The single direct dependency that exists today (`prometheus/client_golang`) was added deliberately for the `/metrics` endpoint, after we explicitly weighed it against OpenTelemetry SDK and a hand-rolled Prometheus text emitter and documented why ([see commit `05a6c64`](https://github.com/solcreek/creekd/commit/05a6c64) for the reasoning).

The benefits compound:

- **Supply-chain blast radius is minimal.** Every dep is a potential CVE, a maintainer who walks away, a license change, a transitive that pulls in 40 more transitives. Zero deps means zero of those problems.
- **Stripped binary stays small.** As of `v0.4.0` the stripped binary is ~11 MB. Most "supervisor + reverse proxy" tools in this class are 50-100 MB.
- **`go mod tidy` is boring.** Three months between updates is fine. No constant dep churn, no scrambling to track Dependabot PRs.
- **Reading the source is enough.** You don't need to know what 12 third-party libraries do to understand a function call.

### How to apply

When evaluating whether to add a dependency, run through these in order. If the answer is "yes" before the last one, stop.

1. **Is this in the standard library?** `net/http`, `crypto/*`, `os/exec`, `syscall`, `sync/atomic`, `encoding/json` cover an enormous surface area. Look first.
2. **Can I write it in under 200 lines?** A small `parseSize` helper, a custom log rotator, a Prometheus text-format emitter — all viable. Three similar lines beat a premature dependency.
3. **Is there exactly one obvious, ubiquitous choice?** If you have to pick between two roughly-equivalent libraries, neither is dominant enough. Wait until one wins.
4. **Does the candidate ship with significantly fewer transitive dependencies than its alternatives?** Eight transitive deps for `prometheus/client_golang` was acceptable; 30+ for the OTel SDK was not.
5. **Is the API stable across major versions?** A library that broke its API three times in the last two years is a tax forever.

If you can't say yes to **at least #3 + #5**, the dependency is the wrong call.

### What it does NOT mean

- **Not a rule against ever adding dependencies.** It's a rule against adding them carelessly. Real ones (`prometheus/client_golang`) earn their place.
- **Not a rule against using third-party tools at build time.** `goreleaser`, `govulncheck`, `golangci-lint` are fine — they're not in the runtime binary.
- **Not "reinvent everything."** Inventing crypto primitives or HTTP/2 parsing is a different conversation. The rule applies to glue code, observability emitters, CLI flag parsers — the stuff people grab a library for out of habit.

---

## 2. Substrate, not policy

**creekd measures, enforces process boundaries, and exposes data. It does not make business decisions for the operator.**

### Why

A multi-tenant supervisor has many places where you *could* make a policy call: when does a tenant's bandwidth get throttled? At what memory level do we OOM their process? What's the rate limit per second? When do we send a Slack alert?

If creekd answers any of these, it's wrong for half its users. The fleet operator running 50 prosumer apps on a Hetzner box has a totally different policy than the agency running 5 client sites with strict SLAs has a totally different policy than the in-house platform team running 200 services for an internal Kubernetes-allergic org.

The shape that works for all of them: **creekd makes the data and the levers available; operators glue policy on top**.

- **Memory caps:** creekd lets you set `memory.high` and `memory.max` per app, and ships a sane default (`256M` / `1G`). It does *not* implement tier-based defaults ("Free tier gets 128M, Pro gets 512M") — that's a billing-platform concern.
- **Bandwidth:** creekd exposes `creekd_dispatch_bytes_sent_total{app_id="..."}` via `/metrics`. It does *not* throttle apps over a quota or return 429 at N gigabytes. The quota threshold and the response action are operator decisions.
- **Health probes:** creekd ships a default-on liveness check that kills + restarts on consecutive failures. It does *not* page anyone, run readiness gates, or distinguish "this app is unhealthy" from "this app is in maintenance" — those are observability-stack concerns.
- **Alerting:** zero. The `/metrics` endpoint is the contract; what you alert on belongs in your Prometheus / Grafana / Datadog stack.

### How to apply

When evaluating a feature proposal, ask:

1. **Could two reasonable operators disagree on the right behavior?** If yes, the feature should expose a knob, not pick a default behavior. (`memory.high` is a knob with a default. Calling Slack would be a policy.)
2. **Does this feature require knowing about billing tiers, customer contracts, or SLA targets?** If yes, it belongs in the layer above creekd, not in creekd itself.
3. **Is the measurement separable from the action?** If yes, ship the measurement, defer the action. (`bytes_sent_total` was added; "block traffic over quota" was deliberately deferred.)
4. **Does the feature constrain how creekd can be commercialized?** Quota enforcement baked into the OSS daemon would make a hosted-with-overage-billing product harder to build cleanly. Keep policy levers separable from substrate.

### What it does NOT mean

- **Not "creekd has no defaults."** Defaults are essential — they're the policy floor for someone who hasn't thought about it yet. `CREEKD_DEFAULT_MEMORY_HIGH=256M` is a default; making it un-changeable would be policy.
- **Not "creekd never enforces anything."** Process isolation (cgroups, namespaces), health-check kills, crash-loop detection — all enforcement. The line is at "decisions about what a *tenant* is and what they're allowed."
- **Not "creekd has no opinions."** It absolutely does (see [`docs/DESIGN.md`](docs/DESIGN.md) for the design opinions). Opinions on the engineering side; neutrality on the business side.

---

## When these principles collide

They mostly don't. But the one case that's worth thinking about: **a feature that would require a third-party dep AND is policy-shaped**. For example, "first-class integration with billing platform X" — that's both a new dep (their SDK) and policy (their pricing model). Both principles say no. Compound rejections are easy.

The harder case: **a feature that would require a dep but is purely measurement**. The `/metrics` endpoint was this exact case. The substrate test said yes (measurement, not policy). The stdlib test said pause-and-evaluate. We went through the analysis carefully ([commits `97c9662`, `05a6c64`](https://github.com/solcreek/creekd/commits/main)), chose the dep deliberately, and documented why. That's the model: when both principles fire, slow down and write the decision down.

---

## See also

- [`docs/DESIGN.md`](docs/DESIGN.md) — system shape and engineering choices (process model, dispatch, cgroup integration)
- [`docs/CONFIG.md`](docs/CONFIG.md) — operator-facing knobs (the place where defaults are exposed, per principle 2)
- [`docs/ROADMAP.md`](docs/ROADMAP.md) — what's planned, including which features are deliberately *not* on the list
