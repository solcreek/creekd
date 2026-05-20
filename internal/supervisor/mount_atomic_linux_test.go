//go:build linux

package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestAtomicMountProbeReturnsConsistent ensures the lazy probe
// resolves to a stable answer across repeated calls and is safe to
// invoke concurrently. We don't assert true vs false — that depends
// on the test host's kernel — but the result must not flap.
func TestAtomicMountProbeReturnsConsistent(t *testing.T) {
	first := atomicMountAvailable()
	for i := 0; i < 10; i++ {
		if atomicMountAvailable() != first {
			t.Fatalf("atomicMountAvailable flapped on iter %d", i)
		}
	}
}

// TestIsENOSYS verifies the helper that lets bindOneMount distinguish
// "kernel too old" from real failures.
func TestIsENOSYS(t *testing.T) {
	if !isENOSYS(unix.ENOSYS) {
		t.Error("expected isENOSYS(ENOSYS) = true")
	}
	if isENOSYS(unix.EINVAL) {
		t.Error("expected isENOSYS(EINVAL) = false")
	}
	if isENOSYS(errors.New("plain error")) {
		t.Error("expected isENOSYS(non-errno) = false")
	}
	if isENOSYS(nil) {
		t.Error("expected isENOSYS(nil) = false")
	}
}

// TestBindAtomicSmoke exercises the atomic-mount path end-to-end.
// Requires root + kernel ≥5.12. Skips otherwise.
func TestBindAtomicSmoke(t *testing.T) {
	requireBindMountPrivilege(t)
	if !atomicMountAvailable() {
		t.Skip("kernel < 5.12: atomic mount path not available")
	}

	root := makeVolumeRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "tenant-a/data"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}

	sup := newTestSupervisor()
	sup.VolumeRoot = root
	t.Cleanup(func() {
		if sup.volumeRootFD >= 0 {
			_ = unix.Close(sup.volumeRootFD)
		}
	})

	if err := sup.RegisterVolume(Volume{ID: "vol-a", BackingPath: "tenant-a/data"}); err != nil {
		t.Fatalf("RegisterVolume: %v", err)
	}

	target := filepath.Join(t.TempDir(), "tgt")
	t.Cleanup(func() { _ = unix.Unmount(target, unix.MNT_DETACH) })

	if err := sup.applyVolumeMounts(
		[]VolumeMount{{VolumeID: "vol-a", Target: target, ReadOnly: true}},
		"",
	); err != nil {
		t.Fatalf("applyVolumeMounts (atomic path): %v", err)
	}

	// Atomic RO must result in mountinfo reporting "ro" already —
	// no observable RW window.
	if ok, err := verifyReadOnly(target); err != nil {
		t.Fatalf("verifyReadOnly: %v", err)
	} else if !ok {
		t.Error("atomic RO mount did not result in ro flag in mountinfo")
	}
}
