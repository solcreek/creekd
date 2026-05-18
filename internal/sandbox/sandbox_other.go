//go:build !linux

package sandbox

import "os/exec"

// platformApply on non-Linux returns ErrUnsupported. The shim keeps
// the supervisor's call site compiling on macOS dev builds while
// surfacing a clear runtime error if a Spec ever gets applied there.
func platformApply(_ *exec.Cmd, _ Spec) error {
	return ErrUnsupported
}
