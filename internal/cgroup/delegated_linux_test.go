//go:build linux

package cgroup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOwnCgroup(t *testing.T) {
	cases := []struct {
		in, want string
		err      bool
	}{
		{"0::/system.slice/creekd.service\n", "/system.slice/creekd.service", false},
		{"0::/\n", "/", false}, // ProtectControlGroups=private: its own cgroup namespace
		{"12:pids:/user.slice\n0::/user.slice/user-1000.slice/session-3.scope\n", "/user.slice/user-1000.slice/session-3.scope", false},
		{"12:pids:/system.slice\n11:memory:/system.slice\n", "", true}, // cgroup v1 only
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := parseOwnCgroup(tc.in)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("parseOwnCgroup(%q) = %q, %v; want %q, err=%v", tc.in, got, err, tc.want, tc.err)
		}
	}
}

// The delegated manager is rooted at creekd's own cgroup, and its
// parent is relative to it: nothing it writes lies outside the subtree.
func TestNewDelegatedManagerRootsAtOwnCgroup(t *testing.T) {
	orig := procSelfCgroup
	t.Cleanup(func() { procSelfCgroup = orig })
	fake := filepath.Join(t.TempDir(), "cgroup")

	for own, wantRoot := range map[string]string{
		"0::/system.slice/creekd.service\n": "/sys/fs/cgroup/system.slice/creekd.service",
		"0::/\n":                            "/sys/fs/cgroup",
	} {
		if err := os.WriteFile(fake, []byte(own), 0o600); err != nil {
			t.Fatal(err)
		}
		procSelfCgroup = fake
		m, err := NewDelegatedManager("apps")
		if err != nil {
			t.Fatal(err)
		}
		if m.Root != wantRoot || m.Parent != "apps" || !m.Delegated {
			t.Errorf("own %q: manager = %+v, want Root %s, Parent apps, Delegated", own, m, wantRoot)
		}
		if got, want := m.parentPath(), filepath.Join(wantRoot, "apps"); got != want {
			t.Errorf("parentPath = %s, want %s", got, want)
		}
	}
}

// A delegated parent must stay below creekd's own cgroup.
func TestNewDelegatedManagerRejectsNonLocalParent(t *testing.T) {
	orig := procSelfCgroup
	t.Cleanup(func() { procSelfCgroup = orig })
	fake := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(fake, []byte("0::/system.slice/creekd.service\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	procSelfCgroup = fake

	for _, parent := range []string{"", ".", "./", "apps/..", "..", "../outside", "../../outside", "apps/../../outside", "/apps", "/sys/fs/cgroup"} {
		if m, err := NewDelegatedManager(parent); err == nil {
			t.Errorf("parent %q accepted: manager writes under %s", parent, m.parentPath())
		}
	}
	for _, parent := range []string{"apps", "apps/sub", "apps/./sub"} {
		m, err := NewDelegatedManager(parent)
		if err != nil {
			t.Errorf("parent %q refused: %v", parent, err)
			continue
		}
		if !strings.HasPrefix(m.parentPath(), m.Root+"/") {
			t.Errorf("parent %q resolves to %s, outside %s", parent, m.parentPath(), m.Root)
		}
	}
}
