// Package sandbox confines a supervised child to a subset of the host's
// process / IPC / mount / hostname / user namespaces, with optional
// chroot and no-new-privs hardening.
//
// Surface (Phase 1):
//
//	PID  ns      child becomes pid 1 of its own process tree
//	UTS  ns      hostname changes don't escape to the host
//	IPC  ns      separate SysV IPC namespace
//	Mount ns     separate mount table (pairs with chroot)
//	User ns      UID/GID remap; "root inside" runs as a subuid outside
//	Chroot       caller-supplied rootfs replaces / before exec
//	NoNewPrivs   block setuid/setgid escalation post-exec
//
// Network namespace + veth wiring lives in package network. Seccomp
// BPF filters and full capability-set manipulation are out of scope
// — Go's stdlib doesn't expose seccomp or capset cleanly, and wiring
// them up needs CGO bindings to libseccomp / libcap. Tracked for
// Phase 2 once the build pipeline opens to CGO.
//
// Composition: the clone flags here go on the supervisor's spawn gate
// (internal/supervisor spawngate_linux.go), the process that later
// execs the app, so every namespace is in place before the app's first
// instruction. Chroot is the exception: the gate applies it itself,
// after the supervisor has placed it in its cgroup.
package sandbox
