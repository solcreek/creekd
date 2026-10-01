//go:build linux

package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procSelfCgroup is where the kernel reports the calling process's
// cgroup. Overridable for tests.
var procSelfCgroup = "/proc/self/cgroup"

// SupervisorLeaf is the child cgroup a delegated creekd moves itself
// into, so its own cgroup holds no processes and can hand controllers
// down to the per-app cgroups (cgroup v2's "no internal processes"
// rule).
const SupervisorLeaf = "supervisor"

// NewDelegatedManager returns a Manager rooted at creekd's own cgroup —
// the subtree systemd hands a unit with Delegate=yes — instead of the
// host's cgroup root. parent is then relative to that subtree (e.g.
// "apps" → <own cgroup>/apps/<app>). Under ProtectControlGroups=private
// the service runs in its own cgroup namespace and its cgroup reads as
// "/", so the root is the mount point itself; under
// ProtectControlGroups=no it is the real /system.slice/<unit> path.
// Either way nothing outside the delegated subtree is written.
func NewDelegatedManager(parent string) (*Manager, error) {
	if parent == "" {
		return nil, errors.New("cgroup: empty parent")
	}
	own, err := ownCgroup()
	if err != nil {
		return nil, err
	}
	return &Manager{Root: filepath.Join(Root, own), Parent: parent, Delegated: true}, nil
}

// ownCgroup returns the calling process's cgroup v2 path, e.g.
// "/system.slice/creekd.service" or "/" inside a cgroup namespace.
func ownCgroup() (string, error) {
	data, err := os.ReadFile(procSelfCgroup)
	if err != nil {
		return "", fmt.Errorf("cgroup: read %s: %w", procSelfCgroup, err)
	}
	return parseOwnCgroup(string(data))
}

// parseOwnCgroup extracts the unified-hierarchy entry ("0::<path>") from
// /proc/self/cgroup text. A host still on cgroup v1 has no such line.
func parseOwnCgroup(data string) (string, error) {
	for _, line := range strings.Split(data, "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok && strings.HasPrefix(p, "/") {
			return filepath.Clean(p), nil
		}
	}
	return "", errors.New("cgroup: no cgroup v2 entry (0::) in /proc/self/cgroup; delegated cgroups need the unified hierarchy")
}

// leaveRoot moves this process from the delegated root into
// Root/SupervisorLeaf, if it is still in the root. Idempotent. Must run
// before any child is spawned into the root, or the root keeps
// processes and enabling controllers on it fails.
func (m *Manager) leaveRoot() error {
	own, err := ownCgroup()
	if err != nil {
		return err
	}
	if filepath.Join(Root, own) != filepath.Clean(m.Root) {
		return nil // already in a child (or Root was overridden in a test)
	}
	leaf := filepath.Join(m.Root, SupervisorLeaf)
	if err := os.Mkdir(leaf, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("cgroup: mkdir %s: %w", leaf, err)
	}
	// Writing a PID to cgroup.procs moves the whole process, every
	// thread included.
	if err := writeFile(filepath.Join(leaf, "cgroup.procs"), strconv.Itoa(os.Getpid())); err != nil {
		return fmt.Errorf("cgroup: move creekd into %s: %w", leaf, err)
	}
	return nil
}
