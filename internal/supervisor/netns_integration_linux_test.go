//go:build linux

package supervisor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/creekd/internal/dispatch"
)

// requireNetPrivilegeSup is the supervisor-side gate for netns tests:
// CAP_NET_ADMIN must be available so we can create bridges + veth.
func requireNetPrivilegeSup(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skipf("iproute2 not available: %v", err)
	}
	probe := fmt.Sprintf("cp%d", time.Now().UnixNano()%1_000_000)
	cmd := exec.Command("ip", "link", "add", probe, "type", "bridge")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("net privilege missing: %v output=%s", err, out)
	}
	_ = exec.Command("ip", "link", "del", probe).Run()
}

// newNetSupervisor returns a test-tuned supervisor with a per-test
// unique bridge + subnet so concurrent tests don't collide. The
// bridge is torn down via t.Cleanup.
func newNetSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	sup := newTestSupervisor()
	// Each test gets its own /24 in the 10.42.x.0 range by hashing
	// the test name into the third octet (avoiding 0 and 255).
	suffix := time.Now().UnixNano() % 200
	sup.NetSubnet = fmt.Sprintf("10.42.%d.0/24", 30+suffix)
	sup.NetBridgeName = fmt.Sprintf("nbr%d", time.Now().UnixNano()%1_000_000)
	t.Cleanup(func() {
		_ = exec.Command("ip", "link", "del", sup.NetBridgeName).Run()
		// Best-effort: also remove the matching masquerade rule.
		_ = exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING",
			"-s", sup.NetSubnet, "!", "-o", sup.NetBridgeName, "-j", "MASQUERADE").Run()
	})
	return sup
}

// TestNetIsolationAllocatesIPAndSetsNamespace: spawn an app with
// NetIsolation=true and verify the supervisor (1) allocated an IP
// from its pool, (2) created the netns, (3) the spawned process
// lives in a different net namespace than the supervisor itself.
func TestNetIsolationAllocatesIPAndSetsNamespace(t *testing.T) {
	requireNetPrivilegeSup(t)

	sup := newNetSupervisor(t)
	app, err := sup.Spawn(Config{
		ID:           "net1",
		Command:      "/bin/sleep",
		Args:         []string{"30"},
		Port:         19600,
		NetIsolation: true,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop(app.ID) })

	if app.NetIP == nil {
		t.Fatal("App.NetIP not assigned")
	}
	if !strings.HasPrefix(app.NetIP.String(), "10.42.") {
		t.Errorf("App.NetIP = %s, want prefix 10.42.", app.NetIP)
	}
	if app.NetGateway == nil {
		t.Error("App.NetGateway not assigned")
	}

	// /proc/<pid>/ns/net must differ from supervisor's own. We poll
	// because `ip netns exec` does its setns + inner exec after the
	// parent's cmd.Start returns — there's a brief window in which
	// /proc/<pid>/ns/net still points at the host net namespace.
	selfNS, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatalf("readlink self ns/net: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var childNS string
	for time.Now().Before(deadline) {
		childNS, err = os.Readlink(fmt.Sprintf("/proc/%d/ns/net", app.PID()))
		if err == nil && childNS != selfNS {
			return // success
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("child ns/net = %s, same as supervisor — isolation never applied", childNS)
}

// TestNetIsolationCleansUpOnStop: after Stop, the netns must be gone
// and the IP returned to the pool.
func TestNetIsolationCleansUpOnStop(t *testing.T) {
	requireNetPrivilegeSup(t)

	sup := newNetSupervisor(t)
	app, err := sup.Spawn(Config{
		ID:           "net-cleanup",
		Command:      "/bin/sleep",
		Args:         []string{"30"},
		Port:         19601,
		NetIsolation: true,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	netnsPath := "/var/run/netns/net-cleanup"
	if _, err := os.Stat(netnsPath); err != nil {
		t.Fatalf("netns missing pre-Stop: %v", err)
	}

	// Snapshot the IP before Stop clears it in teardownAppNetwork.
	originalIP := app.NetIP.String()

	if err := sup.Stop(app.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if _, err := os.Stat(netnsPath); err == nil {
		t.Errorf("netns %s present after Stop", netnsPath)
	}
	if app.NetIP != nil {
		t.Errorf("App.NetIP not cleared after Stop: %s", app.NetIP)
	}
	// The pool only guarantees "some free IP", not "the most recently
	// freed". But re-spawning the same ID after a clean Stop should
	// succeed and yield an IP from the same subnet.
	again, err := sup.Spawn(Config{
		ID: "net-cleanup", Command: "/bin/sleep", Args: []string{"30"}, Port: 19601,
		NetIsolation: true,
	})
	if err != nil {
		t.Fatalf("re-Spawn: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("net-cleanup") })
	if again.NetIP == nil {
		t.Errorf("re-Spawn produced no NetIP")
	}
	t.Logf("freed %s → re-allocated %s", originalIP, again.NetIP)
}

// TestNetIsolationDispatchRoutesToContainerIP: spawn an HTTP child
// with NetIsolation, route via the container IP, send a request
// through the dispatch router, and verify the response.
func TestNetIsolationDispatchRoutesToContainerIP(t *testing.T) {
	requireNetPrivilegeSup(t)

	sup := newNetSupervisor(t)
	app, err := sup.Spawn(Config{
		ID:      "net-http",
		Command: os.Args[0],
		Args:    []string{"-test.run=^$"},
		Port:    19602,
		Env: []string{
			"CREEK_TEST_HTTPAPP=1",
			"SIGNATURE=netiso",
		},
		NetIsolation: true,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop(app.ID) })

	if app.NetIP == nil {
		t.Fatal("no NetIP")
	}

	// Direct probe to the container IP (bridge route from host).
	url := fmt.Sprintf("http://%s:%d/health", app.NetIP, app.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	var lastErr error
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !strings.Contains(string(body), "netiso") {
				t.Fatalf("body = %q, want signature 'netiso'", string(body))
			}
			return
		}
		lastErr = err
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("never reached child at %s: %v", url, lastErr)
}

// TestNetIsolationHealthProbeReachesContainer: the supervisor's
// HTTPHealthChecker must hit the app's NetIP, not the host
// loopback. Pre-fix, every health probe for a net-isolated app went
// to http://127.0.0.1:<port> — nothing listening there since the
// child is in its own netns — so the supervisor would mark every
// net-isolated app unhealthy and restart-loop it.
func TestNetIsolationHealthProbeReachesContainer(t *testing.T) {
	requireNetPrivilegeSup(t)

	sup := newNetSupervisor(t)
	// Probe path: the test HTTP child serves /health → 200 always
	// (HEALTH_MODE="" → always-pass). Enable the background probe
	// loop with a short interval so a failing probe shows up fast.
	sup.HealthChecker = &HTTPHealthChecker{Path: "/health"}
	sup.HealthCheckInterval = 50 * time.Millisecond
	sup.HealthCheckTimeout = 1 * time.Second
	sup.HealthCheckFailureThreshold = 3

	app, err := sup.Spawn(Config{
		ID:      "net-health",
		Command: os.Args[0],
		Args:    []string{"-test.run=^$"},
		Port:    19603,
		Env: []string{
			"CREEK_TEST_HTTPAPP=1",
			"SIGNATURE=net-health",
		},
		NetIsolation: true,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop(app.ID) })

	if app.NetIP == nil {
		t.Fatal("no NetIP")
	}

	// Give the probe loop ~600ms (multiple intervals). If the probe
	// were still hitting 127.0.0.1, we'd see HealthFailures climb
	// past the threshold and the app would restart.
	originalPID := app.PID()
	time.Sleep(600 * time.Millisecond)

	if got := app.HealthFailures(); got > 0 {
		t.Errorf("HealthFailures = %d after 600ms, want 0 — probe is missing the netns",
			got)
	}
	if app.PID() != originalPID {
		t.Errorf("app restarted (PID %d → %d) — probe failures tripped restart",
			originalPID, app.PID())
	}
}

// TestNetIsolationDeployRoutesViaNetIP: deploying a net-isolated
// app must use router.SetAddr with v2's NetIP. Pre-fix, deploy used
// router.Set which defaulted host to 127.0.0.1, breaking traffic to
// any newly-deployed net-isolated app.
func TestNetIsolationDeployRoutesViaNetIP(t *testing.T) {
	requireNetPrivilegeSup(t)

	sup := newNetSupervisor(t)
	sup.HealthChecker = &HTTPHealthChecker{Path: "/health"}
	sup.HealthCheckTimeout = 1 * time.Second
	sup.HealthCheckInterval = 0

	v1, err := sup.Spawn(Config{
		ID:      "net-deploy",
		Command: os.Args[0],
		Args:    []string{"-test.run=^$"},
		Port:    19604,
		Env: []string{
			"CREEK_TEST_HTTPAPP=1",
			"SIGNATURE=v1",
		},
		NetIsolation: true,
	})
	if err != nil {
		t.Fatalf("Spawn v1: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop(v1.ID) })
	if v1.NetIP == nil {
		t.Fatal("v1 NetIP nil")
	}

	// Set up a router and seed the v1 route via SetAddr, mirroring
	// what the admin spawn handler does for net-iso apps.
	router := dispatch.NewRouter()
	if err := router.SetAddr(v1.ID, v1.NetIP.String(), v1.Port); err != nil {
		t.Fatalf("router.SetAddr v1: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v2, err := sup.Deploy(ctx, router, DeployConfig{
		Config: Config{
			ID:      "net-deploy",
			Command: os.Args[0],
			Args:    []string{"-test.run=^$"},
			Port:    19605,
			Env: []string{
				"CREEK_TEST_HTTPAPP=1",
				"SIGNATURE=v2",
			},
			NetIsolation: true,
		},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if v2.NetIP == nil {
		t.Fatal("v2 NetIP nil")
	}

	// After deploy, the router's backend for ID should resolve to
	// v2's NetIP, NOT 127.0.0.1. This is the SetAddr-vs-Set fix.
	b := router.Get("net-deploy")
	if b == nil {
		t.Fatal("router has no route after deploy")
	}
	if b.Host != v2.NetIP.String() {
		t.Errorf("router host = %q, want v2 NetIP %q (deploy ignored NetIP)",
			b.Host, v2.NetIP.String())
	}
	if b.Port != v2.Port {
		t.Errorf("router port = %d, want %d", b.Port, v2.Port)
	}
}
