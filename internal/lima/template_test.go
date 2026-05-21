package lima

import (
	"strings"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.CPUs != 2 {
		t.Errorf("CPUs = %d, want 2", cfg.CPUs)
	}
	if cfg.Memory != "2GiB" {
		t.Errorf("Memory = %q, want %q", cfg.Memory, "2GiB")
	}
	if len(cfg.Ports) != 2 {
		t.Fatalf("default ports = %d, want 2 (app + admin)", len(cfg.Ports))
	}
}

func TestAddPrimitive(t *testing.T) {
	cfg := DefaultConfig()
	before := len(cfg.Ports)
	cfg.AddPrimitive("postgres")
	if len(cfg.Ports) != before+1 {
		t.Fatalf("ports after AddPrimitive(postgres) = %d, want %d", len(cfg.Ports), before+1)
	}
	cfg.AddPrimitive("redis")
	if len(cfg.Ports) != before+2 {
		t.Fatalf("ports after AddPrimitive(redis) = %d, want %d", len(cfg.Ports), before+2)
	}
}

func TestAddPrimitiveIdempotent(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AddPrimitive("postgres")
	n := len(cfg.Ports)
	cfg.AddPrimitive("postgres")
	if len(cfg.Ports) != n {
		t.Error("AddPrimitive should be idempotent")
	}
}

func TestAddPrimitiveUnknown(t *testing.T) {
	cfg := DefaultConfig()
	before := len(cfg.Ports)
	cfg.AddPrimitive("unknown-thing")
	if len(cfg.Ports) != before {
		t.Error("unknown primitive should not add ports")
	}
}

func TestRender(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AddPrimitive("postgres")
	yaml := cfg.Render()

	checks := []string{
		"cpus: 2",
		`memory: "2GiB"`,
		"guestPort: 5432",
		"hostPort: 15432",
		"guestPort: 3000",
		"hostPort: 13000",
		"apt-get update",
		"containerd:",
		"system: false",
	}
	for _, check := range checks {
		if !strings.Contains(yaml, check) {
			t.Errorf("rendered YAML missing %q", check)
		}
	}
}

func TestRenderPortForwards(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AddPrimitive("postgres")
	cfg.AddPrimitive("redis")
	yaml := cfg.Render()

	if !strings.Contains(yaml, "hostPort: 15432") {
		t.Error("missing postgres port forward")
	}
	if !strings.Contains(yaml, "hostPort: 16379") {
		t.Error("missing redis port forward")
	}
}
