package supervisor

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// SpawnGateArg is the argv[1] that makes a creekd binary act as a spawn
// gate (see startProcess) instead of doing its normal work.
const SpawnGateArg = "__spawn-gate"

// RunSpawnGateIfRequested turns this process into a spawn gate when it
// was started as one, and never returns in that case. Call it first in
// main — and in TestMain of any package whose tests spawn apps, since
// there the gate re-executes the test binary.
func RunSpawnGateIfRequested() {
	if len(os.Args) > 1 && os.Args[1] == SpawnGateArg {
		os.Exit(runSpawnGate(os.Args[2:]))
	}
}

// runSpawnGate is the gate side of startProcess. It blocks on fd 3 until
// the supervisor has placed it — in the app's cgroup, when there is one
// — and sends one byte; EOF instead means the supervisor gave up, and
// the gate exits without running anything. Released, it optionally
// chroots, then execs the spawn chain (setpriv …, the app) in place, so
// the PID the supervisor holds becomes the app.
//
// fd 4 reports the outcome the way os/exec reports a failed exec: it is
// close-on-exec, so a successful exec closes it with nothing written,
// and any failure before that writes the reason to it. The supervisor
// therefore still sees "no such command" as a spawn error.
//
//	__spawn-gate [--chroot=<dir>] -- <command> [args...]
func runSpawnGate(args []string) int {
	syscall.CloseOnExec(gateStatusFD)
	chroot := ""
	for len(args) > 0 && args[0] != "--" {
		dir, ok := strings.CutPrefix(args[0], "--chroot=")
		if !ok {
			return gateFail(2, "unknown option %q", args[0])
		}
		chroot, args = dir, args[1:]
	}
	if len(args) < 2 {
		return gateFail(2, "usage: %s [--chroot=<dir>] -- <command> [args...]", SpawnGateArg)
	}
	argv := args[1:]

	gate := os.NewFile(3, "spawn-gate")
	var b [1]byte
	n, _ := gate.Read(b[:])
	_ = gate.Close() // never leaks into the app
	if n != 1 {
		return 1 // the supervisor aborted the spawn
	}
	if chroot != "" {
		// Here rather than in SysProcAttr: a chroot applied at clone
		// would hide this binary from its own exec.
		if err := syscall.Chroot(chroot); err != nil {
			return gateFail(126, "chroot %s: %v", chroot, err)
		}
		if err := os.Chdir("/"); err != nil {
			return gateFail(126, "chdir / in %s: %v", chroot, err)
		}
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return gateFail(127, "%v", err)
	}
	err = syscall.Exec(path, argv, os.Environ())
	return gateFail(126, "exec %s: %v", path, err)
}

// gateStatusFD is the close-on-exec pipe the gate reports failures on.
const gateStatusFD = 4

func gateFail(code int, format string, a ...any) int {
	msg := fmt.Sprintf(format, a...)
	fmt.Fprintf(os.Stderr, "creekd spawn gate: %s\n", msg)
	_, _ = syscall.Write(gateStatusFD, []byte(msg))
	return code
}
