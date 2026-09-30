package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// privilegedLinux makes autoRunAsEnabled see a privileged Linux host,
// the only place per-app UIDs are allocated. resolveRunAsLocked is
// tested directly, like applyDefaultSandbox: the subject is the
// identity policy, not the setpriv plumbing that follows it.
func privilegedLinux(t *testing.T) {
	t.Helper()
	origGOOS, origCap := goos, hasSetIDCaps
	t.Cleanup(func() { goos, hasSetIDCaps = origGOOS, origCap })
	goos = "linux"
	hasSetIDCaps = func() bool { return true }
}

// newRunAsSupervisor returns a supervisor with base 1000 and a
// high-water file in a temp dir.
func newRunAsSupervisor(t *testing.T, hwmPath string) *Supervisor {
	t.Helper()
	s := New(nil)
	s.AppUIDBase = 1000
	s.UIDStatePath = hwmPath
	return s
}

// resolve settles cfg under the lock and registers the result, the way
// spawnUnchecked does, so later calls see it as in use.
func resolve(t *testing.T, s *Supervisor, cfg Config) (*RunAs, error) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.resolveRunAsLocked(&cfg); err != nil {
		return nil, err
	}
	s.apps[cfg.ID] = &App{ID: cfg.ID, runAs: cfg.RunAs}
	return cfg.RunAs, nil
}

func mustUID(t *testing.T, s *Supervisor, id string, want int) {
	t.Helper()
	ra, err := resolve(t, s, Config{ID: id})
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	if ra == nil || ra.UID != want || ra.GID != want {
		t.Fatalf("%s: RunAs = %+v, want uid=gid=%d", id, ra, want)
	}
}

func TestRunAsAllocatesDistinctNeverReusedUIDs(t *testing.T) {
	privilegedLinux(t)
	hwm := filepath.Join(t.TempDir(), "app-uid-hwm")
	s := newRunAsSupervisor(t, hwm)

	mustUID(t, s, "a", 1000)
	mustUID(t, s, "b", 1001)

	// Deleting b must not free 1001: a new app would inherit b's files.
	delete(s.apps, "b")
	mustUID(t, s, "c", 1002)

	data, err := os.ReadFile(hwm)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "1002" {
		t.Errorf("persisted high-water mark = %q, want 1002", got)
	}
}

func TestRunAsHighWaterSurvivesRestart(t *testing.T) {
	privilegedLinux(t)
	hwm := filepath.Join(t.TempDir(), "app-uid-hwm")
	s1 := newRunAsSupervisor(t, hwm)
	mustUID(t, s1, "a", 1000)
	mustUID(t, s1, "b", 1001)

	s2 := newRunAsSupervisor(t, hwm)
	if err := s2.LoadUIDHighWater(); err != nil {
		t.Fatal(err)
	}
	mustUID(t, s2, "c", 1002)
}

func TestRunAsObserveRaisesHighWater(t *testing.T) {
	privilegedLinux(t)
	s := newRunAsSupervisor(t, filepath.Join(t.TempDir(), "hwm"))
	// A restored entry already owns 5000 on disk: a legacy entry that
	// needs a fresh UID must be handed something above it.
	s.ObserveRunAs(Config{ID: "restored", RunAs: &RunAs{UID: 5000, GID: 5000}})
	mustUID(t, s, "legacy", 5001)
}

func TestRunAsSkipsUIDsHeldByExplicitApps(t *testing.T) {
	privilegedLinux(t)
	s := newRunAsSupervisor(t, "")
	// An explicit RunAs below the base does not move the mark...
	if _, err := resolve(t, s, Config{ID: "ops", RunAs: &RunAs{UID: 900, GID: 900}}); err != nil {
		t.Fatal(err)
	}
	// ...but one inside the range is never handed out again.
	if _, err := resolve(t, s, Config{ID: "pinned", RunAs: &RunAs{UID: 1000, GID: 1000}}); err != nil {
		t.Fatal(err)
	}
	mustUID(t, s, "auto", 1001)
}

func TestRunAsExplicitValidation(t *testing.T) {
	privilegedLinux(t)
	s := newRunAsSupervisor(t, "")
	if _, err := resolve(t, s, Config{ID: "a", RunAs: &RunAs{UID: 7000, GID: 7000}}); err != nil {
		t.Fatal(err)
	}

	t.Run("uid 0 is refused: root goes through RunAsRoot", func(t *testing.T) {
		if _, err := resolve(t, s, Config{ID: "r", RunAs: &RunAs{UID: 0, GID: 0}}); err == nil {
			t.Fatal("want an error for uid 0")
		}
	})
	t.Run("a UID another app runs as is refused", func(t *testing.T) {
		_, err := resolve(t, s, Config{ID: "b", RunAs: &RunAs{UID: 7000, GID: 7000}})
		if !errors.Is(err, ErrRunAsInUse) {
			t.Fatalf("err = %v, want ErrRunAsInUse", err)
		}
	})
	t.Run("the deploy counterpart may share its app's UID", func(t *testing.T) {
		if _, err := resolve(t, s, Config{ID: deployTempID("a"), RunAs: &RunAs{UID: 7000, GID: 7000}}); err != nil {
			t.Fatalf("v2 of a: %v", err)
		}
	})
}

func TestRunAsOff(t *testing.T) {
	cases := []struct {
		name  string
		setup func(s *Supervisor)
		cfg   Config
	}{
		{"RunAsRoot", func(*Supervisor) {}, Config{ID: "x", RunAsRoot: true, RunAs: &RunAs{UID: 5, GID: 5}}},
		{"AppUIDBase 0", func(s *Supervisor) { s.AppUIDBase = 0 }, Config{ID: "x"}},
		{"DisableDefaultSandbox", func(s *Supervisor) { s.DisableDefaultSandbox = true }, Config{ID: "x"}},
		{"no CAP_SETUID/CAP_SETGID", func(*Supervisor) { hasSetIDCaps = func() bool { return false } }, Config{ID: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			privilegedLinux(t)
			s := newRunAsSupervisor(t, "")
			tc.setup(s)
			ra, err := resolve(t, s, tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if ra != nil {
				t.Fatalf("RunAs = %+v, want nil (root)", ra)
			}
		})
	}
}

func TestRunAsExplicitNeedsLinux(t *testing.T) {
	origGOOS := goos
	t.Cleanup(func() { goos = origGOOS })
	goos = "darwin"
	s := newRunAsSupervisor(t, "")
	if _, err := resolve(t, s, Config{ID: "x", RunAs: &RunAs{UID: 7000, GID: 7000}}); err == nil {
		t.Fatal("an explicit RunAs off Linux must be refused, not silently dropped")
	}
}

func TestLoadUIDHighWaterMalformed(t *testing.T) {
	hwm := filepath.Join(t.TempDir(), "app-uid-hwm")
	if err := os.WriteFile(hwm, []byte("not-a-number\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newRunAsSupervisor(t, hwm)
	if err := s.LoadUIDHighWater(); err == nil {
		t.Fatal("a malformed mark must fail startup: guessing low would reuse UIDs")
	}
}

func TestCloneConfigCopiesRunAs(t *testing.T) {
	in := Config{ID: "x", RunAs: &RunAs{UID: 1, GID: 2}}
	out := CloneConfig(in)
	out.RunAs.UID = 99
	if in.RunAs.UID != 1 {
		t.Fatal("CloneConfig aliased RunAs")
	}
}

func TestRunAsExplicitNeedsSetIDCaps(t *testing.T) {
	privilegedLinux(t)
	hasSetIDCaps = func() bool { return false }
	s := newRunAsSupervisor(t, "")
	if _, err := resolve(t, s, Config{ID: "x", RunAs: &RunAs{UID: 7000, GID: 7000}}); err == nil {
		t.Fatal("an explicit RunAs creekd cannot switch to must be refused at spawn, not crash-loop")
	}
}

// Auto RunAs must not hinge on CAP_SYS_ADMIN: the shipped unit runs
// creekd as a non-root user with CAP_SETUID/CAP_SETGID and no
// CAP_SYS_ADMIN, and that is exactly where apps would otherwise share
// creekd's UID.
func TestRunAsWithoutSysAdmin(t *testing.T) {
	privilegedLinux(t)
	origCap := hasSysAdminCap
	t.Cleanup(func() { hasSysAdminCap = origCap })
	hasSysAdminCap = func() bool { return false }
	s := newRunAsSupervisor(t, "")
	mustUID(t, s, "a", 1000)
}

func TestParseSetIDCaps(t *testing.T) {
	cases := map[string]bool{
		"CapEff:\t000001ffffffffff\n": true,  // root
		"CapEff:\t00000000000004c0\n": true,  // NET_BIND_SERVICE + SETUID + SETGID
		"CapEff:\t0000000000000400\n": false, // NET_BIND_SERVICE only (the old unit)
		"CapEff:\t0000000000000080\n": false, // SETUID without SETGID
		"CapEff:\tzz\n":               false,
		"Name:\tcreekd\n":             false,
	}
	for in, want := range cases {
		if got := parseSetIDCaps([]byte(in)); got != want {
			t.Errorf("parseSetIDCaps(%q) = %v, want %v", in, got, want)
		}
	}
}
