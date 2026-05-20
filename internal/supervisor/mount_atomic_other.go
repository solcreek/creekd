//go:build !linux

package supervisor

// atomicMountAvailable on non-Linux is always false — there is no
// open_tree/mount_setattr/move_mount equivalent. Callers fall back
// to whatever the platform supports (ErrVolumeMountUnsupported in
// our case).
func atomicMountAvailable() bool { return false }
