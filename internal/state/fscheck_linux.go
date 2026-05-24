//go:build linux

package state

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Filesystem magic constants from Linux uapi/linux/magic.h. Sourced
// 2026-05-24; constants are stable across kernel releases (each fs
// pins its own number forever per the kernel ABI contract).
const (
	fsMagicEXT4  = 0xEF53     // also ext2/ext3 — same superblock magic
	fsMagicXFS   = 0x58465342 // 'XFSB'
	fsMagicTMPFS = 0x01021994
	fsMagicZFS   = 0x2FC12FC1
	fsMagicBTRFS = 0x9123683E
)

// fsNameByMagic translates the filesystem magic constant returned by
// statfs(2) into a human-readable name for error messages.
func fsNameByMagic(magic int64) string {
	switch magic {
	case fsMagicEXT4:
		return "ext4 / ext3 / ext2"
	case fsMagicXFS:
		return "xfs"
	case fsMagicTMPFS:
		return "tmpfs"
	case fsMagicZFS:
		return "zfs"
	case fsMagicBTRFS:
		return "btrfs"
	default:
		return fmt.Sprintf("unknown (magic=0x%x)", magic)
	}
}

// checkFilesystem inspects the filesystem hosting the state dir.
// On Linux, ext4/ext3/ext2 and xfs are accepted (rename(2) atomic +
// fsync(dir) durable). zfs and btrfs are explicitly rejected per
// DESIGN-self-host-state.md §"Filesystem requirement (Phase 1)" —
// zfs's fsync(dir) is a no-op due to its txg model; btrfs nodatacow
// weakens rename atomicity. tmpfs is rejected outright (no
// durability).
//
// The check runs at NewStore time; if state.json lives on an
// unsupported filesystem the daemon refuses to boot with
// `unsupported_filesystem` so operators see the constraint loudly
// rather than discovering it through silent data loss.
func checkFilesystem(statePath string) error {
	dir := filepath.Dir(statePath)
	// MkdirAll first so statfs has something real to inspect even on
	// fresh installs (NewStore is allowed to be called before any
	// AddApp).
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		// Directory doesn't exist yet — statfs the deepest existing
		// ancestor. /var/lib/creekd doesn't exist? walk up to /var,
		// then /, both of which always exist on a Linux host.
		probe := dir
		for {
			parent := filepath.Dir(probe)
			if parent == probe {
				return fmt.Errorf("state: statfs %s: %w", dir, err)
			}
			probe = parent
			if err2 := unix.Statfs(probe, &st); err2 == nil {
				break
			}
		}
	}
	magic := int64(st.Type)
	switch magic {
	case fsMagicEXT4, fsMagicXFS:
		return nil
	case fsMagicTMPFS, fsMagicZFS, fsMagicBTRFS:
		return &UnsupportedFilesystemError{
			Path:       dir,
			Detected:   fsNameByMagic(magic),
			MagicValue: magic,
		}
	default:
		return &UnsupportedFilesystemError{
			Path:       dir,
			Detected:   fsNameByMagic(magic),
			MagicValue: magic,
		}
	}
}
