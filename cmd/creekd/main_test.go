package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/solcreek/creekd/internal/supervisor"
)

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"127.0.0.1:9080", true},
		{"localhost:9080", true},
		{"[::1]:9080", true},
		{"0.0.0.0:9080", false},
		{"192.168.1.5:9080", false},
		{":9080", false}, // empty host == any interface
		{"example.com:80", false},
		{"not-an-addr", false}, // malformed
		{"unix:///var/run/creekd.sock", true},   // Unix socket = local
		{"/var/run/creekd.sock", true},           // absolute path = local
	}
	for _, c := range cases {
		if got := isLoopback(c.in); got != c.want {
			t.Errorf("isLoopback(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestConfigureSupervisorFromEnv pins the env-to-supervisor wiring.
// Pre-fix, the net-iso knobs CREEKD_NET_SUBNET / CREEKD_NET_BRIDGE_NAME
// were ignored entirely — so --net-isolation reached the supervisor
// but always failed with "NetIsolation requires both NetSubnet and
// NetBridgeName". This guards against the regression.
func TestConfigureSupervisorFromEnv(t *testing.T) {
	t.Setenv("CREEKD_LOG_DIR", "/var/log/creek")
	t.Setenv("CREEKD_CGROUP_PARENT", "creek.slice")
	t.Setenv("CREEKD_DEFAULT_MEMORY_HIGH", "256M")
	t.Setenv("CREEKD_DEFAULT_MEMORY_MAX", "1G")
	t.Setenv("CREEKD_NET_SUBNET", "10.42.0.0/24")
	t.Setenv("CREEKD_NET_BRIDGE_NAME", "creekbr0")

	sup := supervisor.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := configureSupervisorFromEnv(sup); err != nil {
		t.Fatalf("configureSupervisorFromEnv: %v", err)
	}

	if sup.LogDir != "/var/log/creek" {
		t.Errorf("LogDir = %q, want /var/log/creek", sup.LogDir)
	}
	if sup.CgroupParent != "creek.slice" {
		t.Errorf("CgroupParent = %q, want creek.slice", sup.CgroupParent)
	}
	if want := int64(256 * 1024 * 1024); sup.DefaultMemoryHigh != want {
		t.Errorf("DefaultMemoryHigh = %d, want %d", sup.DefaultMemoryHigh, want)
	}
	if want := int64(1024 * 1024 * 1024); sup.DefaultMemoryMax != want {
		t.Errorf("DefaultMemoryMax = %d, want %d", sup.DefaultMemoryMax, want)
	}
	if sup.NetSubnet != "10.42.0.0/24" {
		t.Errorf("NetSubnet = %q, want 10.42.0.0/24", sup.NetSubnet)
	}
	if sup.NetBridgeName != "creekbr0" {
		t.Errorf("NetBridgeName = %q, want creekbr0", sup.NetBridgeName)
	}
}

// TestConfigureSupervisorFromEnvRejectsBadMemoryHigh: a malformed env
// knob must fail the daemon boot rather than silently disabling the
// soft-cap default. Operators should see the error and fix the typo;
// silently dropping it could mean shipping prod without noisy-neighbor
// protection.
func TestConfigureSupervisorFromEnvRejectsBadMemoryHigh(t *testing.T) {
	t.Setenv("CREEKD_DEFAULT_MEMORY_HIGH", "not-a-size")

	sup := supervisor.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := configureSupervisorFromEnv(sup)
	if err == nil {
		t.Fatal("configureSupervisorFromEnv: want error for malformed value, got nil")
	}
	if !strings.Contains(err.Error(), "CREEKD_DEFAULT_MEMORY_HIGH") {
		t.Errorf("err = %v, want mention of CREEKD_DEFAULT_MEMORY_HIGH", err)
	}
}

func TestConfigureSupervisorFromEnvRejectsBadMemoryMax(t *testing.T) {
	t.Setenv("CREEKD_DEFAULT_MEMORY_MAX", "not-a-size")

	sup := supervisor.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := configureSupervisorFromEnv(sup)
	if err == nil {
		t.Fatal("configureSupervisorFromEnv: want error for malformed value, got nil")
	}
	if !strings.Contains(err.Error(), "CREEKD_DEFAULT_MEMORY_MAX") {
		t.Errorf("err = %v, want mention of CREEKD_DEFAULT_MEMORY_MAX", err)
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"512", 512},
		{"256M", 256 * 1024 * 1024},
		{"256MiB", 256 * 1024 * 1024},
		{"1G", 1024 * 1024 * 1024},
		{"  64K  ", 64 * 1024},
		{"2T", 2 * 1024 * 1024 * 1024 * 1024},
	}
	for _, c := range cases {
		got, err := parseSize(c.in)
		if err != nil {
			t.Errorf("parseSize(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"abc", "-5", "5X"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q): want error, got nil", bad)
		}
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("CREEKD_TEST_VAR", "")
	if got := envOr("CREEKD_TEST_VAR", "fallback"); got != "fallback" {
		t.Errorf("empty env: got %q, want fallback", got)
	}
	t.Setenv("CREEKD_TEST_VAR", "set")
	if got := envOr("CREEKD_TEST_VAR", "fallback"); got != "set" {
		t.Errorf("set env: got %q, want set", got)
	}
}

// handleVersionFlag is the early-exit path that prevents
// `creekd --version` from booting the daemon and hanging install.sh.
// Three acceptable flag spellings; anything else should fall through
// to the normal daemon startup path.
func TestHandleVersionFlag(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantPrint bool
	}{
		{"long flag", []string{"creekd", "--version"}, true},
		{"short flag", []string{"creekd", "-v"}, true},
		{"subcommand-style", []string{"creekd", "version"}, true},
		{"no args", []string{"creekd"}, false},
		{"unrelated arg", []string{"creekd", "--admin-addr=127.0.0.1:9080"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			got := handleVersionFlag(c.args, &buf)
			if got != c.wantPrint {
				t.Errorf("return = %v, want %v", got, c.wantPrint)
			}
			printed := buf.String()
			if c.wantPrint {
				if !strings.Contains(printed, version) {
					t.Errorf("output %q does not contain version %q", printed, version)
				}
			} else if printed != "" {
				t.Errorf("output should be empty for non-version args, got %q", printed)
			}
		})
	}
}

func TestListenAdminAddrTCP(t *testing.T) {
	ln, err := listenAdminAddr("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenAdminAddr TCP: %v", err)
	}
	defer ln.Close()
	if ln.Addr().Network() != "tcp" {
		t.Errorf("network = %q, want tcp", ln.Addr().Network())
	}
}

func TestListenAdminAddrUnixSocket(t *testing.T) {
	sockPath := t.TempDir() + "/admin.sock"
	ln, err := listenAdminAddr("unix://" + sockPath)
	if err != nil {
		t.Fatalf("listenAdminAddr Unix: %v", err)
	}
	defer ln.Close()
	if ln.Addr().Network() != "unix" {
		t.Errorf("network = %q, want unix", ln.Addr().Network())
	}

	// Verify socket permissions are 0600
	fi, statErr := os.Stat(sockPath)
	if statErr != nil {
		t.Fatalf("stat socket: %v", statErr)
	}
	perm := fi.Mode().Perm()
	if perm != 0600 {
		t.Errorf("socket perm = %o, want 0600", perm)
	}
}
