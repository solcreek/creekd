package supervisor

import (
	"testing"
)

func TestFilterSupervisorEnv(t *testing.T) {
	input := []string{
		"PATH=/usr/bin",
		"HOME=/root",
		"CREEKD_ADMIN_TOKEN=secret-token-123",
		"CREEKD_ADMIN_ADDR=127.0.0.1:9080",
		"CREEKD_CGROUP_PARENT=creekd.slice",
		"CREEKD_STATE_DIR=/var/lib/creekd",
		"CREEKCTL_SERVER=http://localhost:9080",
		"CREEKCTL_TOKEN=another-secret",
		"DATABASE_URL=postgresql://localhost/mydb",
		"NODE_ENV=production",
	}

	filtered := filterSupervisorEnv(input)

	// Should keep non-CREEKD/CREEKCTL vars
	want := map[string]bool{
		"PATH":         false,
		"HOME":         false,
		"DATABASE_URL": false,
		"NODE_ENV":     false,
	}
	for _, kv := range filtered {
		for k := range want {
			if len(kv) > len(k) && kv[:len(k)+1] == k+"=" {
				want[k] = true
			}
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("expected %s to be kept, but it was filtered out", k)
		}
	}

	// Should NOT contain any CREEKD_ or CREEKCTL_ vars
	for _, kv := range filtered {
		for _, prefix := range []string{"CREEKD_", "CREEKCTL_"} {
			if len(kv) >= len(prefix) && kv[:len(prefix)] == prefix {
				t.Errorf("CREEKD/CREEKCTL var leaked to child: %s", kv)
			}
		}
	}

	// Count check: 10 input - 6 filtered = 4 kept
	if len(filtered) != 4 {
		t.Errorf("filtered len = %d, want 4 (removed 6 CREEKD/CREEKCTL vars)", len(filtered))
	}
}

func TestFilterSupervisorEnvEmpty(t *testing.T) {
	filtered := filterSupervisorEnv(nil)
	if len(filtered) != 0 {
		t.Errorf("filtered nil input should return empty, got %d", len(filtered))
	}
}

func TestFilterSupervisorEnvNoCreekVars(t *testing.T) {
	input := []string{"PATH=/usr/bin", "HOME=/root"}
	filtered := filterSupervisorEnv(input)
	if len(filtered) != 2 {
		t.Errorf("no CREEKD vars to filter, should keep all %d, got %d", len(input), len(filtered))
	}
}
