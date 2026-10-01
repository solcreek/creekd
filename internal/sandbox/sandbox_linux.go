//go:build linux

package sandbox

import (
	"os/exec"
	"strconv"
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
// installs it explicitly via the util-linux apt package.
//
// Known incompatibility: this wrap does not compose with a chroot
// on a rootfs that doesn't itself contain setpriv. The spawn gate
// chroots before it execs the chain, so setpriv is looked up inside
// the jail. Callers that combine NoNewPrivs with Chroot must either
// copy setpriv (and its shared libs) into the rootfs, or accept
// that the supervised process won't have NoNewPrivs set.
//
// The fix is to call prctl(PR_SET_NO_NEW_PRIVS) inline in the child
// instead of going through setpriv. Go stdlib doesn't expose a
// child-setup hook for this — the cleanest path is a CGO child
// function, which the Phase 2 seccomp + capability-drop work needs
// anyway. NoNewPrivs lands as one extra line in that same C
// function. See docs/DESIGN.md "Known weak points".
func WrapNoNewPrivs(cmd *exec.Cmd) *exec.Cmd {
	return WrapSetpriv(cmd, SetprivOptions{NoNewPrivs: true})
}

// WrapSetpriv prepends one `setpriv` invocation that applies every
// option in opts before exec'ing cmd: --no-new-privs, and, when UID is
// set, --reuid/--regid/--clear-groups plus an explicit drop of the
// inheritable and ambient capability sets.
//
// The explicit drop matters. Moving from root to a non-zero UID clears
// the capability sets on its own, but moving between two non-zero UIDs
// does not: a non-root creekd that holds CAP_SETUID/CAP_SETGID as
// ambient capabilities (the shipped systemd unit) would pass them to
// the app, which could then switch to any UID — creekd's or another
// app's — and read its /proc/<pid>/environ. With inheritable and
// ambient empty and no file capabilities on the app's binary, the exec
// leaves the app with no capabilities at all. The same chroot caveat
// as WrapNoNewPrivs applies.
func WrapSetpriv(cmd *exec.Cmd, opts SetprivOptions) *exec.Cmd {
	origPath := cmd.Path
	origArgs := cmd.Args
	var flags []string
	if opts.UID > 0 {
		flags = append(flags,
			"--reuid="+strconv.Itoa(opts.UID),
			"--regid="+strconv.Itoa(opts.GID),
			"--clear-groups",
			"--inh-caps=-all",
			"--ambient-caps=-all",
		)
	}
	if opts.NoNewPrivs {
		flags = append(flags, "--no-new-privs")
	}
	wrapped := exec.Command("setpriv", append(append(flags, "--", origPath), origArgs[1:]...)...)
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
