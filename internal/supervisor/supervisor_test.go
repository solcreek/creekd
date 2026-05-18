package supervisor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
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

func TestSpawnEmptyCommandRejected(t *testing.T) {
	sup := New(quietLogger())
	_, err := sup.Spawn(Config{ID: "no-cmd", Port: 9100})
	if err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestSpawnInvalidCommandReturnsError(t *testing.T) {
	sup := New(quietLogger())
	_, err := sup.Spawn(Config{
		ID:      "bad-binary",
		Command: "/nonexistent/binary-that-does-not-exist",
		Port:    9101,
	})
	if err == nil {
		t.Fatal("expected error for nonexistent command")
	}
	if got := sup.Get("bad-binary"); got != nil {
		t.Errorf("failed spawn left entry in registry: %v", got)
	}
}

func TestGetReturnsApp(t *testing.T) {
	sup := New(quietLogger())
	app, err := sup.Spawn(Config{ID: "gettable", Command: "sleep", Args: []string{"30"}, Port: 9102})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("gettable") })

	got := sup.Get("gettable")
	if got == nil {
		t.Fatal("Get returned nil for known id")
	}
	if got.ID != app.ID {
		t.Errorf("Get returned wrong app: %v vs %v", got.ID, app.ID)
	}
}

func TestGetUnknownReturnsNil(t *testing.T) {
	sup := New(quietLogger())
	if got := sup.Get("missing"); got != nil {
		t.Errorf("Get on unknown id returned %v, want nil", got)
	}
}

func TestPIDZeroWhenNoProcess(t *testing.T) {
	app := &App{ID: "no-process"}
	if pid := app.PID(); pid != 0 {
		t.Errorf("PID() on uninitialized app = %d, want 0", pid)
	}
}

func TestEnvVariablesPassedToChild(t *testing.T) {
	sup := New(quietLogger())

	tmp, err := os.CreateTemp("", "creekd-env-*")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	t.Cleanup(func() { os.Remove(tmpPath) })

	// Child writes its env to the temp file then exits cleanly.
	// We assert PORT and our custom variable land in the child.
	_, err = sup.Spawn(Config{
		ID:      "env-check",
		Command: "sh",
		Args:    []string{"-c", `printenv > ` + tmpPath + `; sleep 0.5`},
		Port:    9103,
		Env:     []string{"CREEK_TEST_KEY=test-value-xyz"},
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("env-check") })

	// Wait for the child to write the file.
	if !eventuallyTrue(2*time.Second, func() bool {
		data, err := os.ReadFile(tmpPath)
		if err != nil {
			return false
		}
		return strings.Contains(string(data), "PORT=9103") &&
			strings.Contains(string(data), "CREEK_TEST_KEY=test-value-xyz")
	}) {
		data, _ := os.ReadFile(tmpPath)
		t.Errorf("env file missing expected vars; got:\n%s", string(data))
	}
}

func TestStatusStringValues(t *testing.T) {
	cases := []struct {
		s    Status
		want string
	}{
		{StatusUnknown, "unknown"},
		{StatusStarting, "starting"},
		{StatusRunning, "running"},
		{StatusCrashed, "crashed"},
		{StatusStopped, "stopped"},
		{Status(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("Status(%d).String() = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestMultipleRestarts(t *testing.T) {
	sup := New(quietLogger())
	// Speed up the test by shrinking the restart delay.
	sup.restartDelay = 20 * time.Millisecond

	app, err := sup.Spawn(Config{
		ID:      "multi-crash",
		Command: "sh",
		Args:    []string{"-c", "exit 1"},
		Port:    9104,
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("multi-crash") })

	// Collect 4 distinct PIDs over time — proves we restart repeatedly.
	seen := map[int]bool{}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(seen) < 4 {
		if pid := app.PID(); pid != 0 {
			seen[pid] = true
		}
		time.Sleep(15 * time.Millisecond)
	}

	if len(seen) < 4 {
		t.Errorf("expected ≥4 distinct PIDs from multiple restarts, got %d: %v",
			len(seen), seen)
	}
}

func TestConcurrentSpawnsNoRace(t *testing.T) {
	sup := New(quietLogger())

	var wg sync.WaitGroup
	const n = 20
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := sup.Spawn(Config{
				ID:      fmt.Sprintf("concurrent-%d", i),
				Command: "sleep",
				Args:    []string{"30"},
				Port:    9200 + i,
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent spawn err: %v", err)
	}

	if got := len(sup.List()); got != n {
		t.Errorf("expected %d apps after concurrent spawns, got %d", n, got)
	}

	t.Cleanup(func() {
		for i := 0; i < n; i++ {
			_ = sup.Stop(fmt.Sprintf("concurrent-%d", i))
		}
	})
}

func TestListReturnsSnapshot(t *testing.T) {
	sup := New(quietLogger())
	_, err := sup.Spawn(Config{ID: "snap-1", Command: "sleep", Args: []string{"30"}, Port: 9300})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("snap-1") })

	list := sup.List()
	initialLen := len(list)

	_, err = sup.Spawn(Config{ID: "snap-2", Command: "sleep", Args: []string{"30"}, Port: 9301})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("snap-2") })

	if len(list) != initialLen {
		t.Errorf("List() result mutated after later Spawn; snapshot leak (was %d, now %d)",
			initialLen, len(list))
	}

	freshList := sup.List()
	if len(freshList) != initialLen+1 {
		t.Errorf("fresh List() did not see new app: %d vs %d", len(freshList), initialLen+1)
	}
}

func TestUptimeAdvances(t *testing.T) {
	sup := New(quietLogger())
	app, err := sup.Spawn(Config{ID: "uptime", Command: "sleep", Args: []string{"30"}, Port: 9400})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("uptime") })

	u1 := app.Uptime()
	time.Sleep(50 * time.Millisecond)
	u2 := app.Uptime()

	if u1 <= 0 {
		t.Errorf("Uptime immediately after spawn = %v, want > 0", u1)
	}
	if u2 <= u1 {
		t.Errorf("Uptime did not advance: %v then %v", u1, u2)
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

