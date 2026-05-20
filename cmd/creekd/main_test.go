package main

import (
	"bytes"
	"io"
	"log/slog"
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
	t.Setenv("CREEKD_NET_SUBNET", "10.42.0.0/24")
	t.Setenv("CREEKD_NET_BRIDGE_NAME", "creekbr0")

	sup := supervisor.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	configureSupervisorFromEnv(sup)

	if sup.LogDir != "/var/log/creek" {
		t.Errorf("LogDir = %q, want /var/log/creek", sup.LogDir)
	}
	if sup.CgroupParent != "creek.slice" {
		t.Errorf("CgroupParent = %q, want creek.slice", sup.CgroupParent)
	}
	if sup.NetSubnet != "10.42.0.0/24" {
		t.Errorf("NetSubnet = %q, want 10.42.0.0/24", sup.NetSubnet)
	}
	if sup.NetBridgeName != "creekbr0" {
		t.Errorf("NetBridgeName = %q, want creekbr0", sup.NetBridgeName)
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
