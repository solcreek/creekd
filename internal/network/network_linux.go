//go:build linux

package network

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// runIP shells out to `ip` and returns combined output on failure.
// Wrapping a single helper keeps every callsite consistent in
// error-formatting.
func runIP(args ...string) error {
	cmd := exec.Command("ip", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %w (output: %s)",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runIPTables shells out to `iptables` for nat / filter rule edits.
func runIPTables(args ...string) error {
	cmd := exec.Command("iptables", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %w (output: %s)",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Bridge owns a Linux bridge interface plus its gateway IP.
// Bridge.Ensure is idempotent — subsequent calls reuse the existing
// interface and only re-apply settings that drift.
type Bridge struct {
	Name string // e.g. "creekd0"
	Pool *IPPool
}

// Ensure creates the bridge if it does not exist, assigns the
// gateway IP from its pool, and brings the interface up. Safe to
// call repeatedly during creekd startup.
func (b *Bridge) Ensure() error {
	if b.Name == "" {
		return errors.New("network: empty bridge name")
	}
	if b.Pool == nil {
		return errors.New("network: bridge needs an IPPool")
	}

	exists, err := linkExists(b.Name)
	if err != nil {
		return fmt.Errorf("network: probe bridge: %w", err)
	}
	if !exists {
		if err := runIP("link", "add", "name", b.Name, "type", "bridge"); err != nil {
			return err
		}
	}

	gw := fmt.Sprintf("%s/%d", b.Pool.Gateway(), b.Pool.Mask())
	if err := runIP("addr", "add", gw, "dev", b.Name); err != nil {
		// Already-assigned errors are fine for idempotence. iproute2
		// versions differ on phrasing: older builds say
		// "File exists"; newer ones (Debian bookworm) say
		// "Address already assigned".
		msg := err.Error()
		if !strings.Contains(msg, "File exists") && !strings.Contains(msg, "already assigned") {
			return err
		}
	}

	if err := runIP("link", "set", b.Name, "up"); err != nil {
		return err
	}
	return nil
}

// Delete removes the bridge. Used by tests; production should never
// need to tear down the bridge during normal operation.
func (b *Bridge) Delete() error {
	return runIP("link", "del", b.Name)
}

// linkExists returns true iff `ip link show <name>` succeeds.
func linkExists(name string) (bool, error) {
	err := exec.Command("ip", "link", "show", name).Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		// Exit 1 is "device not found"; treat as cleanly-not-exists.
		return false, nil
	}
	return false, err
}

// Namespace is a persistent Linux network namespace mounted at
// /var/run/netns/<Name>. Used so the supervisor can configure the
// netns before any process enters it.
type Namespace struct {
	Name string
}

// Path returns the bind-mount path of the namespace.
func (n *Namespace) Path() string {
	return filepath.Join("/var/run/netns", n.Name)
}

// Create makes the namespace via `ip netns add`. Idempotent if the
// namespace already exists (returns nil).
func (n *Namespace) Create() error {
	if n.Name == "" {
		return errors.New("network: empty namespace name")
	}
	if n.Exists() {
		return nil
	}
	if err := os.MkdirAll("/var/run/netns", 0o755); err != nil {
		return fmt.Errorf("network: prep netns dir: %w", err)
	}
	return runIP("netns", "add", n.Name)
}

// Delete removes the namespace. Returns nil if it does not exist.
// The kernel reaps any veth interface inside the namespace
// automatically on deletion.
func (n *Namespace) Delete() error {
	if !n.Exists() {
		return nil
	}
	return runIP("netns", "del", n.Name)
}

// Exists returns true if /var/run/netns/<Name> is mounted.
func (n *Namespace) Exists() bool {
	_, err := os.Stat(n.Path())
	return err == nil
}

// Exec runs argv inside this namespace. Caller supplies pre-resolved
// command + args; the helper handles the `ip netns exec` wrapping.
// Returns combined output on failure for better diagnostics.
func (n *Namespace) Exec(argv ...string) error {
	full := append([]string{"netns", "exec", n.Name}, argv...)
	return runIP(full...)
}

// VethPair is a host ↔ container veth pair. Setup creates the pair,
// attaches the host side to a bridge, moves the container side into
// a netns, renames it to ContainerIfName, and configures its IP and
// default route. Teardown undoes all of that by removing the host
// side (kernel reaps the peer automatically).
type VethPair struct {
	// HostName / ContainerName are kernel-level interface names. Keep
	// them ≤ 15 chars (IFNAMSIZ limit).
	HostName      string
	ContainerName string

	// ContainerIfName is the name the interface takes once inside the
	// namespace. Defaults to "eth0" if empty.
	ContainerIfName string

	Bridge    *Bridge
	Namespace *Namespace

	ContainerIP net.IP // e.g. 10.42.0.10
	Gateway     net.IP // e.g. 10.42.0.1 (== Bridge gateway)
	PrefixLen   int    // e.g. 24
}

// Setup wires the pair into place. Steps:
//
//  1. ip link add veth-h type veth peer name veth-c
//  2. ip link set veth-h master <bridge>
//  3. ip link set veth-h up
//  4. ip link set veth-c netns <ns>
//  5. ip -n <ns> link set veth-c name eth0
//  6. ip -n <ns> addr add <ip>/<mask> dev eth0
//  7. ip -n <ns> link set lo up
//  8. ip -n <ns> link set eth0 up
//  9. ip -n <ns> route add default via <gateway>
//
// If any step fails, Setup attempts a best-effort Teardown before
// returning the error.
func (v *VethPair) Setup() error {
	if v.HostName == "" || v.ContainerName == "" {
		return errors.New("network: veth needs host + container names")
	}
	if v.Bridge == nil || v.Namespace == nil {
		return errors.New("network: veth needs Bridge and Namespace")
	}
	if v.ContainerIP == nil || v.Gateway == nil {
		return errors.New("network: veth needs ContainerIP and Gateway")
	}
	if v.PrefixLen == 0 {
		return errors.New("network: veth needs PrefixLen")
	}
	if v.ContainerIfName == "" {
		v.ContainerIfName = "eth0"
	}

	if err := v.run(); err != nil {
		_ = v.Teardown()
		return err
	}
	return nil
}

// run carries out the Setup steps; separated so Setup can wrap with
// a cleanup defer.
func (v *VethPair) run() error {
	if err := runIP("link", "add", v.HostName, "type", "veth",
		"peer", "name", v.ContainerName); err != nil {
		return err
	}
	if err := runIP("link", "set", v.HostName, "master", v.Bridge.Name); err != nil {
		return err
	}
	if err := runIP("link", "set", v.HostName, "up"); err != nil {
		return err
	}
	if err := runIP("link", "set", v.ContainerName,
		"netns", v.Namespace.Name); err != nil {
		return err
	}

	addr := fmt.Sprintf("%s/%d", v.ContainerIP, v.PrefixLen)

	for _, args := range [][]string{
		{"-n", v.Namespace.Name, "link", "set", v.ContainerName, "name", v.ContainerIfName},
		{"-n", v.Namespace.Name, "link", "set", "lo", "up"},
		{"-n", v.Namespace.Name, "addr", "add", addr, "dev", v.ContainerIfName},
		{"-n", v.Namespace.Name, "link", "set", v.ContainerIfName, "up"},
		{"-n", v.Namespace.Name, "route", "add", "default", "via", v.Gateway.String()},
	} {
		if err := runIP(args...); err != nil {
			return err
		}
	}
	return nil
}

// Teardown removes the host side of the pair. Best-effort — errors
// are logged via the returned wrapped error but callers typically
// ignore them during cleanup.
func (v *VethPair) Teardown() error {
	if exists, _ := linkExists(v.HostName); !exists {
		return nil
	}
	return runIP("link", "del", v.HostName)
}

// EnsureMasquerade installs an iptables MASQUERADE rule for subnet
// in the nat table's POSTROUTING chain. Idempotent — uses -C first
// to check, only adds if absent.
//
// Rule shape: `-s <subnet> ! -o <bridge> -j MASQUERADE`. The negation
// on -o means "traffic leaving the bridge" — packets that came in,
// got routed, and are heading out via the host's default interface.
func EnsureMasquerade(subnet, bridge string) error {
	rule := []string{"-t", "nat", "-s", subnet, "!", "-o", bridge, "-j", "MASQUERADE"}
	if err := runIPTables(append([]string{"-C", "POSTROUTING"}, rule...)...); err == nil {
		return nil // already present
	}
	return runIPTables(append([]string{"-A", "POSTROUTING"}, rule...)...)
}

// RemoveMasquerade deletes the rule installed by EnsureMasquerade.
// Returns nil if it was not present.
func RemoveMasquerade(subnet, bridge string) error {
	rule := []string{"-t", "nat", "-s", subnet, "!", "-o", bridge, "-j", "MASQUERADE"}
	if err := runIPTables(append([]string{"-C", "POSTROUTING"}, rule...)...); err != nil {
		return nil
	}
	return runIPTables(append([]string{"-D", "POSTROUTING"}, rule...)...)
}
