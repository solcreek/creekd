# Contributing to creekd

Thank you for considering contributing! This document describes how to set up the development environment and the conventions we follow.

## Status

`creekd` is in **Phase 1, pre-1.0** development. API and CLI surfaces are still in flux; expect breaking changes between minor versions until 1.0. See [`docs/ROADMAP.md`](docs/ROADMAP.md) for what's planned.

Issues and pull requests are welcome. The fastest path to a merged PR:

1. Open an issue describing the bug or feature first, especially for non-trivial changes — alignment up front saves rework.
2. Keep PRs small and focused. One logical change per PR.
3. Make CI green (`go test -race ./...`, plus `make test-linux` if you touch cgroup / sandbox / network paths).
4. If behavior changes, update the relevant `docs/` file in the same PR.
5. If your change adds a dependency or a feature that picks behavior on the operator's behalf, read [`ARCHITECTURE.md`](ARCHITECTURE.md) first. Both are gated.

For security issues, see [`SECURITY.md`](SECURITY.md) — please do **not** open public issues for vulnerabilities.

## Development setup

### Requirements

- Go 1.22 or later
- Linux or macOS (Windows via WSL2 should work but is not tested)
- `golangci-lint` (recommended)

### Build

```bash
go build -o bin/creekd ./cmd/creekd
```

### Test

```bash
go test ./...
```

### Lint

```bash
golangci-lint run
```

## Conventions

### Code style

- Standard Go: `gofmt` enforced, `golangci-lint` clean
- Errors wrap with context: `fmt.Errorf("doing X: %w", err)`
- No `panic` in production paths — return errors
- No global mutable state outside of `main`

### Commit messages

Conventional Commits format:

```
feat(supervisor): add exponential backoff to restart policy
fix(cgroup): handle missing /sys/fs/cgroup on macOS dev
docs(roadmap): clarify M5.4 multi-runtime detection rules
```

Types: `feat`, `fix`, `docs`, `test`, `refactor`, `chore`.

### Pull requests

- One logical change per PR
- Include tests where applicable
- Update the relevant `docs/` file if behavior changes
- Add an entry under `## [Unreleased]` in [`CHANGELOG.md`](CHANGELOG.md) for any user-visible change
- Reference the milestone (e.g., "M5.2") in the PR description when relevant

### Issues

- Include `creekd --version` and OS / kernel
- For bugs: minimum reproducer
- For features: motivating use case

## Before opening an issue or PR

- Read [`NON-GOALS.md`](NON-GOALS.md) — confirm your proposal
  isn't documented as outside the project's scope. If it is and
  you believe the reasoning has changed, open an ADR
  (see [`docs/adr/`](docs/adr/)) proposing the revision instead.
- Read [`MAINTAINERS.md`](MAINTAINERS.md) for the decision-making
  process (lazy consensus, 72h silent = approved) and the PR
  review SLA (first triage within 7 days, full review within 14).

## Code of conduct

Participation in this project is governed by the
[Code of Conduct](CODE_OF_CONDUCT.md) (Contributor Covenant 2.1).
By contributing, you agree to abide by its terms.

## DCO sign-off

Every commit must carry a Developer Certificate of Origin
sign-off — the trailing `Signed-off-by: Name <email>` line. This
is the project's lightweight alternative to a CLA: by signing off,
you certify (per [developercertificate.org](https://developercertificate.org))
that you wrote the change OR have the right to submit it under
the project's Apache 2.0 license.

```sh
# Add to each commit automatically:
git commit --signoff -m "..."

# Or configure once per repo:
git config format.signOff true
```

PRs missing sign-off on any commit are blocked at merge time
(checked by the api-diff CI workflow).

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).

We do **not** require a CLA. Contributions remain copyrighted by their authors. The DCO sign-off above is the attestation that you have the right to make this contribution under Apache 2.0.
