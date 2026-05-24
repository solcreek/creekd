package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/solcreek/creekd/internal/adminapi"
	"github.com/solcreek/creekd/internal/apitypes"
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
	// Production default is 30s. Tests can't afford to wait that
	// long for SIGTERM→SIGKILL when a spawned process ends up as
	// PID 1 in its own PID namespace and silently drops SIGTERM —
	// that happens whenever fcb5def's auto-default kicks in
	// (Linux + root, e.g. the privileged Docker matrix in CI).
	sup.GracefulShutdownTimeout = 500 * time.Millisecond
	sup.DisableDefaultSandbox = true

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

func TestParseSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"1024", 1024, false},
		{"256M", 256 * 1024 * 1024, false},
		{"256m", 256 * 1024 * 1024, false},   // lowercase
		{"256Mi", 256 * 1024 * 1024, false},  // k8s-ish suffix
		{"256MiB", 256 * 1024 * 1024, false}, // docker-ish suffix
		{"256MB", 256 * 1024 * 1024, false},  // we choose binary for "MB" too
		{"1G", 1024 * 1024 * 1024, false},
		{"2T", 2 * 1024 * 1024 * 1024 * 1024, false},
		{"  512K  ", 512 * 1024, false}, // trims
		// errors
		{"abc", 0, true},
		{"256X", 0, true},
		{"-1", 0, true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := parseSize(c.in)
			if c.wantErr {
				if err == nil {
					t.Errorf("want error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != c.want {
				t.Errorf("parseSize(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestLimitsFlagsToAPI(t *testing.T) {
	t.Run("all zero returns nil", func(t *testing.T) {
		var lf limitsFlags
		got, err := lf.toAPI()
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})
	t.Run("memory set returns limits", func(t *testing.T) {
		lf := limitsFlags{memoryMax: "256M"}
		got, err := lf.toAPI()
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got == nil || got.MemoryMaxBytes == nil || *got.MemoryMaxBytes != 256*1024*1024 {
			t.Errorf("got %+v, want MemoryMaxBytes=256MiB", got)
		}
	})
	t.Run("invalid size surfaces error", func(t *testing.T) {
		lf := limitsFlags{memoryMax: "256X"}
		if _, err := lf.toAPI(); err == nil {
			t.Error("expected error for invalid size")
		}
	})
	t.Run("memory-high alone returns limits", func(t *testing.T) {
		lf := limitsFlags{memoryHigh: "128M"}
		got, err := lf.toAPI()
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got == nil {
			t.Fatal("expected non-nil limits when --memory-high set")
		}
		if got.MemoryHighBytes == nil || *got.MemoryHighBytes != 128*1024*1024 {
			t.Errorf("MemoryHighBytes = %v, want %d", got.MemoryHighBytes, 128*1024*1024)
		}
		if got.MemoryMaxBytes == nil || *got.MemoryMaxBytes != 0 {
			t.Errorf("MemoryMaxBytes = %v, want 0 (not set)", got.MemoryMaxBytes)
		}
	})
	t.Run("both high and max coexist", func(t *testing.T) {
		lf := limitsFlags{memoryHigh: "256M", memoryMax: "1G"}
		got, err := lf.toAPI()
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got.MemoryHighBytes == nil || *got.MemoryHighBytes != 256*1024*1024 {
			t.Errorf("MemoryHighBytes = %v, want 256MiB", got.MemoryHighBytes)
		}
		if got.MemoryMaxBytes == nil || *got.MemoryMaxBytes != 1024*1024*1024 {
			t.Errorf("MemoryMaxBytes = %v, want 1GiB", got.MemoryMaxBytes)
		}
	})
	t.Run("invalid memory-high surfaces error", func(t *testing.T) {
		lf := limitsFlags{memoryHigh: "bogus"}
		if _, err := lf.toAPI(); err == nil {
			t.Error("expected error for invalid --memory-high size")
		}
	})
}

func TestSandboxFlagsToAPI(t *testing.T) {
	t.Run("zero is nil", func(t *testing.T) {
		var sf sandboxFlags
		if got := sf.toAPI(); got != nil {
			t.Errorf("zero sandboxFlags should be nil, got %+v", got)
		}
	})
	t.Run("pid only", func(t *testing.T) {
		sf := sandboxFlags{pid: true}
		got := sf.toAPI()
		if got == nil || got.PidNamespace == nil || !*got.PidNamespace {
			t.Errorf("pid=true should produce PidNamespace=true, got %+v", got)
		}
	})
	t.Run("chroot only", func(t *testing.T) {
		sf := sandboxFlags{chroot: "/tmp/jail"}
		got := sf.toAPI()
		if got == nil || got.Chroot == nil || *got.Chroot != "/tmp/jail" {
			t.Errorf("chroot should propagate, got %+v", got)
		}
	})
	t.Run("full combo", func(t *testing.T) {
		sf := sandboxFlags{
			pid: true, uts: true, ipc: true, mount: true, user: true,
			noNewPrivs: true, chroot: "/jail",
		}
		got := sf.toAPI()
		if got == nil {
			t.Fatal("got nil")
		}
		if !(derefBool(got.PidNamespace) && derefBool(got.UtsNamespace) && derefBool(got.IpcNamespace) &&
			derefBool(got.MountNamespace) && derefBool(got.UserNamespace) && derefBool(got.NoNewPrivs)) {
			t.Errorf("missing boolean: %+v", got)
		}
		if got.Chroot == nil || *got.Chroot != "/jail" {
			t.Errorf("chroot = %v, want /jail", got.Chroot)
		}
	})
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

// TestUpHealthPathFlag: --health-path flows through the JSON wire
// and lands on App.HealthCheckPath. Probes for that override path
// at runtime are covered in internal/supervisor/healthchecker_test.go;
// this guards the CLI-to-supervisor plumbing.
func TestUpHealthPathFlag(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	if _, err := runSub(t, "up", []string{
		"hp", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
		"--health-path", "/healthz",
	}); err != nil {
		t.Fatalf("up: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("hp") })

	app := sup.Get("hp")
	if app == nil {
		t.Fatal("supervisor: app hp not registered")
	}
	if app.HealthCheckPath != "/healthz" {
		t.Errorf("HealthCheckPath = %q, want /healthz", app.HealthCheckPath)
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
	// GET now returns the k8s-style envelope: name lives under
	// .metadata.name, not as a top-level id field.
	metadata, ok := got["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("envelope missing .metadata object; got %v", got)
	}
	if metadata["name"] != "j" {
		t.Errorf("metadata.name = %v, want j", metadata["name"])
	}
	if got["apiVersion"] != "creek.dev/v1alpha1" {
		t.Errorf("apiVersion = %v, want creek.dev/v1alpha1", got["apiVersion"])
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

// --- Stats subcommand --------------------------------------------

func TestStatsHumanOutputForNonCgroupApp(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	if _, err := runSub(t, "up", []string{
		"s", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
	}); err != nil {
		t.Fatalf("up: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("s") })

	out, err := runSub(t, "stats", []string{"s", "--server", url})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !strings.Contains(out, "cgroup_enabled") || !strings.Contains(out, "false") {
		t.Errorf("output missing cgroup_enabled=false:\n%s", out)
	}
	if !strings.Contains(out, "note") {
		t.Errorf("output missing the no-cgroup hint:\n%s", out)
	}
}

func TestStatsJSONOutput(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	if _, err := runSub(t, "up", []string{
		"j", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
	}); err != nil {
		t.Fatalf("up: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("j") })

	out, err := runSub(t, "stats", []string{"j", "--server", url, "--json"})
	if err != nil {
		t.Fatalf("stats --json: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
	if v["id"] != "j" {
		t.Errorf("id = %v, want j", v["id"])
	}
	if v["cgroup_enabled"] != false {
		t.Errorf("cgroup_enabled = %v, want false", v["cgroup_enabled"])
	}
}

// --- human formatters ---------------------------------------------

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{8 * 1024 * 1024, "8.0 MiB"},
		{16*1024*1024 + 512*1024, "16.5 MiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanMicros(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 µs"},
		{999, "999 µs"},
		{1000, "1.0 ms"},
		{1500, "1.5 ms"},
		{999_999, "1000.0 ms"},
		{1_000_000, "1.00 s"},
		{2_500_000, "2.50 s"},
	}
	for _, c := range cases {
		if got := humanMicros(c.in); got != c.want {
			t.Errorf("humanMicros(%d) = %q, want %q", c.in, got, c.want)
		}
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

// writeBenchManifest builds a minimum-viable .creek-creekd/manifest.json
// inside a fresh tempdir and returns the manifest path. Tests use this
// to exercise the --from wiring end-to-end against a real admin server.
func writeBenchManifest(t *testing.T, runtime, entrypoint string, port int) string {
	t.Helper()
	projectDir := t.TempDir()
	manifestDir := filepath.Join(projectDir, ".creek-creekd")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := fmt.Sprintf(`{
  "version": 1,
  "framework": "nextjs",
  "target": "creekd",
  "buildId": "test-build",
  "nextVersion": "16.2.3",
  "adapter": {"name": "@solcreek/adapter-creekd", "version": "0.1.1"},
  "hasMiddleware": false,
  "hasPrerender": false,
  "runtime": %q,
  "entrypoint": %q,
  "port": %d,
  "serveDirs": [".next/standalone"]
}`, runtime, entrypoint, port)
	manifestPath := filepath.Join(manifestDir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return manifestPath
}

// Integration: `up --from manifest.json` seeds the SpawnRequest from
// the manifest and the resulting app actually lands in the supervisor
// with the manifest's port. CLI passes --command so the spawn doesn't
// depend on Bun/Node being present at test time.
func TestUpFromManifestSeedsPort(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	manifestPath := writeBenchManifest(t, "bun", "server.js", port)

	// --command/--arg are explicit (CLI wins). --port is NOT on the
	// CLI, so the value should come from the manifest.
	out, err := runSub(t, "up", []string{
		"manifested", "--server", url,
		"--command", "sleep", "--arg", "30",
		"--from", manifestPath,
	})
	if err != nil {
		t.Fatalf("up --from: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("manifested") })

	if !strings.Contains(out, "manifested") {
		t.Errorf("up output missing app id:\n%s", out)
	}
	app := sup.Get("manifested")
	if app == nil {
		t.Fatal("app not in supervisor registry")
	}
	if app.Port != port {
		t.Errorf("app.Port = %d, want %d (from manifest)", app.Port, port)
	}
}

// Integration: when the CLI explicitly passes --port, it overrides the
// manifest's port. Same rule for --runtime / --entry (CLI wins over
// manifest), matching the applyManifestTo precedence.
func TestUpFromManifestCLIPortOverrides(t *testing.T) {
	url, sup := newTestBackend(t)
	manifestPort := freeTCPPort(t)
	cliPort := freeTCPPort(t)
	if manifestPort == cliPort {
		// freeTCPPort can collide on a busy host; just pick a different one.
		cliPort = freeTCPPort(t)
	}
	manifestPath := writeBenchManifest(t, "bun", "server.js", manifestPort)

	_, err := runSub(t, "up", []string{
		"override", "--server", url,
		"--command", "sleep", "--arg", "30",
		"--from", manifestPath,
		"--port", strconv.Itoa(cliPort), // CLI must win
	})
	if err != nil {
		t.Fatalf("up --from --port: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("override") })

	app := sup.Get("override")
	if app == nil {
		t.Fatal("app not in supervisor registry")
	}
	if app.Port != cliPort {
		t.Errorf("app.Port = %d, want %d (CLI --port should override manifest)",
			app.Port, cliPort)
	}
}

// `up --from` against a path that doesn't exist surfaces a helpful
// error, doesn't crash, doesn't half-spawn anything.
func TestUpFromManifestMissingFileErrors(t *testing.T) {
	url, sup := newTestBackend(t)
	_, err := runSub(t, "up", []string{
		"nope", "--server", url,
		"--command", "sleep", "--arg", "30",
		"--from", "/tmp/does-not-exist/manifest.json",
	})
	if err == nil {
		t.Fatal("expected error for missing manifest, got nil")
	}
	if !strings.Contains(err.Error(), "--from") {
		t.Errorf("error %q should mention --from", err)
	}
	if got := sup.List(); len(got) != 0 {
		t.Errorf("supervisor has %d apps after failed --from up; want 0", len(got))
	}
}

// deployCapture tracks whether the deploy admin endpoint was hit and,
// if so, what request body the client sent. The Called field is the
// canonical "was the API touched" signal — checking captured.Port
// alone is weak because Port defaults to 0 anyway, so a missing-
// manifest case where the CLI never reached the network would look
// indistinguishable from a request that just happened to set port 0.
type deployCapture struct {
	mu      sync.Mutex
	Called  bool
	Request apitypes.DeployRequest
}

// newDeployCaptureServer returns an httptest server that records
// every POST /v1/apps/<id>/deploy and answers with a synthetic
// running AppView. The harness lets the CLI test run the full
// runDeploy path without needing a real v2 process to become healthy.
func newDeployCaptureServer(t *testing.T) (string, *deployCapture) {
	t.Helper()
	cap := &deployCapture{}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/deploy") {
			cap.mu.Lock()
			defer cap.mu.Unlock()
			cap.Called = true
			if err := json.NewDecoder(r.Body).Decode(&cap.Request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(apitypes.AppView{
				Id:     "deploy-from-test",
				Status: apitypes.Running,
				Port:   cap.Request.Port,
			})
			return
		}
		http.NotFound(w, r)
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL, cap
}

// Integration: `deploy --from` loads the manifest, seeds the
// DeployRequest, and sends the right port to the admin API. Mirrors
// the up --from integration test using a capture-only mock server
// (real supervisor.Deploy needs v2 to become healthy, which a sleep
// command can't do).
func TestDeployFromManifestSeedsPort(t *testing.T) {
	url, cap := newDeployCaptureServer(t)
	manifestPort := freeTCPPort(t)
	manifestPath := writeBenchManifest(t, "bun", "server.js", manifestPort)

	_, err := runSub(t, "deploy", []string{
		"app-id", "--server", url,
		"--command", "sleep", "--arg", "30",
		"--from", manifestPath,
	})
	if err != nil {
		t.Fatalf("deploy --from: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if !cap.Called {
		t.Fatal("admin API was not called")
	}
	if cap.Request.Port != manifestPort {
		t.Errorf("DeployRequest.Port = %d, want %d (from manifest)",
			cap.Request.Port, manifestPort)
	}
	if cap.Request.Runtime == nil || string(*cap.Request.Runtime) != "bun" {
		t.Errorf("DeployRequest.Runtime = %v, want bun (from manifest)", cap.Request.Runtime)
	}
	if cap.Request.Entry == nil || !strings.HasSuffix(*cap.Request.Entry, "server.js") {
		t.Errorf("DeployRequest.Entry = %v, want suffix server.js", cap.Request.Entry)
	}
}

// CLI --port overrides manifest port, same precedence rule as up.
func TestDeployFromManifestCLIPortOverrides(t *testing.T) {
	url, cap := newDeployCaptureServer(t)
	manifestPort := freeTCPPort(t)
	cliPort := freeTCPPort(t)
	if manifestPort == cliPort {
		cliPort = freeTCPPort(t)
	}
	manifestPath := writeBenchManifest(t, "bun", "server.js", manifestPort)

	_, err := runSub(t, "deploy", []string{
		"app-id", "--server", url,
		"--command", "sleep", "--arg", "30",
		"--from", manifestPath,
		"--port", strconv.Itoa(cliPort),
	})
	if err != nil {
		t.Fatalf("deploy --from --port: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if !cap.Called {
		t.Fatal("admin API was not called")
	}
	if cap.Request.Port != cliPort {
		t.Errorf("DeployRequest.Port = %d, want %d (CLI --port should override manifest)",
			cap.Request.Port, cliPort)
	}
}

// `deploy --from` against a path that doesn't exist surfaces a
// helpful error and doesn't trip the admin API.
func TestDeployFromManifestMissingFileErrors(t *testing.T) {
	url, cap := newDeployCaptureServer(t)
	_, err := runSub(t, "deploy", []string{
		"nope", "--server", url,
		"--command", "sleep", "--arg", "30",
		"--from", "/tmp/does-not-exist/manifest.json",
	})
	if err == nil {
		t.Fatal("expected error for missing manifest, got nil")
	}
	if !strings.Contains(err.Error(), "--from") {
		t.Errorf("error %q should mention --from", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.Called {
		t.Error("admin API was called despite missing manifest — runDeploy should error out before client.Deploy")
	}
}

// --- agent-DX feature tests ----------------------------------------

func TestRejectControlChars(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"clean", "hello-world", false},
		{"empty", "", false},
		{"null byte", "hello\x00world", true},
		{"tab", "hello\tworld", true},
		{"newline", "hello\nworld", true},
		{"space is ok", "hello world", false},
		{"unicode ok", "こんにちは", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := rejectControlChars("test", c.input)
			if c.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestFilterFieldsSingleObject(t *testing.T) {
	input := map[string]any{"id": "app1", "status": "running", "port": 3000, "pid": 1234}
	result, err := filterFields(input, "id,status")
	if err != nil {
		t.Fatalf("filterFields: %v", err)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", result)
	}
	if m["id"] != "app1" {
		t.Errorf("id = %v, want app1", m["id"])
	}
	if m["status"] != "running" {
		t.Errorf("status = %v, want running", m["status"])
	}
	if _, exists := m["port"]; exists {
		t.Error("port should be filtered out")
	}
	if _, exists := m["pid"]; exists {
		t.Error("pid should be filtered out")
	}
}

func TestFilterFieldsArray(t *testing.T) {
	input := []map[string]any{
		{"id": "a", "status": "running", "port": 3000},
		{"id": "b", "status": "stopped", "port": 3001},
	}
	result, err := filterFields(input, "id,port")
	if err != nil {
		t.Fatalf("filterFields: %v", err)
	}
	arr, ok := result.([]map[string]any)
	if !ok {
		t.Fatalf("expected []map, got %T", result)
	}
	if len(arr) != 2 {
		t.Fatalf("len = %d, want 2", len(arr))
	}
	if arr[0]["id"] != "a" {
		t.Errorf("[0].id = %v, want a", arr[0]["id"])
	}
	if _, exists := arr[0]["status"]; exists {
		t.Error("[0].status should be filtered out")
	}
}

func TestFilterFieldsEmpty(t *testing.T) {
	input := map[string]any{"id": "x"}
	result, err := filterFields(input, "")
	if err != nil {
		t.Fatalf("filterFields: %v", err)
	}
	m := result.(map[string]any)
	if m["id"] != "x" {
		t.Error("empty fields should return unfiltered")
	}
}

func TestFilterFieldsUnknownField(t *testing.T) {
	input := map[string]any{"id": "x", "status": "running"}
	result, err := filterFields(input, "id,nonexistent")
	if err != nil {
		t.Fatalf("filterFields: %v", err)
	}
	m := result.(map[string]any)
	if m["id"] != "x" {
		t.Errorf("id = %v, want x", m["id"])
	}
	if _, exists := m["nonexistent"]; exists {
		t.Error("nonexistent should not appear")
	}
}

func TestWriteDryRunJSON(t *testing.T) {
	var buf bytes.Buffer
	payload := map[string]any{"runtime": "bun", "port": 3000}
	err := writeDryRun(&buf, "up", "my-app", payload, true)
	if err != nil {
		t.Fatalf("writeDryRun: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if got["dry_run"] != true {
		t.Errorf("dry_run = %v, want true", got["dry_run"])
	}
	if got["command"] != "up" {
		t.Errorf("command = %v, want up", got["command"])
	}
	if got["id"] != "my-app" {
		t.Errorf("id = %v, want my-app", got["id"])
	}
}

func TestWriteDryRunHuman(t *testing.T) {
	var buf bytes.Buffer
	err := writeDryRun(&buf, "rm", "my-app", nil, false)
	if err != nil {
		t.Fatalf("writeDryRun: %v", err)
	}
	if !strings.Contains(buf.String(), "dry-run") {
		t.Errorf("human output missing 'dry-run': %s", buf.String())
	}
	if !strings.Contains(buf.String(), "my-app") {
		t.Errorf("human output missing app id: %s", buf.String())
	}
}

func TestDescribeListsAllCommands(t *testing.T) {
	out, err := runSub(t, "describe", nil)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	var commands []map[string]any
	if err := json.Unmarshal([]byte(out), &commands); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
	if len(commands) < 10 {
		t.Errorf("expected at least 10 commands, got %d", len(commands))
	}
	names := make(map[string]bool)
	for _, c := range commands {
		if n, ok := c["name"].(string); ok {
			names[n] = true
		}
	}
	for _, want := range []string{"ps", "up", "ensure", "deploy", "describe", "events"} {
		if !names[want] {
			t.Errorf("describe output missing command %q", want)
		}
	}
}

func TestDescribeSpecificCommand(t *testing.T) {
	out, err := runSub(t, "describe", []string{"up"})
	if err != nil {
		t.Fatalf("describe up: %v", err)
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
	if info["name"] != "up" {
		t.Errorf("name = %v, want up", info["name"])
	}
	flags, ok := info["flags"].([]any)
	if !ok || len(flags) == 0 {
		t.Error("expected non-empty flags array")
	}
}

func TestDescribeUnknownCommand(t *testing.T) {
	_, err := runSub(t, "describe", []string{"nonexistent"})
	if err == nil {
		t.Error("expected error for unknown command")
	}
}

func TestEnsureCreatesNewApp(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	out, err := runSub(t, "ensure", []string{
		"ens1", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
	})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("ens1") })
	if !strings.Contains(out, "ens1") || !strings.Contains(out, "running") {
		t.Errorf("ensure output missing expected fields:\n%s", out)
	}
}

func TestEnsureIdempotent(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)

	// First call: creates.
	_, err := runSub(t, "ensure", []string{
		"ens2", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
	})
	if err != nil {
		t.Fatalf("ensure (first): %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("ens2") })

	// Second call: should not error (idempotent).
	out, err := runSub(t, "ensure", []string{
		"ens2", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
	})
	if err != nil {
		t.Fatalf("ensure (second): %v", err)
	}
	if !strings.Contains(out, "ens2") {
		t.Errorf("ensure idempotent output missing app id:\n%s", out)
	}
}

func TestDryRunDoesNotSpawn(t *testing.T) {
	url, _ := newTestBackend(t)
	port := freeTCPPort(t)
	out, err := runSub(t, "up", []string{
		"dry1", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
		"--dry-run",
	})
	if err != nil {
		t.Fatalf("up --dry-run: %v", err)
	}
	if !strings.Contains(out, "dry-run") {
		t.Errorf("dry-run output missing marker:\n%s", out)
	}

	// Verify app was not actually spawned.
	psOut, err := runSub(t, "ps", []string{"--server", url})
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	if strings.Contains(psOut, "dry1") {
		t.Error("app was spawned despite --dry-run")
	}
}

func TestDryRunJSON(t *testing.T) {
	url, _ := newTestBackend(t)
	port := freeTCPPort(t)
	out, err := runSub(t, "up", []string{
		"dry2", "--server", url, "--json",
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
		"--dry-run",
	})
	if err != nil {
		t.Fatalf("up --dry-run --json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
	if got["dry_run"] != true {
		t.Errorf("dry_run = %v, want true", got["dry_run"])
	}
}

func TestPSFieldsFilter(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	if _, err := runSub(t, "up", []string{
		"flt", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
	}); err != nil {
		t.Fatalf("up: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("flt") })

	out, err := runSub(t, "ps", []string{"--server", url, "--json", "--fields", "id,status"})
	if err != nil {
		t.Fatalf("ps --fields: %v", err)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
	if len(arr) == 0 {
		t.Fatal("expected at least 1 app")
	}
	app := arr[0]
	if _, ok := app["id"]; !ok {
		t.Error("id field missing despite --fields id,status")
	}
	if _, ok := app["port"]; ok {
		t.Error("port field should be filtered out")
	}
}

func TestJsonInputUp(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	payload := fmt.Sprintf(`{"command":"sleep","args":["30"],"port":%d}`, port)
	out, err := runSub(t, "up", []string{
		"jin", "--server", url, "--json-input", payload,
	})
	if err != nil {
		t.Fatalf("up --json-input: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("jin") })
	if !strings.Contains(out, "jin") {
		t.Errorf("output missing app id:\n%s", out)
	}
}

func TestJsonInputInvalid(t *testing.T) {
	url, _ := newTestBackend(t)
	_, err := runSub(t, "up", []string{
		"bad", "--server", url, "--json-input", "not json",
	})
	if err == nil {
		t.Error("expected error for invalid JSON input")
	}
	if !strings.Contains(err.Error(), "--json-input") {
		t.Errorf("error should mention --json-input: %v", err)
	}
}

func TestRMDryRun(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	if _, err := runSub(t, "up", []string{
		"rmd", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
	}); err != nil {
		t.Fatalf("up: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("rmd") })

	out, err := runSub(t, "rm", []string{"rmd", "--server", url, "--dry-run"})
	if err != nil {
		t.Fatalf("rm --dry-run: %v", err)
	}
	if !strings.Contains(out, "dry-run") {
		t.Errorf("output missing dry-run marker:\n%s", out)
	}

	// App should still be running.
	app := sup.Get("rmd")
	if app == nil {
		t.Error("app was removed despite --dry-run")
	}
}

// --- release phase tests -------------------------------------------

func TestReleaseOutputStruct(t *testing.T) {
	r := releaseOutput{
		Command:    "bun run db:migrate",
		ExitCode:   0,
		DurationMS: 123,
		Output:     "Migration complete.",
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["command"] != "bun run db:migrate" {
		t.Errorf("command = %v", got["command"])
	}
	if got["exit_code"].(float64) != 0 {
		t.Errorf("exit_code = %v", got["exit_code"])
	}
}

func TestRunReleaseSuccess(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	if _, err := runSub(t, "up", []string{
		"rel-ok", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
	}); err != nil {
		t.Fatalf("up: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("rel-ok") })

	cf := commonFlags{server: url}
	result, err := runRelease(context.Background(), cf, "rel-ok", "echo hello", 10)
	if err != nil {
		t.Fatalf("runRelease: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Output, "hello") {
		t.Errorf("Output = %q, want to contain 'hello'", result.Output)
	}
}

func TestRunReleaseFailure(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	if _, err := runSub(t, "up", []string{
		"rel-fail", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
	}); err != nil {
		t.Fatalf("up: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("rel-fail") })

	cf := commonFlags{server: url}
	result, err := runRelease(context.Background(), cf, "rel-fail", "exit 1", 10)
	if err == nil {
		t.Fatal("expected error for failing release")
	}
	if result.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", result.ExitCode)
	}
}

func TestRunReleaseTimeout(t *testing.T) {
	url, sup := newTestBackend(t)
	port := freeTCPPort(t)
	if _, err := runSub(t, "up", []string{
		"rel-to", "--server", url,
		"--command", "sleep", "--arg", "30", "--port", strconv.Itoa(port),
	}); err != nil {
		t.Fatalf("up: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("rel-to") })

	cf := commonFlags{server: url}
	result, err := runRelease(context.Background(), cf, "rel-to", "sleep 30", 1)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want to contain 'timed out'", err.Error())
	}
	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (timeout)", result.ExitCode)
	}
}

// derefBool returns the bool behind p, or false if p is nil.
func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}
