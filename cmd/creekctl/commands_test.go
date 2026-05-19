package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/creekd/internal/adminapi"
	"github.com/solcreek/creekd/internal/dispatch"
	"github.com/solcreek/creekd/internal/supervisor"
)

// newTestBackend boots a full creekd admin server backed by a real
// supervisor + dispatch router, exposed through httptest. Tests use
// the returned URL via the --server flag, exercising the same flag
// parsing and HTTP wiring the binary uses in production.
func newTestBackend(t *testing.T) (url string, sup *supervisor.Supervisor) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sup = supervisor.New(logger)
	sup.Stdout = io.Discard
	sup.Stderr = io.Discard
	sup.WaitDelay = 500 * time.Millisecond
	sup.HealthCheckInterval = 0

	srv := adminapi.New(sup, dispatch.NewRouter(), "")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs.URL, sup
}

// freeTCPPort returns an OS-allocated free TCP port.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// runSub executes a subcommand against the given argv, capturing
// stdout into a buffer. Returns (stdout, error).
func runSub(t *testing.T, name string, argv []string) (string, error) {
	t.Helper()
	cmd, ok := subcommands[name]
	if !ok {
		t.Fatalf("unknown subcommand %q", name)
	}
	var buf bytes.Buffer
	err := cmd.Run(context.Background(), &buf, argv)
	return buf.String(), err
}

// --- flag parsing -------------------------------------------------

func TestRequireSplitID(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		wantID  string
		wantErr bool
	}{
		{"id only", []string{"myapp"}, "myapp", false},
		{"id then flags", []string{"myapp", "--port", "9000"}, "myapp", false},
		{"flag first → no id", []string{"--port", "9000"}, "", true},
		{"empty", []string{}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, _, err := requireSplitID(c.argv)
			if c.wantErr {
				if err == nil {
					t.Errorf("want error, got id=%q", id)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if id != c.wantID {
				t.Errorf("id = %q, want %q", id, c.wantID)
			}
		})
	}
}

func TestStringSliceFlagAccumulates(t *testing.T) {
	var s stringSliceFlag
	_ = s.Set("a")
	_ = s.Set("b")
	_ = s.Set("c")
	if got := s.String(); got != "a,b,c" {
		t.Errorf("String() = %q, want a,b,c", got)
	}
	if len(s) != 3 {
		t.Errorf("len = %d, want 3", len(s))
	}
}

// --- subcommand integration --------------------------------------

func TestPSEmpty(t *testing.T) {
	url, _ := newTestBackend(t)
	out, err := runSub(t, "ps", []string{"--server", url})
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	if !strings.Contains(out, "ID") || !strings.Contains(out, "STATUS") {
		t.Errorf("missing header in output:\n%s", out)
	}
	// Only one line: the header.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("got %d lines, want 1 (header only)", len(lines))
	}
}

func TestUpAndPS(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	out, err := runSub(t, "up", []string{
		"smoke", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
	})
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("smoke") })
	if !strings.Contains(out, "smoke") || !strings.Contains(out, "running") {
		t.Errorf("up output missing expected fields:\n%s", out)
	}

	// ps must now show one row.
	out, err = runSub(t, "ps", []string{"--server", url})
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("got %d lines, want 2 (header + row):\n%s", len(lines), out)
	}
}

func TestGetJSONOutputIsValidJSON(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	if _, err := runSub(t, "up", []string{
		"j", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
	}); err != nil {
		t.Fatalf("up: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("j") })

	out, err := runSub(t, "get", []string{"j", "--server", url, "--json"})
	if err != nil {
		t.Fatalf("get --json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output not valid JSON: %v\noutput:\n%s", err, out)
	}
	if got["id"] != "j" {
		t.Errorf("id = %v, want j", got["id"])
	}
}

func TestRMReturnsStoppedMessage(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	if _, err := runSub(t, "up", []string{
		"r", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
	}); err != nil {
		t.Fatalf("up: %v", err)
	}
	out, err := runSub(t, "rm", []string{"r", "--server", url})
	if err != nil {
		t.Fatalf("rm: %v", err)
	}
	if out != "stopped r\n" {
		t.Errorf("output = %q, want %q", out, "stopped r\n")
	}
	if sup.Get("r") != nil {
		t.Errorf("supervisor still has app post-rm")
	}
}

func TestGetUnknownAppReturnsError(t *testing.T) {
	url, _ := newTestBackend(t)
	_, err := runSub(t, "get", []string{"ghost", "--server", url})
	if err == nil {
		t.Fatal("expected error for unknown app")
	}
	// Error should surface the NotFound status meaningfully.
	if !strings.Contains(err.Error(), "not_found") &&
		!strings.Contains(err.Error(), "404") {
		t.Errorf("error message = %q, want NotFound mention", err)
	}
}

func TestMissingIDReturnsClearError(t *testing.T) {
	url, _ := newTestBackend(t)
	_, err := runSub(t, "get", []string{"--server", url})
	if err == nil {
		t.Fatal("expected error when id missing")
	}
	if !strings.Contains(err.Error(), "missing <id>") {
		t.Errorf("error = %q, want 'missing <id>' message", err)
	}
}

func TestPSJSONOutput(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	if _, err := runSub(t, "up", []string{
		"a", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
	}); err != nil {
		t.Fatalf("up: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("a") })

	out, err := runSub(t, "ps", []string{"--server", url, "--json"})
	if err != nil {
		t.Fatalf("ps --json: %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON array: %v\noutput:\n%s", err, out)
	}
	if len(got) != 1 || got[0]["id"] != "a" {
		t.Errorf("decoded = %+v, want one entry with id=a", got)
	}
}

func TestFlagOrderingPositionalAfterFlags(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	// Note: positional ID appears AFTER several flags. With stdlib
	// flag's natural behaviour the ID would be misplaced; our
	// requireSplitID prefix-extracts the positional, so this still
	// fails cleanly with "missing <id>" rather than silently picking
	// "--command" as the ID.
	_, err := runSub(t, "up", []string{
		"--server", url, "--command", "sleep", "--arg", "30",
		"--port", strconv.Itoa(port),
	})
	if err == nil {
		t.Fatalf("expected error when ID is missing entirely")
	}
	if !strings.Contains(err.Error(), "missing <id>") {
		t.Errorf("error = %q, want 'missing <id>'", err)
	}

	// And the cleanup path doesn't leak state.
	if got := sup.List(); len(got) != 0 {
		t.Errorf("supervisor has %d apps after failed up; want 0", len(got))
	}
}
