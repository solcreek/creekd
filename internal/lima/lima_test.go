package lima

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func testVM(t *testing.T, script string) *VM {
	t.Helper()
	vm := NewVM("test-vm", slog.New(slog.NewTextHandler(os.Stderr, nil)))
	vm.execCommand = func(name string, arg ...string) *exec.Cmd {
		args := append([]string{"-c", script, "--", name}, arg...)
		return exec.Command("bash", args...)
	}
	return vm
}

func TestAvailable(t *testing.T) {
	// Just verify it doesn't panic — result depends on host.
	_ = Available()
}

func TestExistsTrue(t *testing.T) {
	script := `echo '{"name":"test-vm","status":"Running"}'`
	vm := testVM(t, script)
	exists, err := vm.Exists()
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Error("Exists = false, want true")
	}
}

func TestExistsFalse(t *testing.T) {
	script := `echo '{"name":"other-vm","status":"Running"}'`
	vm := testVM(t, script)
	exists, err := vm.Exists()
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Error("Exists = true, want false")
	}
}

func TestRunningTrue(t *testing.T) {
	script := `echo '{"name":"test-vm","status":"Running"}'`
	vm := testVM(t, script)
	running, err := vm.Running()
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if !running {
		t.Error("Running = false, want true")
	}
}

func TestRunningStopped(t *testing.T) {
	script := `echo '{"name":"test-vm","status":"Stopped"}'`
	vm := testVM(t, script)
	running, err := vm.Running()
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if running {
		t.Error("Running = true, want false")
	}
}

func TestShell(t *testing.T) {
	vm := NewVM("test-vm", slog.New(slog.NewTextHandler(os.Stderr, nil)))
	vm.execCommand = func(name string, arg ...string) *exec.Cmd {
		// Simulate: limactl shell test-vm bash -c "<command>"
		// We just echo back the command that was passed
		cmd := arg[len(arg)-1]
		return exec.Command("bash", "-c", fmt.Sprintf("echo %q", cmd))
	}
	out, err := vm.Shell("hello world")
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("Shell output = %q, want to contain %q", out, "hello world")
	}
}

func TestMultipleInstances(t *testing.T) {
	script := `printf '{"name":"vm-a","status":"Running"}\n{"name":"test-vm","status":"Stopped"}\n{"name":"vm-c","status":"Running"}\n'`
	vm := testVM(t, script)

	exists, err := vm.Exists()
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Error("Exists = false, want true")
	}

	running, err := vm.Running()
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if running {
		t.Error("Running = true, want false (status is Stopped)")
	}
}

func TestEmptyList(t *testing.T) {
	script := `echo ""`
	vm := testVM(t, script)
	exists, err := vm.Exists()
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Error("Exists = true for empty list")
	}
}
