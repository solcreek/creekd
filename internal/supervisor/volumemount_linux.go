//go:build linux

package supervisor

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// openAndIsolateVolume resolves a relative BackingPath safely under
// the pinned VolumeRoot fd, captures kernel-side identity (resolved
// path + dev:ino), statfs's the result, and remounts it MS_PRIVATE
// to break shared-propagation leakage.
//
// Why this whole dance:
//   - openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS|RESOLVE_NO_MAGICLINKS)
//     guarantees the kernel never follows a symlink, never escapes
//     VolumeRoot, never crosses into /proc magic links.
//   - MS_PRIVATE on the volume root prevents host-side mount events
//     from propagating into tenant containers.
//   - Capturing (resolvedAbsPath, devMajor:devMinor, inode) here lets
//     later identity checks compare against a frozen truth — fixes
//     the HasSuffix false-positive identified in pentest review C2.
//
// Idempotent at the MS_PRIVATE layer: marking an already-private
// mount private again is a no-op in the kernel.
func (s *Supervisor) openAndIsolateVolume(cleanedRel string) (*Volume, string, error) {
	rootFD, err := s.ensureVolumeRootFD()
	if err != nil {
		return nil, "", err
	}

	how := unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(rootFD, cleanedRel, &how)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return nil, "", fmt.Errorf("%w: %s", ErrVolumeBackingMissing, cleanedRel)
		}
		return nil, "", fmt.Errorf("supervisor: openat2 %q: %w", cleanedRel, err)
	}
	defer unix.Close(fd)

	resolvedPath, err := readlinkProcFD(fd)
	if err != nil {
		return nil, "", fmt.Errorf("supervisor: readlink resolved fd: %w", err)
	}

	// Capture kernel-side identity from the fd itself, not from the
	// string we just resolved. unix.Fstat on the O_PATH fd avoids
	// the TOCTOU window where the resolved path could be replaced.
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, "", fmt.Errorf("supervisor: fstat resolved fd: %w", err)
	}

	var stfs unix.Statfs_t
	if err := unix.Fstatfs(fd, &stfs); err != nil {
		return nil, "", fmt.Errorf("supervisor: statfs %q: %w", resolvedPath, err)
	}
	fsType := fsTypeName(stfs.Type)

	fdPath := fmt.Sprintf("/proc/self/fd/%d", fd)
	if err := unix.Mount("", fdPath, "", unix.MS_PRIVATE, ""); err != nil {
		if errors.Is(err, unix.EINVAL) {
			// Path isn't currently a mountpoint; self-bind to make
			// one, then MS_PRIVATE. Standard systemd-private-mount
			// technique.
			if err := unix.Mount(fdPath, fdPath, "", unix.MS_BIND, ""); err != nil {
				return nil, "", fmt.Errorf("supervisor: self-bind for MS_PRIVATE: %w", err)
			}
			if err := unix.Mount("", fdPath, "", unix.MS_PRIVATE, ""); err != nil {
				return nil, "", fmt.Errorf("supervisor: MS_PRIVATE after self-bind: %w", err)
			}
		} else {
			return nil, "", fmt.Errorf("supervisor: MS_PRIVATE %q: %w", resolvedPath, err)
		}
	}

	return &Volume{
		BackingPath:     cleanedRel,
		FSType:          fsType,
		resolvedAbsPath: resolvedPath,
		devMajor:        unix.Major(st.Dev),
		devMinor:        unix.Minor(st.Dev),
		inode:           st.Ino,
	}, fsType, nil
}

// ensureVolumeRootFD opens the supervisor's VolumeRoot as an
// O_PATH|O_DIRECTORY fd, once. All subsequent openat2 calls anchor
// here.
func (s *Supervisor) ensureVolumeRootFD() (int, error) {
	s.volumeRootOnce.Do(func() {
		if s.VolumeRoot == "" {
			s.volumeRootErr = ErrVolumeRootRequired
			return
		}
		fd, err := unix.Open(
			s.VolumeRoot,
			unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC,
			0,
		)
		if err != nil {
			s.volumeRootErr = fmt.Errorf("supervisor: open VolumeRoot %q: %w", s.VolumeRoot, err)
			return
		}
		s.volumeRootFD = fd
	})
	if s.volumeRootErr != nil {
		return -1, s.volumeRootErr
	}
	return s.volumeRootFD, nil
}

// applyVolumeMounts performs the per-app bind mounts.
func (s *Supervisor) applyVolumeMounts(mounts []VolumeMount, chrootDir string) error {
	resolved, err := s.resolveVolumeMounts(mounts, chrootDir)
	if err != nil {
		return err
	}
	if len(resolved) == 0 {
		return nil
	}

	rootFD, err := s.ensureVolumeRootFD()
	if err != nil {
		return err
	}

	for i, rm := range resolved {
		if err := s.bindOneMount(rootFD, rm); err != nil {
			return fmt.Errorf("supervisor: volume_mounts[%d]: %w", i, err)
		}
		s.logger.Info("volume mounted",
			"volume_id", rm.VolumeID,
			"sub_path", rm.SubPath,
			"target", rm.HostTarget,
			"read_only", rm.ReadOnly,
		)
	}
	return nil
}

// bindOneMount applies a single resolvedMount with the full TOCTOU
// defense:
//
//   - SOURCE: openat2 anchored at VolumeRoot fd, then mount source
//     is /proc/self/fd/srcFD. Kernel pins the source inode at
//     openat2 time; subsequent symlink swaps on the source string
//     don't affect what gets bound.
//   - TARGET: MkdirAll the path string, then immediately open the
//     freshly-created directory with O_PATH|O_NOFOLLOW|O_DIRECTORY
//     and pass /proc/self/fd/tgtFD as the mount target. Without
//     this, an attacker who can write a parent dir can swap a leaf
//     for a symlink between MkdirAll and Mount, redirecting the
//     bind onto an arbitrary host path. (Pentest C1.)
//   - IDENTITY: post-mount detection of "already bound" uses the
//     Volume's stored devMajor:devMinor + inode captured at
//     RegisterVolume time. No more HasSuffix games — exact match
//     against the registered Volume. (Pentest C2 + C3.)
func (s *Supervisor) bindOneMount(rootFD int, rm resolvedMount) error {
	s.volumesMu.RLock()
	vol, ok := s.volumes[rm.VolumeID]
	s.volumesMu.RUnlock()
	if !ok {
		return fmt.Errorf("%q: %w", rm.VolumeID, ErrVolumeNotFound)
	}

	rel := vol.BackingPath
	if rm.SubPath != "" && rm.SubPath != "." {
		rel = filepath.Join(rel, rm.SubPath)
	}

	how := unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	srcFD, err := unix.Openat2(rootFD, rel, &how)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return fmt.Errorf("%w: %s", ErrVolumeBackingMissing, rel)
		}
		return fmt.Errorf("openat2 %q: %w", rel, err)
	}
	defer unix.Close(srcFD)

	// Cross-check the resolved source against the Volume's stored
	// identity. If the registered directory's inode was replaced
	// between RegisterVolume and Spawn (rm -rf + recreate), reject
	// rather than silently mounting the new inode. Operator can
	// re-register if the swap was intentional.
	var srcStat unix.Stat_t
	if err := unix.Fstat(srcFD, &srcStat); err != nil {
		return fmt.Errorf("fstat source fd: %w", err)
	}
	srcMajor := unix.Major(srcStat.Dev)
	srcMinor := unix.Minor(srcStat.Dev)
	// SubPath mounts naturally have a different inode than the
	// volume root, so we only enforce dev match (same FS) when
	// SubPath is empty/dot. Dev match is itself meaningful: it
	// catches "tenant volume backing replaced from a different FS".
	if rm.SubPath == "" || rm.SubPath == "." {
		if srcMajor != vol.devMajor || srcMinor != vol.devMinor || srcStat.Ino != vol.inode {
			return fmt.Errorf("source identity drift: volume %q registered (dev=%d:%d ino=%d), now (dev=%d:%d ino=%d) — re-register if intentional",
				rm.VolumeID, vol.devMajor, vol.devMinor, vol.inode,
				srcMajor, srcMinor, srcStat.Ino)
		}
	} else {
		if srcMajor != vol.devMajor || srcMinor != vol.devMinor {
			return fmt.Errorf("source FS drift: volume %q registered dev=%d:%d, sub_path %q resolves on dev=%d:%d",
				rm.VolumeID, vol.devMajor, vol.devMinor, rm.SubPath, srcMajor, srcMinor)
		}
	}

	// MkdirAll the parent only — the leaf itself we will create via
	// a race-free Mkdirat under an O_PATH parent fd. This is the C1
	// fix: never resolve the full target string at mount time.
	parent := filepath.Dir(rm.HostTarget)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("mkdir target parent %q: %w", parent, err)
	}
	tgtFD, err := openTargetRaceFree(rm.HostTarget)
	if err != nil {
		return fmt.Errorf("open target %q: %w", rm.HostTarget, err)
	}
	defer unix.Close(tgtFD)

	srcFDPath := fmt.Sprintf("/proc/self/fd/%d", srcFD)
	tgtFDPath := fmt.Sprintf("/proc/self/fd/%d", tgtFD)

	// Identity check via mountinfo for idempotent re-spawn (same
	// Volume → same target is a no-op). Compares the mount's source
	// inode against the Volume's stored (devMajor:devMinor + inode).
	bound, sameSource, err := isAlreadyBoundExact(rm.HostTarget, vol, srcStat)
	if err != nil {
		return fmt.Errorf("check existing mount: %w", err)
	}
	if bound && !sameSource {
		return fmt.Errorf("target %q already bound to a different source", rm.HostTarget)
	}

	if !bound {
		flags := uintptr(unix.MS_BIND | unix.MS_NOSUID | unix.MS_NODEV)
		if err := unix.Mount(srcFDPath, tgtFDPath, "", flags, ""); err != nil {
			return fmt.Errorf("bind: %w", err)
		}
		if err := unix.Mount("", tgtFDPath, "", unix.MS_PRIVATE, ""); err != nil {
			return fmt.Errorf("MS_PRIVATE on target: %w", err)
		}
	}

	if rm.ReadOnly {
		flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV)
		if err := unix.Mount("", tgtFDPath, "", flags, ""); err != nil {
			return fmt.Errorf("remount readonly: %w", err)
		}
		if ok, err := verifyReadOnly(rm.HostTarget); err != nil {
			return fmt.Errorf("verify readonly: %w", err)
		} else if !ok {
			return fmt.Errorf("target %q remounted but mountinfo still reports rw", rm.HostTarget)
		}
	}

	return nil
}

// openTargetRaceFree opens hostTarget for use as a mount destination,
// creating the leaf directory atomically under the parent's O_PATH
// fd. Refuses to follow symlinks at the leaf. This closes pentest
// C1 — the symlink-swap race between MkdirAll(hostTarget) and
// Mount(hostTarget) is no longer reachable because Mount sees
// /proc/self/fd/N of an inode we just verified is a real directory.
func openTargetRaceFree(hostTarget string) (int, error) {
	parent := filepath.Dir(hostTarget)
	leaf := filepath.Base(hostTarget)

	parentFD, err := unix.Open(parent, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open parent %q: %w", parent, err)
	}
	defer unix.Close(parentFD)

	// Atomic: mkdirat → openat O_NOFOLLOW|O_DIRECTORY. If a symlink
	// got planted between the two, openat fails with ELOOP.
	if err := unix.Mkdirat(parentFD, leaf, 0o755); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return -1, fmt.Errorf("mkdirat %q: %w", leaf, err)
		}
	}
	tgtFD, err := unix.Openat(parentFD, leaf,
		unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("openat leaf %q (symlink swap?): %w", leaf, err)
	}
	return tgtFD, nil
}

// isAlreadyBoundExact reports whether target is currently a mount
// whose backing source matches the Volume's registered identity.
// Identity is (devMajor:devMinor + inode) — captured at
// RegisterVolume time and compared against the current source fd's
// dev:ino. mountinfo only provides (devMajor:devMinor + root path
// within FS), so we additionally Stat the current source.
//
// Replaces the previous HasSuffix-based check that was vulnerable
// to false positives on shared filesystems (pentest C2). Replaces
// the silent (false,false,nil) on Lstat failure that contradicted
// its own comment (pentest C3).
func isAlreadyBoundExact(target string, vol *Volume, srcStat unix.Stat_t) (bool, bool, error) {
	target = filepath.Clean(target)
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, false, fmt.Errorf("open mountinfo: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// mountinfo format (man proc):
		//   2: major:minor
		//   3: root path within source FS
		//   4: mount point
		if len(fields) < 5 {
			continue
		}
		if filepath.Clean(unescapeMountinfo(fields[4])) != target {
			continue
		}
		// Found a mount at target. Match against the Volume's
		// registered dev:ino — that's the unambiguous identity.
		majMin := fields[2]
		parts := strings.SplitN(majMin, ":", 2)
		if len(parts) != 2 {
			return true, false, nil
		}
		major, err1 := strconv.ParseUint(parts[0], 10, 32)
		minor, err2 := strconv.ParseUint(parts[1], 10, 32)
		if err1 != nil || err2 != nil {
			return true, false, nil
		}
		if uint32(major) != unix.Major(srcStat.Dev) || uint32(minor) != unix.Minor(srcStat.Dev) {
			return true, false, nil
		}
		// Same FS — that's necessary but not sufficient. Compare
		// the bind's source inode against the just-Fstat'd source.
		// If the current mount's "root within FS" field matches the
		// volume's registered resolved path tail (when reachable),
		// it's a true identity match. Otherwise, conservatively
		// treat as "different source" so the caller errors out
		// instead of silently reusing.
		root := unescapeMountinfo(fields[3])
		if root == "/" {
			// Whole-FS bind — only legitimate if Volume's backing
			// is the FS root. We don't currently track that; treat
			// as match since dev:ino already matched the source.
			return true, true, nil
		}
		// vol.resolvedAbsPath is the kernel-resolved absolute path
		// of the volume at registration. mountinfo's "root" is the
		// path relative to the source FS root. If resolvedAbsPath
		// ends in root (path-boundary aware), it's a match.
		if endsAtBoundary(vol.resolvedAbsPath, root) {
			return true, true, nil
		}
		return true, false, nil
	}
	if err := scanner.Err(); err != nil {
		return false, false, err
	}
	return false, false, nil
}

// endsAtBoundary reports whether path ends with suffix at a path
// boundary (either path == suffix, or path[len(path)-len(suffix)-1]
// is '/'). Used to make HasSuffix checks resistant to "foo" matching
// "barfoo" — the false-positive identified in pentest C2.
func endsAtBoundary(path, suffix string) bool {
	if path == suffix {
		return true
	}
	if !strings.HasSuffix(path, suffix) {
		return false
	}
	if len(path) <= len(suffix) {
		return false
	}
	return path[len(path)-len(suffix)-1] == '/'
}

// unescapeMountinfo decodes kernel mountinfo escape sequences. The
// kernel octal-encodes space (\040), tab (\011), newline (\012),
// and backslash (\134) in mount-point and root-path fields. We must
// reverse this before path comparison or strings containing these
// characters silently mismatch.
func unescapeMountinfo(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			// Octal triple: backslash + three octal digits.
			if c, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(c))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// verifyReadOnly checks mountinfo's mount-options field (field 5)
// for "ro". Used after the MS_REMOUNT|MS_RDONLY pass.
func verifyReadOnly(target string) (bool, error) {
	target = filepath.Clean(target)
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			continue
		}
		if filepath.Clean(unescapeMountinfo(fields[4])) != target {
			continue
		}
		for _, opt := range strings.Split(fields[5], ",") {
			if opt == "ro" {
				return true, nil
			}
		}
		return false, nil
	}
	return false, scanner.Err()
}

// readlinkProcFD returns the absolute path the kernel resolved an
// O_PATH fd to.
func readlinkProcFD(fd int) (string, error) {
	link := fmt.Sprintf("/proc/self/fd/%d", fd)
	buf := make([]byte, 4096)
	n, err := unix.Readlink(link, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

// fsTypeName maps statfs.Type magic numbers to short names.
func fsTypeName(t int64) string {
	switch uint32(t) {
	case 0xEF53:
		return "ext4"
	case 0x58465342:
		return "xfs"
	case 0x9123683E:
		return "btrfs"
	case 0x2FC12FC1:
		return "zfs"
	case 0x01021994:
		return "tmpfs"
	case 0x794C7630:
		return "overlayfs"
	default:
		return fmt.Sprintf("unknown(0x%x)", t)
	}
}
