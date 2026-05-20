//go:build linux

package supervisor

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The kernel 5.12 mount API offers an atomic alternative to the
// legacy three-syscall sequence (MS_BIND → MS_PRIVATE → MS_REMOUNT|
// MS_RDONLY) used by bindOneMount. Pentest review flagged three
// race / window bugs in the legacy path:
//
//   H1. MS_PRIVATE happens AFTER MS_BIND, so the new mount briefly
//       carries default shared propagation. A concurrent operation
//       cloning from the same parent can pick up the still-shared
//       peer and propagate.
//   H2. MS_REMOUNT|MS_RDONLY is observable as a transition from RW
//       to RO. Tenants who open(O_WRONLY) on the bind in the gap
//       retain a writable fd past the remount (CVE-class).
//   H3. NOSUID|NODEV are top-level only; sub-mounts of the source
//       inherit the underlying mount's flags (e.g., suid on /usr).
//
// The atomic path uses:
//   - open_tree(src_fd, "", OPEN_TREE_CLONE|OPEN_TREE_CLOEXEC|AT_RECURSIVE)
//     ⇒ detached mount tree, observable nowhere else
//   - mount_setattr(tree_fd, "", AT_RECURSIVE|AT_EMPTY_PATH, &attr)
//     ⇒ atomic set of MOUNT_ATTR_{RDONLY,NOSUID,NODEV,NOSYMFOLLOW}
//        plus MS_PRIVATE propagation, applied to entire subtree
//   - move_mount(tree_fd, "", dst_parent_fd, leaf, MOVE_MOUNT_F_EMPTY_PATH)
//     ⇒ atomic attach at the target; from the host's point of view
//        the bind appears with all flags already set
//
// Kernel ≥5.12 is required (mount_setattr was added in commit
// 2a1867219c7b). On older kernels, ENOSYS surfaces from
// SYS_MOUNT_SETATTR and the supervisor falls back to legacy.

// Syscall numbers — duplicated here because x/sys exposes the
// SYS_OPEN_TREE constants but no Go-level wrappers as of v0.35.
// Linux man page references: open_tree(2), mount_setattr(2),
// move_mount(2).

// atomicMountSupported caches the kernel's support for
// mount_setattr. Initialized lazily on first call; tri-state via
// atomic int32: 0=unknown, 1=supported, 2=unsupported.
var (
	atomicMountSupportOnce sync.Once
	atomicMountSupportFlag atomic.Int32
)

// atomicMountAvailable reports whether the kernel supports the
// open_tree/mount_setattr/move_mount path. Probe is performed once
// and cached; on first call we attempt the syscall with arguments
// that always fail (invalid fd) and look at whether the kernel
// rejected the SYSCALL NUMBER (ENOSYS) or the arguments (EBADF /
// EINVAL — meaning the syscall exists).
func atomicMountAvailable() bool {
	atomicMountSupportOnce.Do(func() {
		// Best probe: invoke mount_setattr with an obviously-bad
		// fd. If the kernel knows the syscall it returns EBADF;
		// if not, ENOSYS.
		var attr unix.MountAttr
		_, _, errno := unix.Syscall6(
			unix.SYS_MOUNT_SETATTR,
			uintptr(^uintptr(0)), // invalid fd
			0, 0,
			uintptr(unsafe.Pointer(&attr)),
			unsafe.Sizeof(attr), 0,
		)
		if errno == unix.ENOSYS {
			atomicMountSupportFlag.Store(2)
		} else {
			atomicMountSupportFlag.Store(1)
		}
	})
	return atomicMountSupportFlag.Load() == 1
}

// AT_RECURSIVE is the recursive-application flag for mount_setattr
// and open_tree. x/sys defines it for some arches but not as a
// portable constant — duplicated here so all builds resolve it.
const atRecursive = 0x8000

// openTree wraps the open_tree(2) syscall. Returns a fd referring
// to a detached (OPEN_TREE_CLONE) recursive (AT_RECURSIVE) clone of
// the mount at (dirfd, pathname).
func openTree(dirfd int, pathname string, flags uintptr) (int, error) {
	var p *byte
	if pathname != "" {
		var err error
		p, err = unix.BytePtrFromString(pathname)
		if err != nil {
			return -1, err
		}
	} else {
		// Empty string handling — pass nil ptr and rely on caller
		// to OR in AT_EMPTY_PATH if they want that semantic.
		empty := []byte{0}
		p = &empty[0]
	}
	r0, _, errno := unix.Syscall6(
		unix.SYS_OPEN_TREE,
		uintptr(dirfd),
		uintptr(unsafe.Pointer(p)),
		flags,
		0, 0, 0,
	)
	if errno != 0 {
		return -1, errno
	}
	return int(r0), nil
}

// mountSetattr wraps the mount_setattr(2) syscall.
func mountSetattr(dirfd int, pathname string, flags uintptr, attr *unix.MountAttr) error {
	var p *byte
	if pathname != "" {
		var err error
		p, err = unix.BytePtrFromString(pathname)
		if err != nil {
			return err
		}
	} else {
		empty := []byte{0}
		p = &empty[0]
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_MOUNT_SETATTR,
		uintptr(dirfd),
		uintptr(unsafe.Pointer(p)),
		flags,
		uintptr(unsafe.Pointer(attr)),
		unsafe.Sizeof(*attr),
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// moveMount wraps the move_mount(2) syscall.
func moveMount(fromDirfd int, fromPath string, toDirfd int, toPath string, flags uintptr) error {
	fp, err := unix.BytePtrFromString(fromPath)
	if err != nil {
		return err
	}
	tp, err := unix.BytePtrFromString(toPath)
	if err != nil {
		return err
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_MOVE_MOUNT,
		uintptr(fromDirfd),
		uintptr(unsafe.Pointer(fp)),
		uintptr(toDirfd),
		uintptr(unsafe.Pointer(tp)),
		flags,
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// AT_EMPTY_PATH allows the syscall to operate on the fd itself when
// pathname is empty.
const atEmptyPath = 0x1000

// bindAtomic performs the bind using the modern kernel mount API.
// Equivalent to the legacy MS_BIND → MS_PRIVATE → optional MS_REMOUNT
// sequence but with no observable intermediate state. Caller passes:
//
//   - srcFD: O_PATH fd of the source directory (already openat2-resolved)
//   - tgtParentFD: O_PATH fd of the target's parent directory
//   - tgtLeaf: leaf name inside the parent
//   - readOnly: whether MOUNT_ATTR_RDONLY is added to the atomic set
//
// Returns ENOSYS when the kernel doesn't support mount_setattr;
// the caller falls back to the legacy path. Other errors are
// returned verbatim.
func bindAtomic(srcFD, tgtParentFD int, tgtLeaf string, readOnly bool) error {
	// Stage 1: clone the source's mount into a detached tree fd.
	// OPEN_TREE_CLONE creates a brand-new mount object that lives
	// nowhere; subsequent setattr calls can't be observed by any
	// other process / namespace until move_mount attaches it.
	treeFD, err := openTree(srcFD, "",
		unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|atRecursive|atEmptyPath)
	if err != nil {
		return fmt.Errorf("open_tree: %w", err)
	}
	defer unix.Close(treeFD)

	// Stage 2: set the protective flags atomically across the
	// whole (possibly recursive) tree. AT_RECURSIVE applies to all
	// sub-mounts — fixes H3 (NOSUID on sub-mounts inherited).
	// Propagation=MS_PRIVATE fixes H1 (no shared-propagation
	// window). MOUNT_ATTR_RDONLY in the same call fixes H2 (no
	// observable RW gap).
	attrSet := uint64(unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV | unix.MOUNT_ATTR_NOSYMFOLLOW)
	if readOnly {
		attrSet |= unix.MOUNT_ATTR_RDONLY
	}
	attr := unix.MountAttr{
		Attr_set:    attrSet,
		Propagation: unix.MS_PRIVATE,
	}
	if err := mountSetattr(treeFD, "",
		atRecursive|atEmptyPath, &attr); err != nil {
		return fmt.Errorf("mount_setattr: %w", err)
	}

	// Stage 3: atomically attach the tree at the target. Until this
	// call returns, no other thread / process / namespace can see
	// the new mount.
	if err := moveMount(treeFD, "",
		tgtParentFD, tgtLeaf,
		unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount: %w", err)
	}

	return nil
}

// isENOSYS reports whether err wraps an ENOSYS errno. Used by the
// fallback path to distinguish "kernel doesn't support the new
// API" from real failures we shouldn't retry on.
func isENOSYS(err error) bool {
	var errno unix.Errno
	if errors.As(err, &errno) {
		return errno == unix.ENOSYS
	}
	return false
}
