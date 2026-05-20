// Package cgroup enforces per-tenant memory and CPU limits via Linux
// cgroup v2.
//
// # M5.5 — per-tenant cgroup v2 enforcement
//
// Linux only. On macOS (dev environment) this package logs a warning
// and degrades gracefully — limits are not enforced but supervision
// still works.
//
// Memory limits by tier:
//   - Free:      128 MB
//   - Hobby:     256 MB
//   - Team:      512 MB
//   - Dedicated: unlimited (host-level only)
package cgroup
