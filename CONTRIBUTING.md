# Contributing to creekd

Thank you for considering contributing! This document describes how to set up the development environment and the conventions we follow.

## Status

`creekd` is in **Phase 1 private development** through approximately Nov 2026. The repo will go public at Phase 1 launch (public beta). Until then, contributions are by invitation only.

If you're interested in contributing post-launch, please:
1. Follow [`solcreek`](https://github.com/solcreek) on GitHub
2. Watch this repo (when it goes public) for "good first issue" labels
3. Join our community when it opens (TBD pre-launch)

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
- Reference the milestone (e.g., "M5.2") in PR description

### Issues

When the repo opens for community issues post-launch:

- Use the bug report or feature request templates
- Include `creekd --version` output
- For bugs: minimum reproducer; for features: motivating use case

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).

We do **not** require a CLA. Contributions remain copyrighted by their authors.
