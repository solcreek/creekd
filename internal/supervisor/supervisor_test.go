package supervisor

import (
	"context"
	"io"
	"log/slog"
	"os"
	"syscall"
	"testing"
	"time"
)

// quietLogger returns a slog.Logger that discards all output. Tests
// stay readable when not in -v mode.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// eventuallyTrue polls the predicate until it returns true or timeout
// elapses. Returns true if the condition was met, false on timeout.
func eventuallyTrue(timeout time.Duration, f func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return f()
}

func TestSpawnLongRunning(t *testing.T) {
	sup := New(quietLogger())

	app, err := sup.Spawn(Config{
		ID:      "long-running",
		Command: "sleep",
		Args:    []string{"30"},
		Port:    9001,
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop(app.ID) })

	if app.Status() != StatusRunning {
		t.Fatalf("expected StatusRunning, got %s", app.Status())
	}
	if app.PID() == 0 {
		t.Fatalf("expected non-zero PID after spawn")
	}
	if app.Port != 9001 {
		t.Errorf("expected port 9001, got %d", app.Port)
	}
}

func TestSpawnDuplicateRejected(t *testing.T) {
	sup := New(quietLogger())

	_, err := sup.Spawn(Config{ID: "dup", Command: "sleep", Args: []string{"30"}, Port: 9002})
	if err != nil {
		t.Fatalf("first Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("dup") })

	_, err = sup.Spawn(Config{ID: "dup", Command: "sleep", Args: []string{"30"}, Port: 9003})
	if err == nil {
		t.Fatal("expected error spawning duplicate id, got nil")
	}
}

func TestSpawnEmptyIDRejected(t *testing.T) {
	sup := New(quietLogger())
	_, err := sup.Spawn(Config{Command: "sleep", Args: []string{"1"}, Port: 9004})
	if err == nil {
		t.Fatal("expected error for empty id")
	}
}

func TestStopRemovesFromRegistry(t *testing.T) {
	sup := New(quietLogger())

	app, err := sup.Spawn(Config{ID: "to-stop", Command: "sleep", Args: []string{"30"}, Port: 9005})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}

	if got := sup.Get(app.ID); got == nil {
		t.Fatal("app missing from registry before Stop")
	}

	if err := sup.Stop(app.ID); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if got := sup.Get(app.ID); got != nil {
		t.Errorf("app still in registry after Stop: %v", got)
	}

	// Wait for watch goroutine to observe and stop.
	if !eventuallyTrue(2*time.Second, func() bool {
		return app.Status() == StatusStopped
	}) {
		t.Errorf("app status never reached StatusStopped, got %s", app.Status())
	}
}

func TestStopUnknownIDReturnsError(t *testing.T) {
	sup := New(quietLogger())
	err := sup.Stop("does-not-exist")
	if err == nil {
		t.Fatal("expected error stopping unknown app")
	}
}

// TestCrashTriggersRestart confirms naive restart works. We spawn a
// short-lived process that exits with non-zero code; the supervisor
// must observe the exit and start a new process.
func TestCrashTriggersRestart(t *testing.T) {
	sup := New(quietLogger())

	app, err := sup.Spawn(Config{
		ID:      "crashy",
		Command: "sh",
		Args:    []string{"-c", "exit 1"},
		Port:    9006,
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop(app.ID) })

	firstPID := app.PID()
	if firstPID == 0 {
		t.Fatal("expected non-zero initial PID")
	}

	// Process exits immediately. Supervisor should restart within ~200ms
	// (100ms naive delay + scheduling). Give it 2s to be safe.
	restarted := eventuallyTrue(2*time.Second, func() bool {
		pid := app.PID()
		return pid != 0 && pid != firstPID
	})
	if !restarted {
		t.Fatalf("supervisor did not restart crashy app within timeout (initial pid %d, current %d)",
			firstPID, app.PID())
	}
}

// TestIsolationOneCrashDoesNotAffectOthers spawns multiple apps, kills
// one externally, and verifies the others remain alive.
//
// This is the core acceptance criterion for M5.1: process isolation.
func TestIsolationOneCrashDoesNotAffectOthers(t *testing.T) {
	sup := New(quietLogger())

	apps := make([]*App, 3)
	for i := 0; i < 3; i++ {
		id := []string{"a", "b", "c"}[i]
		app, err := sup.Spawn(Config{
			ID:      id,
			Command: "sleep",
			Args:    []string{"30"},
			Port:    9010 + i,
		})
		if err != nil {
			t.Fatalf("Spawn %q failed: %v", id, err)
		}
		apps[i] = app
		t.Cleanup(func() { _ = sup.Stop(id) })
	}

	// External kill of app "b".
	bPID := apps[1].PID()
	if bPID == 0 {
		t.Fatal("app b has no PID")
	}
	proc, err := os.FindProcess(bPID)
	if err != nil {
		t.Fatalf("could not find PID %d: %v", bPID, err)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill failed: %v", err)
	}

	// "a" and "c" should remain running with their original PIDs unchanged.
	aPID := apps[0].PID()
	cPID := apps[2].PID()

	// Brief settling time so any spurious effects would manifest.
	time.Sleep(200 * time.Millisecond)

	if apps[0].PID() != aPID {
		t.Errorf("app a PID changed (was %d, now %d) — isolation broken",
			aPID, apps[0].PID())
	}
	if apps[2].PID() != cPID {
		t.Errorf("app c PID changed (was %d, now %d) — isolation broken",
			cPID, apps[2].PID())
	}

	// "b" should be restarted with a new PID within timeout.
	restarted := eventuallyTrue(2*time.Second, func() bool {
		pid := apps[1].PID()
		return pid != 0 && pid != bPID
	})
	if !restarted {
		t.Errorf("app b not restarted after kill (was %d, now %d)",
			bPID, apps[1].PID())
	}
}

func TestStopAllShutdownPath(t *testing.T) {
	sup := New(quietLogger())

	for i := 0; i < 3; i++ {
		id := []string{"x", "y", "z"}[i]
		_, err := sup.Spawn(Config{
			ID:      id,
			Command: "sleep",
			Args:    []string{"30"},
			Port:    9020 + i,
		})
		if err != nil {
			t.Fatalf("Spawn %q failed: %v", id, err)
		}
	}

	if got := len(sup.List()); got != 3 {
		t.Fatalf("expected 3 running apps, got %d", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sup.StopAll(ctx)

	if got := len(sup.List()); got != 0 {
		t.Errorf("expected 0 apps after StopAll, got %d", got)
	}
}

