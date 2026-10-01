//go:build linux

package supervisor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/solcreek/creekd/internal/cgroup"
)

// joinCgroup moves pid into cg. A variable so tests can make it fail.
var joinCgroup = func(cg *cgroup.Cgroup, pid int) error { return cg.AddProcess(pid) }

// startProcess starts cmd behind a spawn gate: creekd re-executes itself
// (/proc/self/exe, SpawnGateArg), the supervisor moves that process into
// the app's cgroup, and only then releases it to exec cmd. No app code
// runs outside its cgroup, so its children cannot escape it, and the
// spawn needs no clone3: systemd's RestrictNamespaces turns clone3 into
// ENOSYS, which made CLONE_INTO_CGROUP fail under the shipped unit.
// Every spawn takes this path, with or without a cgroup, so there is
// one way an app starts. Returns the started command, whose PID is the
// app's once the gate has exec'd.
func (s *Supervisor) startProcess(app *App, cmd *exec.Cmd) (*exec.Cmd, error) {
	args := []string{SpawnGateArg}
	attr := cmd.SysProcAttr
	if attr != nil && attr.Chroot != "" {
		// The gate chroots after it is released; at clone time the
		// chroot would hide /proc/self/exe's binary.
		args = append(args, "--chroot="+attr.Chroot)
		c := *attr
		c.Chroot = ""
		attr = &c
	}
	args = append(append(args, "--", cmd.Path), cmd.Args[1:]...)

	r, w, err := os.Pipe() // release: we write, the gate reads (fd 3)
	if err != nil {
		return nil, fmt.Errorf("supervisor: spawn gate pipe: %w", err)
	}
	sr, sw, err := os.Pipe() // status: the gate writes a failure (fd 4)
	if err != nil {
		_, _ = r.Close(), w.Close()
		return nil, fmt.Errorf("supervisor: spawn gate pipe: %w", err)
	}
	defer sr.Close()
	gated := exec.Command("/proc/self/exe", args...)
	gated.Env, gated.Stdout, gated.Stderr = cmd.Env, cmd.Stdout, cmd.Stderr
	gated.SysProcAttr, gated.WaitDelay = attr, cmd.WaitDelay
	gated.ExtraFiles = []*os.File{r, sw}

	err = gated.Start()
	_, _ = r.Close(), sw.Close()
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("supervisor: starting %q: %w", app.ID, err)
	}
	abort := func(cause error) (*exec.Cmd, error) {
		_ = w.Close() // EOF: the gate exits without exec'ing anything
		_ = gated.Process.Kill()
		_ = gated.Wait()
		return nil, cause
	}
	if app.cg != nil {
		if err := joinCgroup(app.cg, gated.Process.Pid); err != nil {
			return abort(fmt.Errorf("supervisor: move %q into its cgroup: %w", app.ID, err))
		}
	}
	if _, err := w.Write([]byte{1}); err != nil {
		return abort(fmt.Errorf("supervisor: release spawn gate for %q: %w", app.ID, err))
	}
	_ = w.Close()

	// EOF with nothing written: the exec succeeded (fd 4 is close-on-exec
	// in the gate). Anything written is why it did not.
	_ = sr.SetReadDeadline(time.Now().Add(gateExecTimeout))
	msg, err := io.ReadAll(sr)
	if err != nil {
		return abort(fmt.Errorf("supervisor: spawn gate for %q did not exec within %v: %w", app.ID, gateExecTimeout, err))
	}
	if len(msg) > 0 {
		_ = gated.Wait() // the gate exits right after reporting
		return nil, fmt.Errorf("supervisor: starting %q: %s", app.ID, msg)
	}
	return gated, nil
}

// gateExecTimeout bounds the wait for a released gate to exec. It only
// trips if something before the exec hangs (a stuck chroot mount).
const gateExecTimeout = 30 * time.Second
