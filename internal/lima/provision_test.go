package lima

import "testing"

func TestScriptsEmbedded(t *testing.T) {
	expected := []string{
		"postgres", "redis", "sqlite",
		"runtime-bun", "runtime-node", "runtime-deno",
	}
	for _, name := range expected {
		script, ok := scripts[name]
		if !ok {
			t.Errorf("script %q not found in embedded scripts", name)
			continue
		}
		if len(script) < 20 {
			t.Errorf("script %q suspiciously short (%d bytes)", name, len(script))
		}
	}
}

func TestAvailablePrimitives(t *testing.T) {
	prims := AvailablePrimitives()
	if len(prims) != 6 {
		t.Errorf("AvailablePrimitives() = %d, want 6", len(prims))
	}
}
