// Package sandbox confines a supervised child to a subset of the host's
// process / IPC / mount / hostname namespaces, optionally with a chroot.
//
// Phase 1 surface — strict subset of what a full container runtime
// would provide. The intent is to harden the per-app boundary on top
// of cgroup v2 (M5.5), not to replicate runc.
//
//	PID  ns   — child becomes pid 1 of its own process tree
//	UTS  ns   — child's hostname changes don't escape to the host
//	IPC  ns   — separate SysV IPC namespace
//	Mount ns  — separate mount table (required if you want chroot to
//	            survive unmounts inside the child)
//	Chroot    — change root to a caller-supplied directory before exec
//
// Network and user namespaces, seccomp filters, and capabilities
// dropping are intentionally out of scope here — they each warrant
// their own focused implementation.
package sandbox
