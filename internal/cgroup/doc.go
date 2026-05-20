// Package cgroup enforces per-app memory, pids, and CPU limits via
// Linux cgroup v2.
//
// # Platform behaviour
//
// Linux: Manager.Create(name, Limits) attaches a sub-cgroup under
// the configured parent slice and writes memory.max, pids.max, and
// cpu.max according to the Limits struct.
//
// Non-Linux (macOS / Windows / others): every Manager method
// returns ErrUnsupported. A spawn that requested CgroupLimits will
// fail at the Create call; the supervisor surfaces the error
// instead of silently dropping the limits. Spawns without
// CgroupLimits work unchanged on all platforms.
//
// # Use from supervisor
//
// The supervisor only invokes this package when the per-app
// Config.CgroupLimits is non-nil AND the daemon's CgroupParent
// env knob is set. Both conditions empty disables cgroup enforcement
// path entirely.
package cgroup
