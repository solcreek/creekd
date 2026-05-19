//go:build !linux

package sandbox

import "os/exec"

// platformApply on non-Linux returns ErrUnsupported. The shim keeps
// the supervisor's call site compiling on macOS dev builds while
// surfacing a clear runtime error if a Spec ever gets applied there.
func platformApply(_ *exec.Cmd, _ Spec) error {
	return ErrUnsupported
}

// WrapNoNewPrivs is a no-op on non-Linux — setpriv is a Linux tool.
// The supervisor only invokes the wrapper when sandbox.Apply has
// already succeeded, so this path is in practice unreachable on
// non-Linux hosts. Kept defined to satisfy the supervisor's
// platform-agnostic call site.
func WrapNoNewPrivs(cmd *exec.Cmd) *exec.Cmd { return cmd }
