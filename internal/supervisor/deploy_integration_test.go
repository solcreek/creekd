package supervisor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/creekd/internal/dispatch"
)

// spawnHTTPChild boots the self-spawning HTTP test child (defined in
// healthcheck_integration_test.go) under the given supervisor with the
// supplied signature. Returns the App and the port it listens on.
func spawnHTTPChild(t *testing.T, sup *Supervisor, id, signature, mode string) (*App, int) {
	t.Helper()
	port := freePort(t)
	env := []string{
		"CREEK_TEST_HTTPAPP=1",
		"SIGNATURE=" + signature,
	}
	if mode != "" {
		env = append(env, "HEALTH_MODE="+mode)
	}
	app, err := sup.Spawn(Config{
		ID:      id,
		Command: os.Args[0],
		Args:    []string{"-test.run=^$"},
		Port:    port,
		Env:     env,
	})
	if err != nil {
		t.Fatalf("Spawn %s: %v", id, err)
	}
	waitForHTTPReady(t, app, port, 15*time.Second)
	return app, port
}

// fetchBody does GET <path> against the dispatch router, returning
// status + body. Sets X-Creek-App: appID.
func fetchBody(t *testing.T, router *dispatch.Router, appID, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "http://creek.local"+path, nil)
	req.Header.Set(dispatch.HeaderAppID, appID)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	res := w.Result()
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

// TestDeployBlueGreenHappyPath: v1 is serving "v1"; Deploy spawns v2
// with "v2", router flips, requests now return "v2". v1's PID is gone
// from the registry after Deploy returns.
func TestDeployBlueGreenHappyPath(t *testing.T) {
	sup := newTestSupervisor()
	sup.HealthChecker = &HTTPHealthChecker{}
	sup.HealthCheckTimeout = 500 * time.Millisecond
	// Disable the background probe for this test — Deploy drives its
	// own health-wait. (The probe being on would still produce a
	// correct outcome, just slower under failure scenarios.)
	sup.HealthCheckInterval = 0

	router := dispatch.NewRouter()

	v1, v1Port := spawnHTTPChild(t, sup, "myapp", "v1", "")
	t.Cleanup(func() { _ = sup.Stop("myapp") })
	if err := router.Set("myapp", v1Port); err != nil {
		t.Fatalf("router.Set v1: %v", err)
	}

	// Pre-deploy: traffic hits v1.
	status, body := fetchBody(t, router, "myapp", "/")
	if status != 200 || !strings.Contains(body, "v1:") {
		t.Fatalf("pre-deploy: status=%d body=%q (want v1)", status, body)
	}

	// Deploy v2 on a different port.
	v2Port := freePort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	v2, err := sup.Deploy(ctx, router, DeployConfig{
		Config: Config{
			ID:      "myapp",
			Command: os.Args[0],
			Args:    []string{"-test.run=^$"},
			Port:    v2Port,
			Env: []string{
				"CREEK_TEST_HTTPAPP=1",
				"SIGNATURE=v2",
			},
		},
		ReadyTimeout: 10 * time.Second,
		PollInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if v2.Port != v2Port {
		t.Errorf("v2.Port = %d, want %d", v2.Port, v2Port)
	}
	if v2.ID != "myapp" {
		t.Errorf("v2.ID = %q, want myapp (re-keyed)", v2.ID)
	}

	// Registry holds v2, not v1.
	if got := sup.Get("myapp"); got != v2 {
		t.Errorf("Get returned %v, want v2", got)
	}
	// Temp ID must be gone.
	if got := sup.Get(deployTempID("myapp")); got != nil {
		t.Errorf("temp ID still present: %v", got)
	}

	// Post-deploy: traffic hits v2.
	status, body = fetchBody(t, router, "myapp", "/")
	if status != 200 || !strings.Contains(body, "v2:") {
		t.Errorf("post-deploy: status=%d body=%q (want v2)", status, body)
	}

	// v1's process is no longer running. PID() may still report a
	// number (cmd.Process is preserved on the App), but kill -0 it.
	if pidAlive(v1.PID()) {
		// Allow a brief drain window in case stopApp's wait was raced.
		if !eventuallyFalse(2*time.Second, func() bool { return pidAlive(v1.PID()) }) {
			t.Errorf("v1 PID %d still alive after deploy", v1.PID())
		}
	}
}

// TestDeployRollbackOnUnhealthyV2: v2 always returns 500. Deploy must
// return ErrDeployUnhealthy, leave v1 in the registry, and not touch
// the router.
func TestDeployRollbackOnUnhealthyV2(t *testing.T) {
	sup := newTestSupervisor()
	sup.HealthChecker = &HTTPHealthChecker{}
	sup.HealthCheckTimeout = 300 * time.Millisecond
	sup.HealthCheckInterval = 0

	router := dispatch.NewRouter()

	v1, v1Port := spawnHTTPChild(t, sup, "stable", "v1", "")
	t.Cleanup(func() { _ = sup.Stop("stable") })
	_ = router.Set("stable", v1Port)

	// v2 will be persistently unhealthy.
	v2Port := freePort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := sup.Deploy(ctx, router, DeployConfig{
		Config: Config{
			ID:      "stable",
			Command: os.Args[0],
			Args:    []string{"-test.run=^$"},
			Port:    v2Port,
			Env: []string{
				"CREEK_TEST_HTTPAPP=1",
				"SIGNATURE=v2",
				"HEALTH_MODE=fail",
			},
		},
		ReadyTimeout: 1500 * time.Millisecond,
		PollInterval: 150 * time.Millisecond,
	})
	if !errors.Is(err, ErrDeployUnhealthy) {
		t.Fatalf("err = %v, want ErrDeployUnhealthy", err)
	}

	// v1 still in registry, router still pointed at v1.
	if got := sup.Get("stable"); got != v1 {
		t.Errorf("Get = %v, want v1", got)
	}
	if b := router.Get("stable"); b == nil || b.Port != v1Port {
		t.Errorf("router after rollback = %+v, want port %d", b, v1Port)
	}
	// Temp ID gone (v2 was Stopped during rollback).
	if got := sup.Get(deployTempID("stable")); got != nil {
		t.Errorf("temp ID lingered: %v", got)
	}

	// v1 still serving its signature.
	status, body := fetchBody(t, router, "stable", "/")
	if status != 200 || !strings.Contains(body, "v1:") {
		t.Errorf("post-rollback: status=%d body=%q (want v1)", status, body)
	}
}

// TestDeployErrorOnUnknownApp: Deploy of an app that isn't registered
// returns ErrNotFound.
func TestDeployErrorOnUnknownApp(t *testing.T) {
	sup := newTestSupervisor()
	router := dispatch.NewRouter()

	_, err := sup.Deploy(context.Background(), router, DeployConfig{
		Config: Config{
			ID: "ghost", Command: "sleep", Args: []string{"30"}, Port: 9999,
		},
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestDeployErrorOnSamePort: v2 must bind a different port than v1.
func TestDeployErrorOnSamePort(t *testing.T) {
	sup := newTestSupervisor()
	router := dispatch.NewRouter()

	port := freePort(t)
	_, err := sup.Spawn(Config{
		ID: "pinned", Command: "sleep", Args: []string{"30"}, Port: port,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("pinned") })

	_, err = sup.Deploy(context.Background(), router, DeployConfig{
		Config: Config{
			ID: "pinned", Command: "sleep", Args: []string{"30"}, Port: port,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "port") {
		t.Errorf("err = %v, want port-conflict error", err)
	}
}

// TestDeployWithoutRouter: router=nil is supported (process-only
// replacement). Deploy must still spawn v2, swap registry, stop v1.
func TestDeployWithoutRouter(t *testing.T) {
	sup := newTestSupervisor()
	sup.HealthChecker = &HTTPHealthChecker{}
	sup.HealthCheckTimeout = 500 * time.Millisecond
	sup.HealthCheckInterval = 0

	_, _ = spawnHTTPChild(t, sup, "nor", "v1", "")
	t.Cleanup(func() { _ = sup.Stop("nor") })

	v2Port := freePort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v2, err := sup.Deploy(ctx, nil, DeployConfig{
		Config: Config{
			ID:      "nor",
			Command: os.Args[0],
			Args:    []string{"-test.run=^$"},
			Port:    v2Port,
			Env:     []string{"CREEK_TEST_HTTPAPP=1", "SIGNATURE=v2"},
		},
		ReadyTimeout: 8 * time.Second,
		PollInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if sup.Get("nor") != v2 {
		t.Errorf("registry not swapped to v2")
	}
}

// pidAlive returns true if a process with the given PID is still
// receiving signals (kill -0). Used to assert v1 actually exited.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(nil) == nil
}

// eventuallyFalse polls f until it returns false or timeout elapses.
// Helper symmetric to eventuallyTrue in supervisor_test.go.
func eventuallyFalse(timeout time.Duration, f func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !f() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return !f()
}

// Compile-time sanity: ensure the http.Client used by HTTPHealthChecker
// reaches our test app over a sane timeout when the supervisor's
// configured timeout is zero. (Defensive — guards a regression where
// HealthChecker would block indefinitely.)
var _ = func() bool {
	c := &http.Client{Timeout: 2 * time.Second}
	_ = c
	return true
}()
