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
	cmd.SysProcAttr.Cloneflags |= cf

	if spec.Chroot != "" {
		cmd.SysProcAttr.Chroot = spec.Chroot
	}
	return nil
}
