package sandbox

import (
	"errors"
	"fmt"
	"os/exec"
)

// Spec describes the namespace + chroot isolation applied to a child
// process. The zero value is "no isolation"; each bool flag opts in
// to one namespace flag at clone time.
//
// Chroot, when non-empty, must reference a directory that already
// contains everything the child needs to exec (binary + libraries +
// /dev nodes the child opens). The kernel performs the chroot AFTER
// any namespace flags take effect, so Chroot combined with
// MountNamespace gives a private filesystem view that cannot leak
// new mounts back to the host.
//
// UserNamespace shifts the child into its own UID/GID namespace.
// The mapping must be supplied via UIDMappings / GIDMappings: each
// entry is a contiguous range from ContainerID through Size,
// mapped onto HostID..HostID+Size-1 on the host. The container's
// 0 (root inside) typically maps to a non-root subuid on the host
// — the standard rootless-container trick. AllowSetgroups gates
// whether the child can call setgroups(2); the safe default
// (false) writes "deny" to /proc/<pid>/setgroups, blocking the
// classic CVE-2014-3158-style escape.
type Spec struct {
	PIDNamespace   bool
	UTSNamespace   bool
	IPCNamespace   bool
	MountNamespace bool
	UserNamespace  bool

	UIDMappings    []IDMap
	GIDMappings    []IDMap
	AllowSetgroups bool

	// NoNewPrivs sets PR_SET_NO_NEW_PRIVS on the child via prctl
	// during clone. Once enabled the process cannot acquire new
	// privileges across exec — setuid / setgid bits are ignored,
	// file capabilities are stripped, and the syscall is sticky
	// (cannot be unset for the lifetime of the process tree).
	// Cheap, high-leverage hardening that pairs well with the user
	// namespace: even if an attacker pivots through a misconfigured
	// suid binary inside the chroot, they cannot gain real-uid
	// privileges outside the sandbox.
	NoNewPrivs bool

	Chroot string
}

// IDMap mirrors syscall.SysProcIDMap so callers don't need to import
// "syscall" just to construct a Spec. ContainerID is the start of
// the range inside the new namespace; HostID is the start outside;
// Size is the range length.
type IDMap struct {
	ContainerID int
	HostID      int
	Size        int
}

// Any reports whether any isolation knob is enabled.
func (s Spec) Any() bool {
	return s.PIDNamespace || s.UTSNamespace || s.IPCNamespace ||
		s.MountNamespace || s.UserNamespace || s.NoNewPrivs ||
		s.Chroot != ""
}

// ErrUnsupported is returned by Apply on non-Linux hosts when a non-empty
// Spec is requested. macOS dev builds compile via the shim in
// sandbox_other.go; runtime use on those hosts surfaces this error
// instead of silently ignoring requested isolation.
var ErrUnsupported = errors.New("sandbox: not supported on this platform")

// Apply mutates cmd's SysProcAttr so that Start spawns the child with
// the namespaces and chroot described by spec. Existing fields on
// SysProcAttr (e.g. UseCgroupFD set by the cgroup attach helper) are
// preserved — Apply only ORs in additional flags. Calling Apply with
// the zero spec is a no-op and never errors.
//
// On non-Linux platforms, calling Apply with any flag set returns
// ErrUnsupported; the cmd is left untouched.
func Apply(cmd *exec.Cmd, spec Spec) error {
	if cmd == nil {
		return errors.New("sandbox: nil cmd")
	}
	if !spec.Any() {
		return nil
	}
	if err := platformApply(cmd, spec); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	return nil
}
