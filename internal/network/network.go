package network

import (
	"errors"
	"fmt"
	"net"
	"sync"
)

// ErrUnsupported is returned when a Linux-only operation is invoked
// on a non-Linux build.
var ErrUnsupported = errors.New("network: not supported on this platform")

// ErrPoolExhausted is returned by IPPool.Allocate when the pool has
// no free addresses.
var ErrPoolExhausted = errors.New("network: ip pool exhausted")

// IPPool allocates host IPs from a CIDR for per-app container IPs.
// The first usable address is reserved as the bridge gateway (returned
// by Gateway()); subsequent calls to Allocate hand out the remaining
// addresses in ascending order.
//
// Allocate / Release / Gateway are safe for concurrent use.
type IPPool struct {
	cidr    *net.IPNet
	gateway net.IP

	mu    sync.Mutex
	inuse map[string]struct{}
}

// NewIPPool constructs a pool covering cidr. cidr must be an IPv4
// CIDR with at least 4 usable addresses (network, gateway, and two
// host slots) — anything smaller is rejected.
func NewIPPool(cidr string) (*IPPool, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("network: parse cidr %q: %w", cidr, err)
	}
	if ipnet.IP.To4() == nil {
		return nil, fmt.Errorf("network: ipv4 cidr required, got %q", cidr)
	}
	ones, bits := ipnet.Mask.Size()
	if bits-ones < 2 {
		return nil, fmt.Errorf("network: subnet %q too small (need ≥/30)", cidr)
	}
	// First usable host = network + 1 → gateway.
	gw := nextIP(ipnet.IP.To4())
	return &IPPool{
		cidr:    ipnet,
		gateway: gw,
		inuse:   map[string]struct{}{string(gw.To4()): {}}, // reserve gateway
	}, nil
}

// CIDR returns the canonical CIDR string of the pool.
func (p *IPPool) CIDR() string {
	return p.cidr.String()
}

// Mask returns the prefix length (e.g. 24 for /24).
func (p *IPPool) Mask() int {
	ones, _ := p.cidr.Mask.Size()
	return ones
}

// Gateway returns the gateway IP (first usable host in the pool's
// CIDR; permanently reserved).
func (p *IPPool) Gateway() net.IP {
	return cloneIP(p.gateway)
}

// Allocate returns the next free IP, or ErrPoolExhausted when full.
func (p *IPPool) Allocate() (net.IP, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Start scanning after the gateway.
	cur := nextIP(p.gateway)
	bcast := broadcast(p.cidr)
	for !cur.Equal(bcast) {
		key := string(cur.To4())
		if _, taken := p.inuse[key]; !taken {
			p.inuse[key] = struct{}{}
			return cloneIP(cur), nil
		}
		cur = nextIP(cur)
	}
	return nil, ErrPoolExhausted
}

// Release returns ip to the pool. Releasing the gateway or an
// unallocated IP is a no-op.
func (p *IPPool) Release(ip net.IP) {
	if ip == nil {
		return
	}
	v4 := ip.To4()
	if v4 == nil {
		return
	}
	if v4.Equal(p.gateway) {
		return
	}
	p.mu.Lock()
	delete(p.inuse, string(v4))
	p.mu.Unlock()
}

// nextIP returns ip incremented by 1, treating IPv4 as a big-endian
// integer. Panics on non-IPv4 input.
func nextIP(ip net.IP) net.IP {
	v4 := ip.To4()
	if v4 == nil {
		panic("nextIP: non-IPv4")
	}
	out := make(net.IP, 4)
	copy(out, v4)
	for i := 3; i >= 0; i-- {
		out[i]++
		if out[i] != 0 {
			break
		}
	}
	return out
}

// broadcast returns the broadcast (last) address of cidr.
func broadcast(cidr *net.IPNet) net.IP {
	ip := cidr.IP.To4()
	mask := cidr.Mask
	out := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		out[i] = ip[i] | ^mask[i]
	}
	return out
}

// cloneIP returns a defensive copy.
func cloneIP(ip net.IP) net.IP {
	out := make(net.IP, len(ip))
	copy(out, ip)
	return out
}
