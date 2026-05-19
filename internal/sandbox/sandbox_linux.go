//go:build linux

package sandbox

import (
	"os/exec"
	"syscall"
)

// platformApply is the Linux backend for Apply. It ORs the namespace
// clone flags into SysProcAttr.Cloneflags so they compose with any
// flags the caller (or other helpers like attachCgroup) set earlier.
func platformApply(cmd *exec.Cmd, spec Spec) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	var cf uintptr
	if spec.PIDNamespace {
		cf |= syscall.CLONE_NEWPID
	}
	if spec.UTSNamespace {
		cf |= syscall.CLONE_NEWUTS
	}
	if spec.IPCNamespace {
		cf |= syscall.CLONE_NEWIPC
	}
	if spec.MountNamespace {
		cf |= syscall.CLONE_NEWNS
	}
	if spec.UserNamespace {
		cf |= syscall.CLONE_NEWUSER
		cmd.SysProcAttr.UidMappings = toSysIDMap(spec.UIDMappings)
		cmd.SysProcAttr.GidMappings = toSysIDMap(spec.GIDMappings)
		cmd.SysProcAttr.GidMappingsEnableSetgroups = spec.AllowSetgroups
	}
	cmd.SysProcAttr.Cloneflags |= cf

	if spec.Chroot != "" {
		cmd.SysProcAttr.Chroot = spec.Chroot
	}
	// NoNewPrivs is intentionally NOT applied here. Go's
	// syscall.SysProcAttr does not expose PR_SET_NO_NEW_PRIVS, so
	// the supervisor wraps the command with `setpriv --no-new-privs`
	// in startLocked instead. See WrapNoNewPrivs.
	return nil
}

// WrapNoNewPrivs prepends `setpriv --no-new-privs --` to cmd's Path
// and Args, returning the rewritten *exec.Cmd. setpriv (from
// util-linux) calls prctl(PR_SET_NO_NEW_PRIVS, 1) before exec'ing
// the inner command — the resulting child cannot acquire new
// privileges via setuid/setgid binaries for the rest of its
// lifetime.
//
// setpriv must be in PATH on the host. Every mainstream Linux
// distribution ships it as part of util-linux; the Dockerfile.test
// pulls it explicitly via the iproute2 / iptables install layer.
func WrapNoNewPrivs(cmd *exec.Cmd) *exec.Cmd {
	origPath := cmd.Path
	origArgs := cmd.Args
	wrapped := exec.Command("setpriv", append([]string{
		"--no-new-privs", "--", origPath,
	}, origArgs[1:]...)...)
	// Carry forward fields that startLocked already set: env, stdio,
	// SysProcAttr, WaitDelay. The kernel sees the same SysProcAttr
	// (cloneflags + namespace mappings + cgroup fd), only the leaf
	// binary is now setpriv.
	wrapped.Env = cmd.Env
	wrapped.Stdout = cmd.Stdout
	wrapped.Stderr = cmd.Stderr
	wrapped.SysProcAttr = cmd.SysProcAttr
	wrapped.WaitDelay = cmd.WaitDelay
	return wrapped
}

// toSysIDMap converts the public IDMap slice into the syscall-level
// representation Go's exec.Cmd expects.
func toSysIDMap(in []IDMap) []syscall.SysProcIDMap {
	if len(in) == 0 {
		return nil
	}
	out := make([]syscall.SysProcIDMap, len(in))
	for i, m := range in {
		out[i] = syscall.SysProcIDMap{
			ContainerID: m.ContainerID,
			HostID:      m.HostID,
			Size:        m.Size,
		}
	}
	return out
}
