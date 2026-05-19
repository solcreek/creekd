package network

import (
	"errors"
	"net"
	"testing"
)

func TestNewIPPoolValidatesCIDR(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"10.42.0.0/24", false},
		{"10.42.0.0/30", false}, // 4 addresses: net, gw, host, bcast → allocate ok
		{"10.42.0.0/31", true},  // too small
		{"10.42.0.0/32", true},  // too small
		{"not-a-cidr", true},
		{"fe80::/64", true}, // IPv6 rejected for now
	}
	for _, c := range cases {
		_, err := NewIPPool(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NewIPPool(%q): expected error", c.in)
			}
		} else if err != nil {
			t.Errorf("NewIPPool(%q): unexpected err: %v", c.in, err)
		}
	}
}

func TestIPPoolGatewayIsFirstUsable(t *testing.T) {
	p, err := NewIPPool("10.42.0.0/24")
	if err != nil {
		t.Fatalf("NewIPPool: %v", err)
	}
	if got := p.Gateway().String(); got != "10.42.0.1" {
		t.Errorf("Gateway = %s, want 10.42.0.1", got)
	}
	if got := p.Mask(); got != 24 {
		t.Errorf("Mask = %d, want 24", got)
	}
}

func TestIPPoolAllocateAscending(t *testing.T) {
	p, _ := NewIPPool("10.42.0.0/29") // /29 → 8 addrs, gw=.1, hosts .2..6, bcast=.7
	want := []string{"10.42.0.2", "10.42.0.3", "10.42.0.4", "10.42.0.5", "10.42.0.6"}
	for _, w := range want {
		got, err := p.Allocate()
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}
		if got.String() != w {
			t.Errorf("Allocate = %s, want %s", got, w)
		}
	}
	// Next call exhausts.
	if _, err := p.Allocate(); !errors.Is(err, ErrPoolExhausted) {
		t.Errorf("expected ErrPoolExhausted, got %v", err)
	}
}

func TestIPPoolReleaseAllowsReallocation(t *testing.T) {
	p, _ := NewIPPool("10.42.0.0/30") // 4 addrs: .0 net, .1 gw, .2 host, .3 bcast
	first, _ := p.Allocate()
	if first.String() != "10.42.0.2" {
		t.Fatalf("first = %s, want 10.42.0.2", first)
	}
	if _, err := p.Allocate(); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected exhaustion, got %v", err)
	}
	p.Release(first)
	again, err := p.Allocate()
	if err != nil {
		t.Fatalf("Allocate after release: %v", err)
	}
	if again.String() != "10.42.0.2" {
		t.Errorf("re-allocated = %s, want 10.42.0.2", again)
	}
}

func TestIPPoolReleaseGatewayIsNoop(t *testing.T) {
	p, _ := NewIPPool("10.42.0.0/30")
	// Release should not free the gateway — verified by allocation
	// behaviour staying the same.
	p.Release(p.Gateway())
	got, err := p.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got.String() == p.Gateway().String() {
		t.Error("Allocate handed out the gateway after Release(gateway)")
	}
}

func TestIPPoolReleaseNilAndNonV4(t *testing.T) {
	p, _ := NewIPPool("10.42.0.0/24")
	// Should not panic.
	p.Release(nil)
	p.Release(net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
}

func TestIPPoolCIDRRoundTrips(t *testing.T) {
	p, _ := NewIPPool("10.42.0.0/24")
	if p.CIDR() != "10.42.0.0/24" {
		t.Errorf("CIDR = %s, want 10.42.0.0/24", p.CIDR())
	}
}

func TestIPPoolConcurrentAllocateRelease(t *testing.T) {
	// /24 has 254 host slots; spin up workers that allocate and release.
	p, _ := NewIPPool("10.42.0.0/24")
	const workers = 16
	const iter = 50

	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		go func() {
			for i := 0; i < iter; i++ {
				ip, err := p.Allocate()
				if err != nil {
					errCh <- err
					return
				}
				p.Release(ip)
			}
			errCh <- nil
		}()
	}
	for w := 0; w < workers; w++ {
		if err := <-errCh; err != nil {
			t.Errorf("worker err: %v", err)
		}
	}
}
