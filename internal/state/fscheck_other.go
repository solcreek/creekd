//go:build !linux

package state

// checkFilesystem is a no-op on non-Linux platforms. macOS dev
// machines use APFS, which doesn't share the rename + fsync(dir)
// guarantees Linux ext4/xfs provide, but creekd's only production
// target is Linux — the Mac path is for dev iteration only and the
// no-op is intentional (warning would be noise).
//
// Per DESIGN-self-host-state.md §"Filesystem requirement (Phase 1)"
// the check is Linux-only by design.
func checkFilesystem(_ string) error {
	return nil
}
