//go:build linux

package cgroup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Root is the cgroup v2 mount point. Constant for production; tests
// override via Manager.Root.
const Root = "/sys/fs/cgroup"

// DefaultPeriod is the default cpu.max period in microseconds. With
// Quota measured in microseconds-per-Period, this gives 100 ms windows.
const DefaultPeriod int64 = 100_000

// Limits describes the resource caps to install on a sub-cgroup.
// Zero-valued fields are treated as "no limit" (the literal string
// "max" in cgroup v2).
type Limits struct {
	// MemoryMax caps RSS+swap in bytes. 0 means unlimited.
	MemoryMax int64
	// PidsMax caps the number of tasks. 0 means unlimited.
	PidsMax int64
	// CPUQuota / CPUPeriod express cpu.max as "quota period" in
	// microseconds. If CPUQuota is 0, no CPU limit is set. If
	// CPUPeriod is 0, DefaultPeriod is used.
	CPUQuota  int64
	CPUPeriod int64
}

// Stats is a snapshot of cgroup-tracked events. Used after a child
// exit to determine whether the OOM killer was responsible.
type Stats struct {
	// OOMKill is the cumulative count of tasks killed by the OOM
	// killer inside this cgroup. Drawn from memory.events:oom_kill.
	OOMKill int64
	// OOM is the count of times this cgroup hit its memory limit
	// (regardless of whether a kill happened). memory.events:oom.
	OOM int64
}

// Manager owns a parent slice under which per-app sub-cgroups live.
// Root defaults to /sys/fs/cgroup but tests can override.
type Manager struct {
	// Root is the cgroup v2 mount point.
	Root string
	// Parent is the relative slice directory under Root that holds
	// per-app sub-cgroups. Example: "creekd.slice". Required.
	Parent string
}

// NewManager returns a Manager rooted at the standard cgroup v2 mount
// with the given parent slice name (e.g. "creekd.slice"). The parent
// directory is created on first use.
func NewManager(parent string) *Manager {
	return &Manager{Root: Root, Parent: parent}
}

// parentPath returns the absolute path of the parent slice directory.
func (m *Manager) parentPath() string {
	return filepath.Join(m.Root, m.Parent)
}

// EnsureParent creates the parent slice directory if it does not
// exist and makes sure cpu + memory + pids controllers are delegated
// to it. Idempotent.
func (m *Manager) EnsureParent() error {
	if m.Parent == "" {
		return errors.New("cgroup: empty parent slice")
	}
	if err := os.MkdirAll(m.parentPath(), 0o755); err != nil {
		return fmt.Errorf("cgroup: mkdir parent %s: %w", m.parentPath(), err)
	}

	// Enable the controllers we need on the root's cgroup.subtree_control
	// so children of the parent can use memory / cpu / pids. cgroup v2
	// requires a chain: root must delegate to parent's parent, which
	// is the root itself; we only need to write at the root.
	rootSubtree := filepath.Join(m.Root, "cgroup.subtree_control")
	if err := enableControllers(rootSubtree, "+cpu", "+memory", "+pids"); err != nil {
		return err
	}
	parentSubtree := filepath.Join(m.parentPath(), "cgroup.subtree_control")
	if err := enableControllers(parentSubtree, "+cpu", "+memory", "+pids"); err != nil {
		return err
	}
	return nil
}

// enableControllers writes "+cpu +memory +pids" (or whatever tokens
// are given) to path. Each token can be a one-shot write; some kernels
// require single-token writes, so we try the combined form first and
// fall back to individual writes on EINVAL.
func enableControllers(path string, tokens ...string) error {
	combined := strings.Join(tokens, " ")
	if err := writeFile(path, combined); err == nil {
		return nil
	}
	for _, t := range tokens {
		if err := writeFile(path, t); err != nil {
			// "Device or resource busy" is harmless when the controller
			// is already enabled on a sibling. We tolerate EBUSY.
			if errors.Is(err, syscall.EBUSY) {
				continue
			}
			return fmt.Errorf("cgroup: enable %s on %s: %w", t, path, err)
		}
	}
	return nil
}

// Cgroup is a single sub-cgroup under Manager.Parent. Hold a Cgroup
// for the duration of an app's lifetime; call Remove on cleanup.
type Cgroup struct {
	mgr   *Manager
	name  string
	path  string
	limit Limits
}

// Create makes a new sub-cgroup named name under m.Parent and writes
// the limits described by lim. The cgroup is empty (no tasks) until
// the caller spawns a process into it via OpenFD + SysProcAttr.
func (m *Manager) Create(name string, lim Limits) (*Cgroup, error) {
	if name == "" {
		return nil, errors.New("cgroup: empty name")
	}
	if strings.ContainsAny(name, "/\x00") {
		return nil, fmt.Errorf("cgroup: invalid name %q", name)
	}
	if err := m.EnsureParent(); err != nil {
		return nil, err
	}
	path := filepath.Join(m.parentPath(), name)
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("cgroup: mkdir %s: %w", path, err)
	}
	c := &Cgroup{mgr: m, name: name, path: path, limit: lim}
	if err := c.applyLimits(lim); err != nil {
		// Best-effort cleanup if we partially wrote limits.
		_ = os.Remove(path)
		return nil, err
	}
	return c, nil
}

// Path returns the absolute path of this cgroup directory.
func (c *Cgroup) Path() string { return c.path }

// Name returns the leaf name of this cgroup (passed to Create).
func (c *Cgroup) Name() string { return c.name }

// applyLimits writes the configured limits to cgroup files. Zero
// values become "max".
func (c *Cgroup) applyLimits(lim Limits) error {
	if err := writeMaxOrValue(filepath.Join(c.path, "memory.max"), lim.MemoryMax); err != nil {
		return err
	}
	if err := writeMaxOrValue(filepath.Join(c.path, "pids.max"), lim.PidsMax); err != nil {
		return err
	}
	if lim.CPUQuota > 0 {
		period := lim.CPUPeriod
		if period <= 0 {
			period = DefaultPeriod
		}
		v := fmt.Sprintf("%d %d", lim.CPUQuota, period)
		if err := writeFile(filepath.Join(c.path, "cpu.max"), v); err != nil {
			return err
		}
	}
	// CPUQuota == 0: leave cpu.max at its default "max <period>". No-op.
	return nil
}

// writeMaxOrValue writes "max" when v <= 0, otherwise the decimal v.
func writeMaxOrValue(path string, v int64) error {
	if v <= 0 {
		return writeFile(path, "max")
	}
	return writeFile(path, strconv.FormatInt(v, 10))
}

// OpenFD opens the cgroup directory as a file descriptor suitable for
// exec.Cmd.SysProcAttr.CgroupFD. The caller closes the returned *os.File
// after Cmd.Start (the kernel duplicates the fd via clone3).
func (c *Cgroup) OpenFD() (*os.File, error) {
	f, err := os.Open(c.path)
	if err != nil {
		return nil, fmt.Errorf("cgroup: open %s: %w", c.path, err)
	}
	return f, nil
}

// AddProcess writes pid to cgroup.procs, moving an already-running
// process into this cgroup. Prefer CLONE_INTO_CGROUP (via OpenFD +
// SysProcAttr) when starting a fresh child — that way the child is
// born inside the cgroup with no enforcement gap. AddProcess is
// useful for adopting external processes after the fact.
func (c *Cgroup) AddProcess(pid int) error {
	return writeFile(filepath.Join(c.path, "cgroup.procs"), strconv.Itoa(pid))
}

// Stats reads memory.events and returns the current snapshot.
func (c *Cgroup) Stats() (Stats, error) {
	var s Stats
	data, err := os.ReadFile(filepath.Join(c.path, "memory.events"))
	if err != nil {
		return s, fmt.Errorf("cgroup: read memory.events: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		n, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "oom_kill":
			s.OOMKill = n
		case "oom":
			s.OOM = n
		}
	}
	return s, nil
}

// MemoryCurrent reads memory.current — the cgroup's instantaneous
// RSS-equivalent in bytes (resident + page cache attributable to the
// cgroup, per the kernel's accounting).
func (c *Cgroup) MemoryCurrent() (int64, error) {
	return readInt64(filepath.Join(c.path, "memory.current"))
}

// MemoryMax reads memory.max. Returns (0, nil) when the file holds
// the literal "max" sentinel (no limit), matching the semantics of
// Limits.MemoryMax.
func (c *Cgroup) MemoryMax() (int64, error) {
	data, err := os.ReadFile(filepath.Join(c.path, "memory.max"))
	if err != nil {
		return 0, fmt.Errorf("cgroup: read memory.max: %w", err)
	}
	v := strings.TrimSpace(string(data))
	if v == "max" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cgroup: parse memory.max %q: %w", v, err)
	}
	return n, nil
}

// PidsCurrent reads pids.current — the number of tasks (threads +
// processes) currently inside the cgroup.
func (c *Cgroup) PidsCurrent() (int64, error) {
	return readInt64(filepath.Join(c.path, "pids.current"))
}

// CPUUsageMicros parses cpu.stat and returns the accumulated CPU time
// in microseconds (usage_usec field). Used as a counter — diff two
// snapshots over time to derive a CPU% gauge.
func (c *Cgroup) CPUUsageMicros() (int64, error) {
	data, err := os.ReadFile(filepath.Join(c.path, "cpu.stat"))
	if err != nil {
		return 0, fmt.Errorf("cgroup: read cpu.stat: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "usage_usec" {
			continue
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("cgroup: parse usage_usec %q: %w", fields[1], err)
		}
		return n, nil
	}
	return 0, fmt.Errorf("cgroup: usage_usec not found in cpu.stat")
}

// readInt64 reads a single decimal integer from path. Used for
// single-value cgroup files (memory.current, pids.current).
func readInt64(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("cgroup: read %s: %w", path, err)
	}
	v := strings.TrimSpace(string(data))
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cgroup: parse %s value %q: %w", path, v, err)
	}
	return n, nil
}

// Remove deletes the cgroup directory. The kernel requires the cgroup
// to be empty (cgroup.procs empty). Returns the error from rmdir;
// callers typically run this after the supervised process has exited.
func (c *Cgroup) Remove() error {
	// cgroup v2 requires cgroup.procs to be empty before rmdir.
	// Best-effort: poll briefly so we tolerate slow kernel cleanup.
	deadline := time.Now().Add(200 * time.Millisecond)
	for {
		if err := os.Remove(c.path); err == nil {
			return nil
		} else if !errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("cgroup: remove %s: %w", c.path, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cgroup: remove %s: still busy after grace", c.path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// writeFile writes content to path, opening with O_WRONLY (no
// truncation needed for cgroup files — kernel handles updates).
func writeFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.WriteString(f, content); err != nil {
		return err
	}
	return nil
}
