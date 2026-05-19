package adminclient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/creekd/internal/adminapi"
	"github.com/solcreek/creekd/internal/dispatch"
	"github.com/solcreek/creekd/internal/supervisor"
)

// newTestStack wires the same supervisor + adminapi the daemon uses,
// fronted by httptest.NewServer so the client can issue real HTTP
// calls. Returns the client + supervisor (for assertions) + cleanup.
func newTestStack(t *testing.T, token string) (*Client, *supervisor.Supervisor, *httptest.Server) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sup := supervisor.New(logger)
	sup.Stdout = io.Discard
	sup.Stderr = io.Discard
	sup.WaitDelay = 500 * time.Millisecond
	sup.HealthCheckInterval = 0

	server := adminapi.New(sup, dispatch.NewRouter(), token)
	httpSrv := httptest.NewServer(server.Handler())

	client := New(Config{Server: httpSrv.URL, Token: token})
	t.Cleanup(httpSrv.Close)
	return client, sup, httpSrv
}

// freeTCPPort returns an OS-allocated free port.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestNewAppliesDefaults(t *testing.T) {
	c := New(Config{})
	if c.Server() != DefaultServer {
		t.Errorf("Server = %q, want %q", c.Server(), DefaultServer)
	}
}

func TestNewTrimsTrailingSlash(t *testing.T) {
	c := New(Config{Server: "http://localhost:9080//"})
	if c.Server() != "http://localhost:9080" {
		t.Errorf("Server = %q, want trimmed", c.Server())
	}
}

func TestListEmpty(t *testing.T) {
	c, _, _ := newTestStack(t, "")
	apps, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(apps) != 0 {
		t.Errorf("expected empty, got %d apps", len(apps))
	}
}

func TestSpawnGetStop(t *testing.T) {
	c, sup, _ := newTestStack(t, "")
	port := freeTCPPort(t)

	got, err := c.Spawn(context.Background(), adminapi.SpawnRequest{
		ID: "app1", Command: "sleep", Args: []string{"30"}, Port: port,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got.ID != "app1" || got.Port != port {
		t.Errorf("Spawn view = %+v", got)
	}
	t.Cleanup(func() { _ = sup.Stop("app1") })

	fetched, err := c.Get(context.Background(), "app1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fetched.PID == 0 {
		t.Errorf("PID = 0, want > 0")
	}

	if err := c.Stop(context.Background(), "app1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := c.Get(context.Background(), "app1"); !IsNotFound(err) {
		t.Errorf("Get after Stop: err = %v, want 404", err)
	}
}

func TestSpawnDuplicateReturnsAPIError(t *testing.T) {
	c, sup, _ := newTestStack(t, "")
	port := freeTCPPort(t)
	if _, err := c.Spawn(context.Background(), adminapi.SpawnRequest{
		ID: "dup", Command: "sleep", Args: []string{"30"}, Port: port,
	}); err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("dup") })

	_, err := c.Spawn(context.Background(), adminapi.SpawnRequest{
		ID: "dup", Command: "sleep", Args: []string{"30"}, Port: port,
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != http.StatusConflict {
		t.Errorf("Status = %d, want 409", apiErr.Status)
	}
	if apiErr.Code != adminapi.CodeConflict {
		t.Errorf("Code = %q, want %q", apiErr.Code, adminapi.CodeConflict)
	}
}

func TestGetUnknownIsNotFound(t *testing.T) {
	c, _, _ := newTestStack(t, "")
	_, err := c.Get(context.Background(), "ghost")
	if !IsNotFound(err) {
		t.Errorf("err = %v, want IsNotFound", err)
	}
}

func TestAuthFailureSurfaces401(t *testing.T) {
	// Server requires "secret"; client sends nothing.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sup := supervisor.New(logger)
	sup.HealthCheckInterval = 0
	srv := adminapi.New(sup, dispatch.NewRouter(), "secret")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	c := New(Config{Server: httpSrv.URL}) // no token
	_, err := c.List(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Errorf("err = %v, want 401 APIError", err)
	}
}

func TestRestartHappyPath(t *testing.T) {
	c, sup, _ := newTestStack(t, "")
	sup.InitialBackoff = 10 * time.Millisecond
	sup.MaxBackoff = 20 * time.Millisecond

	port := freeTCPPort(t)
	first, _ := c.Spawn(context.Background(), adminapi.SpawnRequest{
		ID: "rs", Command: "sleep", Args: []string{"30"}, Port: port,
	})
	t.Cleanup(func() { _ = sup.Stop("rs") })

	v, err := c.Restart(context.Background(), "rs", adminapi.RestartRequest{TimeoutMS: 3000})
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if v.PID == 0 || v.PID == first.PID {
		t.Errorf("PID = %d, want new non-zero PID (was %d)", v.PID, first.PID)
	}
}

func TestLogsTailReturnsLines(t *testing.T) {
	c, sup, _ := newTestStack(t, "")
	sup.LogDir = t.TempDir()

	port := freeTCPPort(t)
	_, _ = c.Spawn(context.Background(), adminapi.SpawnRequest{
		ID: "logtail", Command: "sh", Args: []string{"-c", "echo hello-tail; sleep 30"},
		Port: port,
	})
	t.Cleanup(func() { _ = sup.Stop("logtail") })

	// Give the child a moment to print.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body, err := c.LogsTail(context.Background(), "logtail", 10)
		if err == nil && strings.Contains(body, "hello-tail") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	body, err := c.LogsTail(context.Background(), "logtail", 10)
	t.Errorf("never saw 'hello-tail' in tail; last err=%v body=%q", err, body)
}

func TestResetOnHealthyReturns409(t *testing.T) {
	c, sup, _ := newTestStack(t, "")
	port := freeTCPPort(t)
	if _, err := c.Spawn(context.Background(), adminapi.SpawnRequest{
		ID: "h", Command: "sleep", Args: []string{"30"}, Port: port,
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("h") })

	_, err := c.Reset(context.Background(), "h")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Errorf("err = %v, want 409 APIError", err)
	}
}

func TestStopUnknownIsNotFound(t *testing.T) {
	c, _, _ := newTestStack(t, "")
	err := c.Stop(context.Background(), "ghost")
	if !IsNotFound(err) {
		t.Errorf("err = %v, want IsNotFound", err)
	}
}
