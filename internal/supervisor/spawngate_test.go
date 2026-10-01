package supervisor

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gate runs this test binary as a spawn gate (TestMain hands it to
// RunSpawnGateIfRequested) with the release pipe on fd 3 and the status
// pipe on fd 4, and returns the command, the release write end and the
// status read end.
func gate(t *testing.T, args ...string) (*exec.Cmd, *os.File, *strings.Builder) {
	cmd, w, _, out := gateWithStatus(t, args...)
	return cmd, w, out
}

func gateWithStatus(t *testing.T, args ...string) (*exec.Cmd, *os.File, *os.File, *strings.Builder) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	sr, sw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	cmd := exec.Command(os.Args[0], append([]string{SpawnGateArg}, args...)...)
	cmd.ExtraFiles = []*os.File{r, sw}
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_, _ = r.Close(), sw.Close()
	t.Cleanup(func() { _ = sr.Close() })
	return cmd, w, sr, &out
}

// The status pipe carries nothing on a successful exec (it is
// close-on-exec) and the reason on a failed one.
func TestSpawnGateReportsExecOutcome(t *testing.T) {
	cmd, w, status, _ := gateWithStatus(t, "--", "true")
	_, _ = w.Write([]byte{1})
	_ = w.Close()
	if msg, _ := io.ReadAll(status); len(msg) != 0 {
		t.Errorf("successful exec reported %q", msg)
	}
	_ = cmd.Wait()

	cmd, w, status, _ = gateWithStatus(t, "--", "/nonexistent/binary")
	_, _ = w.Write([]byte{1})
	_ = w.Close()
	msg, _ := io.ReadAll(status)
	if !strings.Contains(string(msg), "/nonexistent/binary") {
		t.Errorf("failed exec reported %q, want the missing command named", msg)
	}
	if code := exitCode(cmd.Wait()); code != 127 {
		t.Errorf("exit %d, want 127", code)
	}
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// Released, the gate execs the command — without the gate pipe: fd 3
// must not leak into the app.
func TestSpawnGateExecsWhenReleased(t *testing.T) {
	cmd, w, out := gate(t, "--", "sh", "-c", "echo released; if [ -e /dev/fd/3 ]; then echo fd3-leaked; else echo fd3-closed; fi")
	if _, err := w.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("gate: %v (%s)", err, out)
	}
	if got := strings.Fields(out.String()); len(got) != 2 || got[0] != "released" || got[1] != "fd3-closed" {
		t.Fatalf("output %q, want the command to run with fd 3 closed", out)
	}
}

// EOF instead of the release byte: the supervisor gave up (it could not
// place the process in its cgroup). Nothing may run.
func TestSpawnGateRunsNothingWhenAborted(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	cmd, w, out := gate(t, "--", "touch", marker)
	_ = w.Close()
	if code := exitCode(cmd.Wait()); code != 1 {
		t.Fatalf("exit %d, want 1 (%s)", code, out)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the command ran although the spawn was aborted: %v", err)
	}
}

func TestSpawnGateRejectsBadArgs(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown option": {"--bogus", "--", "true"},
		"no command":     {"--"},
		"no separator":   {"true"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd, w, out := gate(t, args...)
			_, _ = w.Write([]byte{1})
			_ = w.Close()
			if code := exitCode(cmd.Wait()); code != 2 {
				t.Fatalf("exit %d, want 2 (%s)", code, out)
			}
		})
	}
}
