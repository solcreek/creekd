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

// newTestSupervisor returns a Supervisor wired for tests: quiet logger,
// discarded child stdout/stderr to avoid pipe-leak into the test
// process, and a short WaitDelay so child cleanup is bounded.
func newTestSupervisor() *Supervisor {
	sup := New(quietLogger())
	sup.Stdout = io.Discard
	sup.Stderr = io.Discard
	sup.WaitDelay = 500 * time.Millisecond
	return sup
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
	sup := newTestSupervisor()

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
	sup := newTestSupervisor()

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
	sup := newTestSupervisor()
	_, err := sup.Spawn(Config{Command: "sleep", Args: []string{"1"}, Port: 9004})
	if err == nil {
		t.Fatal("expected error for empty id")
	}
}

func TestStopRemovesFromRegistry(t *testing.T) {
	sup := newTestSupervisor()

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
	sup := newTestSupervisor()
	err := sup.Stop("does-not-exist")
	if err == nil {
		t.Fatal("expected error stopping unknown app")
	}
}

// TestCrashTriggersRestart confirms naive restart works. We spawn a
// short-lived process that exits with non-zero code; the supervisor
// must observe the exit and start a new process.
func TestCrashTriggersRestart(t *testing.T) {
	sup := newTestSupervisor()

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
	sup := newTestSupervisor()

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
	sup := newTestSupervisor()
	_, err := sup.Spawn(Config{ID: "no-cmd", Port: 9100})
	if err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestSpawnInvalidCommandReturnsError(t *testing.T) {
	sup := newTestSupervisor()
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
	sup := newTestSupervisor()
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
	sup := newTestSupervisor()
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
	sup := newTestSupervisor()

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
	sup := newTestSupervisor()
	// Tight backoff + disabled crash-loop so we can observe many restarts.
	sup.InitialBackoff = 10 * time.Millisecond
	sup.MaxBackoff = 20 * time.Millisecond
	sup.CrashLoopThreshold = 1000

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
	sup := newTestSupervisor()

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
	sup := newTestSupervisor()
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
	sup := newTestSupervisor()
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

// --- M5.2: restart policy + crash-loop detection -----------------------

func TestComputeBackoffSequence(t *testing.T) {
	sup := newTestSupervisor()
	sup.InitialBackoff = 1 * time.Second
	sup.MaxBackoff = 30 * time.Second

	// Sequence: 1s, 2s, 4s, 8s, 16s, 30s, 30s, 30s, ...
	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}
	for i, w := range want {
		got := sup.computeBackoff(i)
		if got != w {
			t.Errorf("computeBackoff(%d) = %v, want %v", i, got, w)
		}
	}
}

func TestComputeBackoffNegative(t *testing.T) {
	sup := newTestSupervisor()
	if got := sup.computeBackoff(-1); got != sup.InitialBackoff {
		t.Errorf("computeBackoff(-1) = %v, want %v", got, sup.InitialBackoff)
	}
}

func TestCrashLoopDetectionTripsThreshold(t *testing.T) {
	sup := newTestSupervisor()
	sup.InitialBackoff = 5 * time.Millisecond
	sup.MaxBackoff = 10 * time.Millisecond
	sup.RestartWindow = 2 * time.Second
	sup.CrashLoopThreshold = 3

	app, err := sup.Spawn(Config{
		ID:      "loopy",
		Command: "sh",
		Args:    []string{"-c", "exit 1"},
		Port:    9500,
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("loopy") })

	// App should crash-loop quickly: 3 crashes accepted, 4th triggers
	// suspension. Total time ~< 1s with millisecond backoff.
	if !eventuallyTrue(2*time.Second, func() bool {
		return app.Status() == StatusCrashLooping
	}) {
		t.Errorf("expected StatusCrashLooping, got %s (restarts=%d)",
			app.Status(), app.RestartCount())
	}
}

func TestCrashLoopHealthyCrashDoesNotTrip(t *testing.T) {
	sup := newTestSupervisor()
	sup.InitialBackoff = 5 * time.Millisecond
	sup.MaxBackoff = 10 * time.Millisecond
	sup.RestartWindow = 60 * time.Second
	sup.CrashLoopThreshold = 5

	// Spawn an app that crashes ONCE then runs long.
	tmp, err := os.CreateTemp("", "creekd-once-*")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	tmp.Close()
	t.Cleanup(func() { os.Remove(tmp.Name()) })

	// Script: first run exits 1, subsequent runs sleep 30s.
	script := fmt.Sprintf(`if [ ! -f %s ]; then touch %s; exit 1; else sleep 30; fi`,
		tmp.Name()+".marker", tmp.Name()+".marker")
	t.Cleanup(func() { os.Remove(tmp.Name() + ".marker") })

	app, err := sup.Spawn(Config{
		ID:      "one-time-crash",
		Command: "sh",
		Args:    []string{"-c", script},
		Port:    9501,
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("one-time-crash") })

	// After the second spawn, it should sleep and stay running.
	if !eventuallyTrue(2*time.Second, func() bool {
		return app.Status() == StatusRunning && app.RestartCount() == 1
	}) {
		t.Errorf("expected single restart leading to running; status=%s restarts=%d",
			app.Status(), app.RestartCount())
	}

	// Confirm it does NOT enter crash-loop.
	time.Sleep(200 * time.Millisecond)
	if app.Status() == StatusCrashLooping {
		t.Errorf("healthy single crash tripped crash-loop")
	}
}

func TestResetClearsCrashLoop(t *testing.T) {
	sup := newTestSupervisor()
	sup.InitialBackoff = 5 * time.Millisecond
	sup.MaxBackoff = 10 * time.Millisecond
	sup.RestartWindow = 60 * time.Second
	sup.CrashLoopThreshold = 2

	// Use a marker file so the FIRST 3 invocations crash (tripping the
	// crash-loop), and subsequent invocations sleep (so Reset succeeds).
	tmp, err := os.CreateTemp("", "creekd-reset-*")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	tmp.Close()
	t.Cleanup(func() { os.Remove(tmp.Name()); os.Remove(tmp.Name() + ".counter") })

	script := fmt.Sprintf(`
		COUNTER_FILE=%s.counter
		COUNT=$(cat $COUNTER_FILE 2>/dev/null || echo 0)
		COUNT=$((COUNT + 1))
		echo $COUNT > $COUNTER_FILE
		if [ $COUNT -le 3 ]; then exit 1; fi
		sleep 30
	`, tmp.Name())

	app, err := sup.Spawn(Config{
		ID:      "resettable",
		Command: "sh",
		Args:    []string{"-c", script},
		Port:    9502,
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("resettable") })

	// Wait for crash-loop.
	if !eventuallyTrue(2*time.Second, func() bool {
		return app.Status() == StatusCrashLooping
	}) {
		t.Fatalf("expected StatusCrashLooping, got %s", app.Status())
	}

	// Reset → should succeed and the next spawn sleeps successfully.
	if err := sup.Reset("resettable"); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}

	if !eventuallyTrue(2*time.Second, func() bool {
		return app.Status() == StatusRunning
	}) {
		t.Errorf("after Reset expected StatusRunning, got %s", app.Status())
	}

	// Restart count was cleared.
	if rc := app.RestartCount(); rc != 0 {
		t.Errorf("after Reset expected RestartCount=0, got %d", rc)
	}
}

func TestResetUnknownAppReturnsError(t *testing.T) {
	sup := newTestSupervisor()
	err := sup.Reset("does-not-exist")
	if err == nil {
		t.Fatal("expected error resetting unknown app")
	}
}

func TestResetNonCrashLoopingReturnsError(t *testing.T) {
	sup := newTestSupervisor()

	_, err := sup.Spawn(Config{
		ID:      "healthy",
		Command: "sleep",
		Args:    []string{"30"},
		Port:    9503,
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("healthy") })

	err = sup.Reset("healthy")
	if err == nil {
		t.Fatal("expected error resetting a healthy app")
	}
}

func TestRecordRestartTrimsOldEntries(t *testing.T) {
	app := &App{ID: "trim-test"}
	window := 100 * time.Millisecond
	base := time.Now()

	// Record entries: 3 within window, then 1 older.
	app.recordRestart(base, window)
	app.recordRestart(base.Add(10*time.Millisecond), window)
	app.recordRestart(base.Add(20*time.Millisecond), window)

	if got := app.RestartCount(); got != 3 {
		t.Errorf("expected 3 restarts in window, got %d", got)
	}

	// Now add a record far in the future; old ones should fall out.
	app.recordRestart(base.Add(200*time.Millisecond), window)

	if got := app.RestartCount(); got != 1 {
		t.Errorf("expected 1 restart after window slide, got %d", got)
	}
}

func TestStopAllShutdownPath(t *testing.T) {
	sup := newTestSupervisor()

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

