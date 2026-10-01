//go:build !linux

package supervisor

import (
	"fmt"
	"os/exec"
)

// startProcess starts cmd directly. Off Linux there are no cgroups to
// place the app in (cgroup.Create returns ErrUnsupported) and no
// /proc/self/exe to re-execute, so there is nothing for a gate to do.
func (s *Supervisor) startProcess(app *App, cmd *exec.Cmd) (*exec.Cmd, error) {
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("supervisor: starting %q: %w", app.ID, err)
	}
	return cmd, nil
}
