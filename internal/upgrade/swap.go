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
// Mode follows src but is force-executable (0o111 OR'd in) and
// capped at 0o755. Every release artifact must be runnable; we
// never grant more than rwxr-xr-x. Note: this preserves src's
// read/write bits — a src created with a restrictive umask
// (e.g. 0600) becomes 0711, not 0755.
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

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("upgrade: open src %s: %w", src, err)
	}
	defer in.Close()

	// os.CreateTemp: same-filesystem sibling (for an atomic rename),
	// randomised name (no symlink-clobber race on a predictable
	// path), and O_EXCL semantics (we never follow a pre-existing
	// file at the chosen path).
	out, err := os.CreateTemp(filepath.Dir(dst), ".upgrade-*.tmp")
	if err != nil {
		return fmt.Errorf("upgrade: create tmp in %s: %w", filepath.Dir(dst), err)
	}
	tmp := out.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmp)
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("upgrade: copy %s → %s: %w", src, tmp, err)
	}
	if err := out.Chmod(mode); err != nil {
		_ = out.Close()
		return fmt.Errorf("upgrade: chmod %s: %w", tmp, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("upgrade: sync %s: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("upgrade: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("upgrade: rename %s → %s: %w", tmp, dst, err)
	}
	committed = true
	// Best-effort: the rename already succeeded, so fsync errors
	// here are not fatal to the swap.
	if dirFd, derr := os.Open(filepath.Dir(dst)); derr == nil {
		_ = dirFd.Sync()
		_ = dirFd.Close()
	}
	return nil
}

// CopyFile duplicates src to dst. Tries a hard link first (O(1),
// shared inode, no extra disk space); on EXDEV / EPERM / FS that
// doesn't support hard links, falls back to a byte copy.
// Callers depending on dst being a DISTINCT inode from src must
// not use this — use SwapBinary for the atomic-swap primitive
// that always allocates a fresh inode.
func CopyFile(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
