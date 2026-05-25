package upgrade

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSwapBinary_ReplacesDstAtomically covers the basic
// replacement contract: after SwapBinary, dst contains src's
// content. The rename trick under the hood is what makes this
// atomic — covered indirectly by the dst-stays-untouched test
// below.
func TestSwapBinary_ReplacesDstAtomically(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new")
	dst := filepath.Join(dir, "old")
	if err := os.WriteFile(src, []byte("new-bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old-bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SwapBinary(src, dst); err != nil {
		t.Fatalf("SwapBinary: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-bytes" {
		t.Errorf("dst contents = %q, want %q", got, "new-bytes")
	}
}

// TestSwapBinary_PreservesExecutableBit covers the mode contract:
// SwapBinary ensures the destination is executable (0755) even
// if src was written with a permissive umask that stripped +x.
func TestSwapBinary_PreservesExecutableBit(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new")
	dst := filepath.Join(dir, "old")
	// Write src with mode 0644 — no exec bit.
	if err := os.WriteFile(src, []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SwapBinary(src, dst); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("dst mode = %o, want exec bit set", info.Mode().Perm())
	}
}

// TestSwapBinary_NoTempLeftOnSuccess covers the cleanliness
// contract: no .upgrade-*.tmp sibling file MUST remain after a
// successful swap (rename consumes it). A leftover would confuse
// the next upgrade attempt and fill the destination directory
// with stale tmp files over time.
func TestSwapBinary_NoTempLeftOnSuccess(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new")
	dst := filepath.Join(dir, "old")
	if err := os.WriteFile(src, []byte("a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SwapBinary(src, dst); err != nil {
		t.Fatal(err)
	}
	leftover, err := filepath.Glob(filepath.Join(dir, ".upgrade-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftover) != 0 {
		t.Errorf("tmp leftover: %v, want none", leftover)
	}
}

// TestSwapBinary_MissingSrcLeavesDstAlone covers the rollback
// contract: when src doesn't exist, SwapBinary errors AND the
// original dst is preserved. No half-upgrade window.
func TestSwapBinary_MissingSrcLeavesDstAlone(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "does-not-exist")
	dst := filepath.Join(dir, "old")
	if err := os.WriteFile(dst, []byte("intact"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SwapBinary(src, dst); err == nil {
		t.Error("SwapBinary with missing src should error")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "intact" {
		t.Errorf("dst clobbered on failed swap: %q, want %q", got, "intact")
	}
}
