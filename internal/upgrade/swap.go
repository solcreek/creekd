package upgrade

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// SwapBinary atomically replaces dst with src using the standard
// write-to-tmp + rename pattern. On Linux + macOS, rename(2)
// replaces dst even if a process is currently executing it — the
// kernel unlinks the old inode but the running process continues
// from the still-open file descriptor. The next exec sees the new
// binary.
//
// mode preserved is whatever's on src (typically 0755 from
// goreleaser's tarball). If src has no executable bit, SwapBinary
// applies 0755 anyway — every release artifact should be runnable.
//
// On any error, dst is left untouched: the tmp file is removed and
// the original binary remains in place. There is no half-upgrade
// window.
func SwapBinary(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("upgrade: stat %s: %w", src, err)
	}
	mode := info.Mode().Perm() | 0o111
	if mode > 0o755 {
		mode = 0o755
	}

	// Write the new bytes to a sibling of dst so the subsequent
	// rename is on the same filesystem (cross-fs rename is
	// non-atomic on Linux and outright fails on some kernels).
	tmp := dst + ".upgrade.tmp"
	if err := copyFile(src, tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("upgrade: rename %s → %s: %w", tmp, dst, err)
	}
	// fsync the parent dir so the rename's directory entry hits
	// disk before this function returns. A power loss between
	// rename and the next sync would otherwise resurrect the old
	// binary on next boot.
	if dirFd, derr := os.Open(filepath.Dir(dst)); derr == nil {
		_ = dirFd.Sync()
		_ = dirFd.Close()
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("upgrade: open src %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("upgrade: open dst %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("upgrade: copy %s → %s: %w", src, dst, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("upgrade: sync %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("upgrade: close %s: %w", dst, err)
	}
	return nil
}
