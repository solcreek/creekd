# Architecture principles

This document records the rules creekd uses to evaluate what gets *added*. [`docs/DESIGN.md`](docs/DESIGN.md) covers the shape of the system that already exists — process model, isolation primitives, dispatch wiring. This one is about future change: when contributors (or maintainers) propose new code, new dependencies, or new features, these are the tests they're judged against.

Three principles. The first two have load-bearing real examples in the codebase; the third is the design commitment that protects principles 1-2 from being walked back later under commercial pressure.

---

## 1. Stdlib-first

**Prefer the Go standard library. When that's not enough, prefer one well-maintained, ubiquitous dependency over many small ones. When in doubt, write the small thing yourself.**

### Why

creekd (current line `v0.1.1`, ~570 tests) ships multi-runtime support, cgroup v2, network namespaces, sandboxing, log rotation, blue-green deploy, and a full HTTP reverse proxy on a **tiny direct-dependency surface**: five direct modules in `go.mod` — `BurntSushi/toml` (parsing `creek.toml`), `getkin/kin-openapi` + `oapi-codegen/runtime` (OpenAPI spec is the wire-format source-of-truth), `prometheus/client_golang` (the `/metrics` endpoint), and `golang.org/x/sys` (stdlib-adjacent). Each was added deliberately, with the alternative considered and rejected (e.g. OpenTelemetry SDK was weighed against `prometheus/client_golang` for `/metrics` and rejected on transitive-dep count).

The benefits compound:

- **Supply-chain blast radius stays small.** Every dep is a potential CVE, a maintainer who walks away, a license change, a transitive that pulls in 40 more transitives. Five direct modules is the floor; we add the sixth only when the analysis below points clearly past it.
- **Stripped binary stays small.** As of `v0.1.1` the stripped `creekd` binary is ~13 MB. Most "supervisor + reverse proxy" tools in this class are 50–100 MB.
- **`go mod tidy` is boring.** Months between updates is fine. No constant dep churn, no scrambling to track Dependabot PRs.
- **Reading the source is enough.** You don't need to know what a dozen third-party libraries do to understand a function call.

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

## 3. The hosted product should be survivable to die

**creekd has zero runtime dependency on any service operated by SolCreek (the company). If the hosted product or its vendor account ever vanishes, every existing creekd instance keeps working unmodified.**

### Why

PaaS outages don't distinguish "the vendor's fault" from "your fault" in the customer's experience. Railway's 8-hour [GCP account suspension on 2026-05-19](https://blog.railway.com/p/incident-report-may-19-2026-gcp-account-outage) is the canonical recent example: GCP suspended Railway's corporate account, and even Railway's workloads running on AWS and bare metal went offline because their control plane was single-cloud. The customer didn't care whose fault it was; their app was down for 8 hours.

The defense against this class of failure is not "engineer better redundancy on the vendor side" — that's their job, not ours. The defense is **architectural**: the substrate the user actually runs (creekd) cannot be coupled to anything that can fail.

That coupling is easy to introduce by accident:

- A license check that phones home — fails when home is unreachable.
- Telemetry that requires the cloud-hosted endpoint — degrades silently or fails.
- An auto-update path that pulls config from a SolCreek-operated registry — stops updating.
- An "activation" step that needs a SolCreek API to be reachable on first boot.

None of these exist in creekd today. This principle's job is to keep it that way.

### How to apply

When evaluating a feature proposal, ask:

1. **Does creekd, once installed, ever need to reach a SolCreek-operated service to keep functioning?** If yes, the feature is wrong — even if the SolCreek service is "high-availability" and "five-9s." The answer must be no.
2. **Does the customer's data live in a place SolCreek can withdraw access to?** Code is in their git. Database is in their Postgres / CF D1 / wherever they declared. Storage is in their R2 / S3 / wherever. If we ever propose putting customer data in a SolCreek-owned store, we're back to Railway's exposure.
3. **Is the eject path treated as a real engineering deliverable?** Not a docs page, not a JSON dump that maybe works — a tested, supported way to leave that's part of the release pipeline. Today it's `git clone + creekctl up`; v2's `creek eject` makes it one command. The bar is "tested and supported," not "theoretically possible."

### What this is NOT

- **Not "Creek Cloud is unimportant."** It is the primary product, the revenue layer, the place most users will start. This principle is about what happens when the primary product fails, not about deprioritising it.
- **Not "creekd is a downgrade."** Self-host is a different shape, not a worse shape. It's the same daemon that runs underneath any future Creek Cloud deployment.
- **Not "we're advertising the eject path as a feature."** Marketing the OSS escape hatch only works if the path is actually smooth. As of `v0.1.1`, it works but takes an afternoon (clone, build, configure DNS, set up TLS). `creek eject` (v2 design intent, design doc lives in the private planning repo) is the deliverable that closes that gap. Until that ships, the principle is a design commitment, not a marketing claim.

### Current state vs forward intent

| Aspect | Today (`creekd v0.1.x`) | Forward (v2 `creek eject`) |
|---|---|---|
| Code portability | ✅ Git repo, user owns it | ✅ Unchanged |
| Data residency | ✅ User's own CF / Postgres account | ✅ Unchanged |
| Substrate independence | ✅ creekd is self-contained | ✅ Unchanged |
| Setup smoothness | ⚠️ Manual (clone, build, configure) | ✅ One command (design intent) |
| TLS termination | ❌ Not bundled (use Caddy in front) | ✅ Bundled or scripted |
| Migration tooling | ❌ Manual | ✅ `creek eject` exports docker-compose + db dump + INFRASTRUCTURE.md |

The principle is **load-bearing today** (you can already leave) but **incomplete today** (the path is not smooth). Both things are true. We don't claim the smooth path until it ships.

---

## When these principles collide

They mostly don't. But the one case that's worth thinking about: **a feature that would require a third-party dep AND is policy-shaped**. For example, "first-class integration with billing platform X" — that's both a new dep (their SDK) and policy (their pricing model). Both principles say no. Compound rejections are easy.

The harder case: **a feature that would require a dep but is purely measurement**. The `/metrics` endpoint was this exact case. The substrate test said yes (measurement, not policy). The stdlib test said pause-and-evaluate. We went through the analysis carefully ([commits `97c9662`, `05a6c64`](https://github.com/solcreek/creekd/commits/main)), chose the dep deliberately, and documented why. That's the model: when both principles fire, slow down and write the decision down.

---

## See also

- [`docs/DESIGN.md`](docs/DESIGN.md) — system shape and engineering choices (process model, dispatch, cgroup integration)
- [`docs/CONFIG.md`](docs/CONFIG.md) — operator-facing knobs (the place where defaults are exposed, per principle 2)
- [`docs/ROADMAP.md`](docs/ROADMAP.md) — what's planned, including which features are deliberately *not* on the list
