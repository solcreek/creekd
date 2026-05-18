//go:build linux

package supervisor

import (
	"os/exec"
	"syscall"
)

// attachCgroup wires fd into cmd.SysProcAttr so the child is born
// inside the cgroup via clone3 + CLONE_INTO_CGROUP. On Linux only.
func attachCgroup(cmd *exec.Cmd, fd int) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		UseCgroupFD: true,
		CgroupFD:    fd,
	}
}
