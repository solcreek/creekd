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
	return nil
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
