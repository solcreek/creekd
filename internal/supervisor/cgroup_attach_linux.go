//go:build linux

package supervisor

import (
	"os/exec"
	"syscall"
)

// attachCgroup wires fd into cmd.SysProcAttr so the child is born
// inside the cgroup via clone3 + CLONE_INTO_CGROUP. On Linux only.
//
// The helper mutates SysProcAttr in place — pre-existing fields
// (notably Cloneflags and Chroot set by sandbox.Apply) are preserved
// so cgroup attachment and namespace isolation compose into one
// clone3 syscall.
func attachCgroup(cmd *exec.Cmd, fd int) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = fd
}
