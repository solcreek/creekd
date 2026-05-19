# Contributing to creekd

Thank you for considering contributing! This document describes how to set up the development environment and the conventions we follow.

## Status

`creekd` is in **Phase 1, pre-1.0** development. API and CLI surfaces are still in flux; expect breaking changes between minor versions until 1.0. See [`docs/ROADMAP.md`](docs/ROADMAP.md) for what's planned.

Issues and pull requests are welcome. The fastest path to a merged PR:

1. Open an issue describing the bug or feature first, especially for non-trivial changes — alignment up front saves rework.
2. Keep PRs small and focused. One logical change per PR.
3. Make CI green (`go test -race ./...`, plus `make test-linux` if you touch cgroup / sandbox / network paths).
4. If behavior changes, update the relevant `docs/` file in the same PR.

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
- Reference the milestone (e.g., "M5.2") in the PR description when relevant

### Issues

- Include `creekd --version` and OS / kernel
- For bugs: minimum reproducer
- For features: motivating use case

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).

We do **not** require a CLA. Contributions remain copyrighted by their authors.
