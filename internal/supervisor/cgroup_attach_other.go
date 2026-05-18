//go:build !linux

package supervisor

import "os/exec"

// attachCgroup is a no-op on non-Linux platforms; the cgroup package
// itself returns ErrUnsupported in those builds so this helper is
// never actually exercised, but keeping it stubbed lets the rest of
// the supervisor compile cleanly on macOS.
func attachCgroup(_ *exec.Cmd, _ int) {}
