package supervisor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// This file contains M5.3b health-probe integration tests that exercise
// the real HTTPHealthChecker against a child process that actually
// listens on a TCP port. The child is the test binary itself: when
// CREEK_TEST_HTTPAPP=1 is set, TestMain hands control to runTestApp.

// TestMain decides whether this process is a regular test runner or
// the self-spawned HTTP child. Keeping this in the supervisor package
// means the integration tests can share the package internals.
func TestMain(m *testing.M) {
	if os.Getenv("CREEK_TEST_HTTPAPP") == "1" {
		runHTTPTestApp()
		return
	}
	os.Exit(m.Run())
}

// runHTTPTestApp is the body of the self-spawned child. Behaviour is
// controlled by env vars set by the parent test:
//
//	PORT (required)        port to listen on
//	HEALTH_MODE            "ok" (default), "fail", "flaky:N", "fail-after:N"
//	HEALTH_DELAY_MS        sleep before responding (default 0)
//	SIGNATURE              embedded in response bodies (default "hc")
//
// "flaky:N" returns 500 for the first N requests, then 200.
// "fail-after:N" returns 200 for the first N requests, then 500 forever.
//
// The signature is echoed in both /health and / responses, letting
// blue-green tests tell v1 and v2 apart by body inspection.
func runHTTPTestApp() {
	port := os.Getenv("PORT")
	if port == "" {
		fmt.Fprintln(os.Stderr, "PORT not set")
		os.Exit(2)
	}
	mode := os.Getenv("HEALTH_MODE")
	if mode == "" {
		mode = "ok"
	}
	delayMs, _ := strconv.Atoi(os.Getenv("HEALTH_DELAY_MS"))
	signature := os.Getenv("SIGNATURE")
	if signature == "" {
		signature = "hc"
	}

	var counter int64

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if delayMs > 0 {
			time.Sleep(time.Duration(delayMs) * time.Millisecond)
		}
		n := atomic.AddInt64(&counter, 1)
		switch {
		case mode == "fail":
			w.WriteHeader(500)
		case mode == "ok":
			w.WriteHeader(200)
		case len(mode) > 6 && mode[:6] == "flaky:":
			k, _ := strconv.ParseInt(mode[6:], 10, 64)
			if n <= k {
				w.WriteHeader(500)
			} else {
				w.WriteHeader(200)
			}
		case len(mode) > 11 && mode[:11] == "fail-after:":
			k, _ := strconv.ParseInt(mode[11:], 10, 64)
			if n <= k {
				w.WriteHeader(200)
			} else {
				w.WriteHeader(500)
			}
		default:
			w.WriteHeader(200)
		}
		_, _ = w.Write([]byte(signature + "\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(signature + ":" + r.URL.Path + "\n"))
	})
	srv := &http.Server{Addr: ":" + port, Handler: mux}
	// Block forever — supervisor will terminate us with a signal.
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "test-app:", err)
		os.Exit(1)
	}
}

// freePort returns an OS-allocated free TCP port. We can race with
// another process binding the same port; tests below tolerate startup
// delay via eventuallyTrue.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// waitForHTTPReady polls the given port until /health responds or
// timeout. Used to gate the assertion phase on the child actually
// listening — otherwise the very first probe could fail on connection
// refusal regardless of HEALTH_MODE.
func waitForHTTPReady(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	if !eventuallyTrue(timeout, func() bool {
		req, err := http.NewRequestWithContext(context.Background(), "GET", url, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	}) {
		t.Fatalf("test-app on port %d never came up", port)
	}
}

// TestRealHTTPHealthCheckPasses: a child that always returns 200 should
// not be restarted by the probe.
func TestRealHTTPHealthCheckPasses(t *testing.T) {
	sup := newTestSupervisor()
	sup.HealthChecker = &HTTPHealthChecker{}
	sup.HealthCheckInterval = 80 * time.Millisecond
	sup.HealthCheckTimeout = 500 * time.Millisecond
	sup.HealthCheckFailureThreshold = 3

	port := freePort(t)
	app, err := sup.Spawn(Config{
		ID:      "http-ok",
		Command: os.Args[0],
		Args:    []string{"-test.run=^$"}, // run no tests in the child
		Port:    port,
		Env:     []string{"CREEK_TEST_HTTPAPP=1", "HEALTH_MODE=ok"},
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop(app.ID) })

	waitForHTTPReady(t, port, 8*time.Second)
	originalPID := app.PID()

	// Let ~10 probes happen.
	time.Sleep(1 * time.Second)

	if got := app.PID(); got != originalPID {
		t.Errorf("PID changed (%d → %d); healthy HTTP child should not restart",
			originalPID, got)
	}
	if app.Status() != StatusRunning {
		t.Errorf("expected StatusRunning, got %s", app.Status())
	}
	if app.HealthFailures() != 0 {
		t.Errorf("expected 0 failures, got %d", app.HealthFailures())
	}
}

// TestRealHTTPHealthCheckFailsAndRestarts: a child that always returns
// 500 should be killed and restarted after the failure threshold. New
// PID must be observable.
func TestRealHTTPHealthCheckFailsAndRestarts(t *testing.T) {
	sup := newTestSupervisor()
	sup.HealthChecker = &HTTPHealthChecker{}
	sup.HealthCheckInterval = 80 * time.Millisecond
	sup.HealthCheckTimeout = 500 * time.Millisecond
	sup.HealthCheckFailureThreshold = 3
	// Crash-loop disabled so we cleanly observe a restart.
	sup.InitialBackoff = 30 * time.Millisecond
	sup.MaxBackoff = 50 * time.Millisecond
	sup.CrashLoopThreshold = 100

	port := freePort(t)
	app, err := sup.Spawn(Config{
		ID:      "http-fail",
		Command: os.Args[0],
		Args:    []string{"-test.run=^$"},
		Port:    port,
		Env:     []string{"CREEK_TEST_HTTPAPP=1", "HEALTH_MODE=fail"},
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop(app.ID) })

	waitForHTTPReady(t, port, 8*time.Second)
	originalPID := app.PID()

	// 3 failures × 80ms interval ≈ 240ms + spawn time. Give generous
	// budget for CI.
	if !eventuallyTrue(5*time.Second, func() bool {
		p := app.PID()
		return p != 0 && p != originalPID
	}) {
		t.Fatalf("expected PID change after health threshold; original=%d, current=%d, failures=%d",
			originalPID, app.PID(), app.HealthFailures())
	}

	if app.HealthFailures() < int64(sup.HealthCheckFailureThreshold) {
		t.Errorf("expected ≥%d cumulative failures, got %d",
			sup.HealthCheckFailureThreshold, app.HealthFailures())
	}
}

// TestRealHTTPHealthCheckFlakyRecovers: a child that returns 500 a few
// times (below threshold) and then 200 should NOT be restarted. This
// pins down the "consecutive failures only" semantics in the real path.
func TestRealHTTPHealthCheckFlakyRecovers(t *testing.T) {
	sup := newTestSupervisor()
	sup.HealthChecker = &HTTPHealthChecker{}
	sup.HealthCheckInterval = 80 * time.Millisecond
	sup.HealthCheckTimeout = 500 * time.Millisecond
	sup.HealthCheckFailureThreshold = 4 // child fails 2 then recovers

	port := freePort(t)
	app, err := sup.Spawn(Config{
		ID:      "http-flaky",
		Command: os.Args[0],
		Args:    []string{"-test.run=^$"},
		Port:    port,
		// First 2 health requests return 500, then 200.
		Env: []string{"CREEK_TEST_HTTPAPP=1", "HEALTH_MODE=flaky:2"},
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop(app.ID) })

	waitForHTTPReady(t, port, 8*time.Second)
	originalPID := app.PID()

	// Let probes drive through the flaky phase and into recovery.
	time.Sleep(1500 * time.Millisecond)

	if got := app.PID(); got != originalPID {
		t.Errorf("PID changed (%d → %d); flaky-then-recovered should not restart",
			originalPID, got)
	}
	// Some failures should have been recorded.
	if app.HealthFailures() == 0 {
		t.Errorf("expected non-zero failures during flaky phase, got 0")
	}
	if app.Status() != StatusRunning {
		t.Errorf("expected StatusRunning post-recovery, got %s", app.Status())
	}
}
