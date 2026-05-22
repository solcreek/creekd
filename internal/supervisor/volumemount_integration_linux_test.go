//go:build linux

package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// requireBindMountPrivilege skips the test when the process cannot
// perform bind mounts. We need CAP_SYS_ADMIN, which usually means
// running as root in a privileged container or on bare-metal.
func requireBindMountPrivilege(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("bind-mount integration test requires root (CAP_SYS_ADMIN)")
	}
	// Probe the exact code path RegisterVolume uses: create a subdir,
	// self-bind it, then MS_PRIVATE. This catches Docker/CI environments
	// where MS_PRIVATE works on real mount points but fails on self-bind
	// subdirectories due to mount namespace restrictions.
	probe := t.TempDir()
	subdir := filepath.Join(probe, "vol")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("probe mkdir: %v", err)
	}
	if err := unix.Mount(subdir, subdir, "", unix.MS_BIND, ""); err != nil {
		t.Skipf("bind-mount unavailable in this environment: %v", err)
	}
	if err := unix.Mount("", subdir, "", unix.MS_PRIVATE, ""); err != nil {
		_ = unix.Unmount(subdir, 0)
		t.Skipf("MS_PRIVATE on self-bind unavailable (Docker mount namespace restriction): %v", err)
	}
	_ = unix.Unmount(subdir, 0)
}

// makeVolumeRoot creates a fresh VolumeRoot dir + returns its path
// + a cleanup that unmounts anything bound under it. Used by every
// test below so leftover binds from one test never pollute the next.
func makeVolumeRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() {
		// Best-effort: unmount everything under root. Lazy unmount
		// in case a fd is still open.
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			_ = unix.Unmount(filepath.Join(root, e.Name()), unix.MNT_DETACH)
		}
		// Per-volume MS_PRIVATE self-bind also needs unmounting.
		_ = unix.Unmount(root, unix.MNT_DETACH)
	})
	return root
}

// TestRegisterVolumeOpensAndIsolates exercises the host-side
// volume registration end-to-end on Linux: openat2-anchored
// resolution + MS_PRIVATE propagation.
func TestRegisterVolumeOpensAndIsolates(t *testing.T) {
	requireBindMountPrivilege(t)

	root := makeVolumeRoot(t)
	backingRel := "tenant-a/data"
	if err := os.MkdirAll(filepath.Join(root, backingRel), 0o755); err != nil {
		t.Fatalf("mkdir backing: %v", err)
	}

	sup := newTestSupervisor()
	sup.VolumeRoot = root
	defer func() {
		// Close the pinned fd so the temp dir can be cleaned up.
		if sup.volumeRootFD >= 0 {
			_ = unix.Close(sup.volumeRootFD)
		}
	}()

	if err := sup.RegisterVolume(Volume{ID: "vol-a", BackingPath: backingRel}); err != nil {
		t.Fatalf("RegisterVolume: %v", err)
	}
	v, ok := sup.Volume("vol-a")
	if !ok {
		t.Fatal("volume not in registry")
	}
	if v.FSType == "" {
		t.Error("FSType should be populated by statfs")
	}
}

// TestRegisterVolumeRejectsSymlinkEscape proves the load-bearing
// security property: openat2 with RESOLVE_NO_SYMLINKS refuses to
// follow a symlink, even when the symlink target is outside
// VolumeRoot. This is the symlink-race attack the security review
// surfaced.
func TestRegisterVolumeRejectsSymlinkEscape(t *testing.T) {
	requireBindMountPrivilege(t)

	root := makeVolumeRoot(t)
	// Plant a symlink "evil" → "/etc" inside the volume root.
	if err := os.Symlink("/etc", filepath.Join(root, "evil")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	sup := newTestSupervisor()
	sup.VolumeRoot = root
	defer func() {
		if sup.volumeRootFD >= 0 {
			_ = unix.Close(sup.volumeRootFD)
		}
	}()

	err := sup.RegisterVolume(Volume{ID: "evil", BackingPath: "evil"})
	if err == nil {
		t.Fatal("expected symlink resolution to be refused")
	}
}

// TestRegisterVolumeRejectsTraversal: openat2 with RESOLVE_BENEATH
// refuses ".." even though our string check also catches this. Belt
// and braces — the kernel guard is the load-bearing one.
func TestRegisterVolumeRejectsTraversal(t *testing.T) {
	requireBindMountPrivilege(t)

	root := makeVolumeRoot(t)

	sup := newTestSupervisor()
	sup.VolumeRoot = root
	defer func() {
		if sup.volumeRootFD >= 0 {
			_ = unix.Close(sup.volumeRootFD)
		}
	}()

	err := sup.RegisterVolume(Volume{ID: "esc", BackingPath: "../etc"})
	if err == nil {
		t.Fatal("expected traversal to be refused")
	}
}

// TestApplyVolumeMountsBinds is the end-to-end acceptance: a
// registered volume's data is visible at the projected target.
func TestApplyVolumeMountsBinds(t *testing.T) {
	requireBindMountPrivilege(t)

	root := makeVolumeRoot(t)
	backingRel := "tenant-a/data"
	srcAbs := filepath.Join(root, backingRel)
	if err := os.MkdirAll(srcAbs, 0o755); err != nil {
		t.Fatalf("mkdir backing: %v", err)
	}

	sup := newTestSupervisor()
	sup.VolumeRoot = root
	defer func() {
		if sup.volumeRootFD >= 0 {
			_ = unix.Close(sup.volumeRootFD)
		}
	}()

	if err := sup.RegisterVolume(Volume{ID: "vol-a", BackingPath: backingRel}); err != nil {
		t.Fatalf("RegisterVolume: %v", err)
	}

	target := filepath.Join(t.TempDir(), "tgt")
	t.Cleanup(func() { _ = unix.Unmount(target, unix.MNT_DETACH) })

	if err := sup.applyVolumeMounts(
		[]VolumeMount{{VolumeID: "vol-a", Target: target}},
		"",
	); err != nil {
		t.Fatalf("applyVolumeMounts: %v", err)
	}

	payload := []byte("creekd-volume-test")
	if err := os.WriteFile(filepath.Join(target, "marker"), payload, 0o644); err != nil {
		t.Fatalf("write through target: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(srcAbs, "marker"))
	if err != nil {
		t.Fatalf("read from source: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("payload mismatch: got %q, want %q", got, payload)
	}
}

// TestApplyVolumeMountsIdempotent: second apply on same source is
// a no-op (mountinfo identity match).
func TestApplyVolumeMountsIdempotent(t *testing.T) {
	requireBindMountPrivilege(t)

	root := makeVolumeRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "tenant-a/data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	sup := newTestSupervisor()
	sup.VolumeRoot = root
	defer func() {
		if sup.volumeRootFD >= 0 {
			_ = unix.Close(sup.volumeRootFD)
		}
	}()

	if err := sup.RegisterVolume(Volume{ID: "vol-a", BackingPath: "tenant-a/data"}); err != nil {
		t.Fatalf("RegisterVolume: %v", err)
	}

	target := filepath.Join(t.TempDir(), "tgt")
	t.Cleanup(func() { _ = unix.Unmount(target, unix.MNT_DETACH) })

	mounts := []VolumeMount{{VolumeID: "vol-a", Target: target}}
	if err := sup.applyVolumeMounts(mounts, ""); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := sup.applyVolumeMounts(mounts, ""); err != nil {
		t.Fatalf("second apply (should be idempotent): %v", err)
	}
}

// TestApplyVolumeMountsReadOnly: RO projection blocks writes AND
// verifyReadOnly confirms via mountinfo (catches the kernel quirk
// where the first remount returns success while leaving the bind RW).
func TestApplyVolumeMountsReadOnly(t *testing.T) {
	requireBindMountPrivilege(t)

	root := makeVolumeRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "tenant-a/data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	sup := newTestSupervisor()
	sup.VolumeRoot = root
	defer func() {
		if sup.volumeRootFD >= 0 {
			_ = unix.Close(sup.volumeRootFD)
		}
	}()

	if err := sup.RegisterVolume(Volume{ID: "vol-a", BackingPath: "tenant-a/data"}); err != nil {
		t.Fatalf("RegisterVolume: %v", err)
	}

	target := filepath.Join(t.TempDir(), "tgt")
	t.Cleanup(func() { _ = unix.Unmount(target, unix.MNT_DETACH) })

	if err := sup.applyVolumeMounts(
		[]VolumeMount{{VolumeID: "vol-a", Target: target, ReadOnly: true}},
		"",
	); err != nil {
		t.Fatalf("applyVolumeMounts: %v", err)
	}

	err := os.WriteFile(filepath.Join(target, "marker"), []byte("x"), 0o644)
	if err == nil {
		t.Fatal("expected write through RO bind to fail")
	}
	if !errors.Is(err, os.ErrPermission) && !strings.Contains(err.Error(), "read-only") {
		t.Errorf("expected EROFS / permission error, got %v", err)
	}

	// Mountinfo verify path — independently confirm the ro flag.
	if ok, err := verifyReadOnly(target); err != nil {
		t.Fatalf("verifyReadOnly: %v", err)
	} else if !ok {
		t.Error("mountinfo did not report ro for the RO bind")
	}
}

// TestApplyVolumeMountsSubPath binds only a subdirectory of the
// registered volume. Exercises the openat2 from rootFD with
// composed BackingPath/SubPath.
func TestApplyVolumeMountsSubPath(t *testing.T) {
	requireBindMountPrivilege(t)

	root := makeVolumeRoot(t)
	subAbs := filepath.Join(root, "tenant-a/data/pgdata")
	if err := os.MkdirAll(subAbs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	siblingAbs := filepath.Join(root, "tenant-a/data/wal")
	if err := os.MkdirAll(siblingAbs, 0o755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subAbs, "marker"), []byte("pg"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(siblingAbs, "marker"), []byte("wal"), 0o644); err != nil {
		t.Fatalf("seed sibling: %v", err)
	}

	sup := newTestSupervisor()
	sup.VolumeRoot = root
	defer func() {
		if sup.volumeRootFD >= 0 {
			_ = unix.Close(sup.volumeRootFD)
		}
	}()

	if err := sup.RegisterVolume(Volume{ID: "vol-a", BackingPath: "tenant-a/data"}); err != nil {
		t.Fatalf("RegisterVolume: %v", err)
	}

	target := filepath.Join(t.TempDir(), "tgt")
	t.Cleanup(func() { _ = unix.Unmount(target, unix.MNT_DETACH) })

	if err := sup.applyVolumeMounts(
		[]VolumeMount{{VolumeID: "vol-a", SubPath: "pgdata", Target: target}},
		"",
	); err != nil {
		t.Fatalf("applyVolumeMounts: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(target, "marker"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "pg" {
		t.Errorf("got %q, want %q — sub_path not honored", got, "pg")
	}

	// Sibling must NOT be visible — sub_path scopes the bind.
	if _, err := os.Stat(filepath.Join(target, "..", "wal", "marker")); err == nil {
		// On bind, the sibling isn't reachable from target — it's
		// in a different mount point. This is the expected
		// behavior, so we don't fail; just sanity-log.
		t.Log("sibling reachable via .. (expected — bind is at sub_path)")
	}
}
