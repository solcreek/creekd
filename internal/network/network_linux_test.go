//go:build linux

package network

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// requireNetPrivilege gates each Linux integration test on the
// presence of CAP_NET_ADMIN. Unprivileged Docker hits the skip path.
func requireNetPrivilege(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skipf("iproute2 (ip) not available: %v", err)
	}
	// Probe by creating + deleting a throwaway bridge. Capture
	// combined output so a real privilege failure is distinguishable
	// from a test-binary PATH issue. The probe name must fit in
	// IFNAMSIZ (15 chars), so keep it tight.
	probe := fmt.Sprintf("cp%d", time.Now().UnixNano()%1_000_000)
	cmd := exec.Command("ip", "link", "add", probe, "type", "bridge")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ip link add failed (need --privileged container or CAP_NET_ADMIN): %v output=%s",
			err, strings.TrimSpace(string(out)))
	}
	_ = exec.Command("ip", "link", "del", probe).Run()
}

// uniqueBridgeName keeps concurrent test runs from colliding. The
// result must fit IFNAMSIZ (15 chars), so prefixes should be short
// (≤6 chars) to leave room for the suffix.
func uniqueBridgeName(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()%1_000_000)
}

func TestBridgeEnsureCreates(t *testing.T) {
	requireNetPrivilege(t)
	pool, err := NewIPPool("10.43.0.0/24")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	b := &Bridge{Name: uniqueBridgeName("cbr"), Pool: pool}
	t.Cleanup(func() { _ = b.Delete() })

	if err := b.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if exists, _ := linkExists(b.Name); !exists {
		t.Errorf("bridge %s not present after Ensure", b.Name)
	}
}

func TestBridgeEnsureIdempotent(t *testing.T) {
	requireNetPrivilege(t)
	pool, _ := NewIPPool("10.44.0.0/24")
	b := &Bridge{Name: uniqueBridgeName("cid"), Pool: pool}
	t.Cleanup(func() { _ = b.Delete() })

	for i := 0; i < 3; i++ {
		if err := b.Ensure(); err != nil {
			t.Errorf("Ensure call #%d: %v", i, err)
		}
	}
}

func TestNamespaceCreateAndDelete(t *testing.T) {
	requireNetPrivilege(t)
	ns := &Namespace{Name: fmt.Sprintf("creekd-ns-%d", time.Now().UnixNano())}
	t.Cleanup(func() { _ = ns.Delete() })

	if ns.Exists() {
		t.Fatal("namespace exists before Create")
	}
	if err := ns.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !ns.Exists() {
		t.Error("namespace missing after Create")
	}
	// Idempotent.
	if err := ns.Create(); err != nil {
		t.Errorf("second Create: %v", err)
	}
	if err := ns.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ns.Exists() {
		t.Error("namespace present after Delete")
	}
	// Idempotent on the delete side too.
	if err := ns.Delete(); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

func TestNamespaceExecLoSeesOnlyLoopback(t *testing.T) {
	requireNetPrivilege(t)
	ns := &Namespace{Name: fmt.Sprintf("creekd-lo-%d", time.Now().UnixNano())}
	if err := ns.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = ns.Delete() })

	// A fresh netns only has `lo`. List interfaces via `ip -n <ns> -o link`.
	out, err := exec.Command("ip", "-n", ns.Name, "-o", "link").CombinedOutput()
	if err != nil {
		t.Fatalf("ip -n link: %v output=%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "lo:") {
		t.Errorf("fresh netns should only have lo; got:\n%s", out)
	}
}

func TestVethPairSetupAndTeardown(t *testing.T) {
	requireNetPrivilege(t)
	pool, _ := NewIPPool("10.45.0.0/24")
	br := &Bridge{Name: uniqueBridgeName("cvb"), Pool: pool}
	if err := br.Ensure(); err != nil {
		t.Fatalf("bridge: %v", err)
	}
	t.Cleanup(func() { _ = br.Delete() })

	ns := &Namespace{Name: fmt.Sprintf("creekd-vns-%d", time.Now().UnixNano())}
	if err := ns.Create(); err != nil {
		t.Fatalf("netns: %v", err)
	}
	t.Cleanup(func() { _ = ns.Delete() })

	containerIP, err := pool.Allocate()
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	// Names must fit IFNAMSIZ (15 chars). Use a short suffix.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%10_000)
	v := &VethPair{
		HostName:      "vh" + suffix,
		ContainerName: "vc" + suffix,
		Bridge:        br,
		Namespace:     ns,
		ContainerIP:   containerIP,
		Gateway:       pool.Gateway(),
		PrefixLen:     pool.Mask(),
	}
	t.Cleanup(func() {
		_ = v.Teardown()
		pool.Release(containerIP)
	})

	if err := v.Setup(); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// Verify: container side is named eth0 inside the netns and has the IP.
	out, err := exec.Command("ip", "-n", ns.Name, "-o", "addr", "show", "eth0").CombinedOutput()
	if err != nil {
		t.Fatalf("addr show: %v output=%s", err, out)
	}
	want := containerIP.String()
	if !strings.Contains(string(out), want) {
		t.Errorf("eth0 missing IP %s; got: %s", want, out)
	}

	// Default route via gateway.
	out, err = exec.Command("ip", "-n", ns.Name, "route", "show", "default").CombinedOutput()
	if err != nil {
		t.Fatalf("route show: %v output=%s", err, out)
	}
	if !strings.Contains(string(out), pool.Gateway().String()) {
		t.Errorf("default route missing %s; got: %s", pool.Gateway(), out)
	}

	// Host side attached to bridge.
	out, err = exec.Command("ip", "-o", "link", "show", "master", br.Name).CombinedOutput()
	if err != nil {
		t.Fatalf("link show master: %v output=%s", err, out)
	}
	if !strings.Contains(string(out), v.HostName) {
		t.Errorf("host veth %s not attached to bridge %s; got: %s",
			v.HostName, br.Name, out)
	}

	// Teardown removes host side (kernel reaps the peer).
	if err := v.Teardown(); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if exists, _ := linkExists(v.HostName); exists {
		t.Errorf("host veth %s still present after Teardown", v.HostName)
	}
}

func TestVethPairValidatesInputs(t *testing.T) {
	cases := []struct {
		name string
		v    VethPair
	}{
		{"missing names", VethPair{Bridge: &Bridge{}, Namespace: &Namespace{}}},
		{"missing bridge", VethPair{HostName: "h", ContainerName: "c", Namespace: &Namespace{}}},
		{"missing ns", VethPair{HostName: "h", ContainerName: "c", Bridge: &Bridge{}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.v.Setup(); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestEnsureMasqueradeAddsAndRemoves(t *testing.T) {
	requireNetPrivilege(t)
	if _, err := exec.LookPath("iptables"); err != nil {
		t.Skipf("iptables not installed: %v", err)
	}
	subnet := "10.46.0.0/24"
	bridge := "cmsq-test" // ≤ 15 chars to satisfy iptables' IFNAMSIZ check

	// Best-effort cleanup before AND after to keep concurrent runs sane.
	_ = RemoveMasquerade(subnet, bridge)
	t.Cleanup(func() { _ = RemoveMasquerade(subnet, bridge) })

	if err := EnsureMasquerade(subnet, bridge); err != nil {
		t.Fatalf("EnsureMasquerade: %v", err)
	}
	// Second call must be a no-op (idempotent).
	if err := EnsureMasquerade(subnet, bridge); err != nil {
		t.Errorf("EnsureMasquerade (second): %v", err)
	}

	// Verify the rule is actually present via iptables-save.
	out, err := exec.Command("iptables-save", "-t", "nat").CombinedOutput()
	if err != nil {
		t.Skipf("iptables-save unavailable: %v", err)
	}
	if !strings.Contains(string(out), subnet) || !strings.Contains(string(out), "MASQUERADE") {
		t.Errorf("rule missing from iptables-save; output:\n%s", out)
	}

	if err := RemoveMasquerade(subnet, bridge); err != nil {
		t.Errorf("RemoveMasquerade: %v", err)
	}
	out, _ = exec.Command("iptables-save", "-t", "nat").CombinedOutput()
	if strings.Contains(string(out), subnet) {
		t.Errorf("rule still present after Remove; output:\n%s", out)
	}
}
