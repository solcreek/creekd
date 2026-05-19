//go:build !linux

package network

import "net"

// Bridge / Namespace / VethPair are defined on non-Linux to keep the
// supervisor package compiling on macOS dev hosts. All methods return
// ErrUnsupported on use; the IPPool in network.go is fully functional
// on every platform.

type Bridge struct {
	Name string
	Pool *IPPool
}

func (b *Bridge) Ensure() error { return ErrUnsupported }
func (b *Bridge) Delete() error { return ErrUnsupported }

type Namespace struct {
	Name string
}

func (n *Namespace) Path() string  { return "" }
func (n *Namespace) Create() error { return ErrUnsupported }
func (n *Namespace) Delete() error { return ErrUnsupported }
func (n *Namespace) Exists() bool  { return false }
func (n *Namespace) Exec(_ ...string) error {
	return ErrUnsupported
}

type VethPair struct {
	HostName, ContainerName string
	ContainerIfName         string
	Bridge                  *Bridge
	Namespace               *Namespace
	ContainerIP             net.IP
	Gateway                 net.IP
	PrefixLen               int
}

func (v *VethPair) Setup() error    { return ErrUnsupported }
func (v *VethPair) Teardown() error { return nil }

func EnsureMasquerade(_, _ string) error { return ErrUnsupported }
func RemoveMasquerade(_, _ string) error { return nil }
