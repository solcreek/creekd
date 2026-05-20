//go:build !linux

package cgroup

import (
	"errors"
	"os"
)

// On non-Linux platforms, the cgroup package compiles but every
// constructor returns ErrUnsupported. This lets the supervisor depend
// on the package unconditionally while reserving real enforcement
// for the Linux production path.
var ErrUnsupported = errors.New("cgroup: not supported on this platform")

// Limits matches the Linux-side shape so callers can construct it
// portably; the values are simply ignored on non-Linux.
type Limits struct {
	MemoryHigh int64
	MemoryMax  int64
	PidsMax    int64
	CPUQuota   int64
	CPUPeriod  int64
}

// Stats mirrors the Linux-side shape. Always returns zero on non-Linux.
type Stats struct {
	OOMKill int64
	OOM     int64
}

// Manager is a no-op container on non-Linux. Methods return ErrUnsupported.
type Manager struct {
	Root   string
	Parent string
}

// NewManager returns a Manager that always errors on use.
func NewManager(parent string) *Manager { return &Manager{Parent: parent} }

// EnsureParent always returns ErrUnsupported on non-Linux.
func (m *Manager) EnsureParent() error { return ErrUnsupported }

// Create always returns ErrUnsupported on non-Linux.
func (m *Manager) Create(_ string, _ Limits) (*Cgroup, error) { return nil, ErrUnsupported }

// Cgroup is a placeholder struct on non-Linux. All methods are no-ops
// or return ErrUnsupported.
type Cgroup struct{}

// Path returns an empty string on non-Linux.
func (c *Cgroup) Path() string { return "" }

// Name returns an empty string on non-Linux.
func (c *Cgroup) Name() string { return "" }

// OpenFD always returns ErrUnsupported on non-Linux.
func (c *Cgroup) OpenFD() (*os.File, error) { return nil, ErrUnsupported }

// AddProcess always returns ErrUnsupported on non-Linux.
func (c *Cgroup) AddProcess(_ int) error { return ErrUnsupported }

// Stats returns an empty snapshot on non-Linux.
func (c *Cgroup) Stats() (Stats, error) { return Stats{}, ErrUnsupported }

// MemoryCurrent / MemoryMax / PidsCurrent / CPUUsageMicros all return
// ErrUnsupported on non-Linux. Supervisor's stats handler treats the
// error as "cgroup_enabled=false" and surfaces only OS-level fields.
func (c *Cgroup) MemoryCurrent() (int64, error)  { return 0, ErrUnsupported }
func (c *Cgroup) MemoryMax() (int64, error)      { return 0, ErrUnsupported }
func (c *Cgroup) PidsCurrent() (int64, error)    { return 0, ErrUnsupported }
func (c *Cgroup) CPUUsageMicros() (int64, error) { return 0, ErrUnsupported }

// Remove always returns nil on non-Linux (there is nothing to remove).
func (c *Cgroup) Remove() error { return nil }
