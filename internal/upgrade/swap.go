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

	// Use os.CreateTemp in the destination directory so:
	//   - the rename is on the same filesystem (cross-fs rename
	//     is non-atomic on Linux and outright fails on some kernels),
	//   - the tmp filename is randomised (avoids a symlink-clobber
	//     race on a predictable sibling name),
	//   - opening uses O_EXCL semantics so we never follow a
	//     pre-existing file at the chosen path.
	tmpFile, err := os.CreateTemp(filepath.Dir(dst), ".upgrade-*.tmp")
	if err != nil {
		return fmt.Errorf("upgrade: create tmp in %s: %w", filepath.Dir(dst), err)
	}
	tmp := tmpFile.Name()
	tmpFile.Close() // we'll reopen via copyFile with the right flags + mode
	if err := copyFile(src, tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("upgrade: rename %s → %s: %w", tmp, dst, err)
	}
	// Best-effort fsync of the parent directory so the rename's
	// entry hits disk before we return. Errors here are NOT fatal:
	// the rename already succeeded, the binary on disk is correct,
	// and failing the call would mislead callers into thinking the
	// swap itself failed. A power loss in the millisecond before
	// the kernel flushes is rare enough that best-effort matches
	// what every other long-lived Unix tool does here.
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
	out, err := os.OpenFile(dst, os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("upgrade: open dst %s: %w", dst, err)
	}
	if err := out.Chmod(mode); err != nil {
		_ = out.Close()
		return fmt.Errorf("upgrade: chmod %s: %w", dst, err)
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
