package supervisor

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

var errChrootIsRoot = errors.New("supervisor: sandbox chroot must not be \"/\"")

// ErrVolumeMountUnsupported is returned by applyVolumeMounts on
// non-Linux hosts when the caller declares any VolumeMounts. The
// supervisor refuses to silently drop them — a stateful workload
// without its bind mounts would lose data on restart.
var ErrVolumeMountUnsupported = errors.New("supervisor: volume_mounts requires Linux + cgroup v2")

// resolvedMount is a VolumeMount after path resolution and
// validation. SourceFD is a borrowed reference to the Volume's
// pinned O_PATH fd (the underlying Linux mount code re-anchors via
// openat2 from it to apply SubPath); HostTarget is the cleaned
// path under chroot (when present) where the bind lands.
type resolvedMount struct {
	VolumeID   string
	SubPath    string // cleaned, possibly empty
	HostTarget string // absolute, chroot-prefixed when applicable
	ReadOnly   bool
}

// resolveVolumeMounts validates the per-app VolumeMounts against
// the supervisor's Volumes registry. Returns the resolved entries
// or the first error encountered. Pure function — no FS or mount
// syscalls — so it can run identically across platforms in unit
// tests and on non-Linux dev hosts.
//
// Rules:
//   - VolumeID must reference an existing Volume (caller must
//     RegisterVolume before Spawn).
//   - SubPath must be relative, no "..", no leading "/".
//   - Target must be absolute, no "..".
//   - chrootDir (when set) must be absolute; HostTarget becomes
//     <chrootDir>/<Target> and must stay inside chrootDir.
//   - Targets within one Config must be unique post-resolution.
//   - Per-mount ReadOnly can only TIGHTEN the Volume's RO setting,
//     never relax it: RW volume → RO mount is allowed; RO volume
//     → RW mount is rejected.
func (s *Supervisor) resolveVolumeMounts(mounts []VolumeMount, chrootDir string) ([]resolvedMount, error) {
	if len(mounts) == 0 {
		return nil, nil
	}

	cleanedChroot := ""
	if chrootDir != "" {
		if !filepath.IsAbs(chrootDir) {
			return nil, fmt.Errorf("supervisor: sandbox chroot %q must be absolute", chrootDir)
		}
		cleanedChroot = filepath.Clean(chrootDir)
		// Chroot="/" silently bypasses AllowedTargetPrefixes because
		// pathInside("/etc", "/") is always true — found in pentest
		// review (C5). A chroot at "/" is also semantically wrong:
		// the child sees the host root as its root, which is no
		// chroot at all.
		if cleanedChroot == "/" {
			return nil, errChrootIsRoot
		}
	}

	s.volumesMu.RLock()
	defer s.volumesMu.RUnlock()

	out := make([]resolvedMount, 0, len(mounts))
	seenTargets := make(map[string]struct{}, len(mounts))
	for i, m := range mounts {
		vol, ok := s.volumes[m.VolumeID]
		if !ok {
			return nil, fmt.Errorf("supervisor: volume_mounts[%d]: %q: %w", i, m.VolumeID, ErrVolumeNotFound)
		}

		if m.SubPath != "" {
			if filepath.IsAbs(m.SubPath) {
				return nil, fmt.Errorf("supervisor: volume_mounts[%d]: sub_path %q must be relative", i, m.SubPath)
			}
			if strings.HasPrefix(m.SubPath, "/") {
				return nil, fmt.Errorf("supervisor: volume_mounts[%d]: sub_path %q must not start with '/'", i, m.SubPath)
			}
			if containsDotDot(m.SubPath) {
				return nil, fmt.Errorf("supervisor: volume_mounts[%d]: sub_path %q contains '..'", i, m.SubPath)
			}
		}

		if m.Target == "" {
			return nil, fmt.Errorf("supervisor: volume_mounts[%d]: empty target", i)
		}
		if !filepath.IsAbs(m.Target) {
			return nil, fmt.Errorf("supervisor: volume_mounts[%d]: target %q must be absolute", i, m.Target)
		}
		if containsDotDot(m.Target) {
			return nil, fmt.Errorf("supervisor: volume_mounts[%d]: target %q contains '..'", i, m.Target)
		}

		// RO promotion only: a RW volume can be projected RO for
		// this app; the reverse would silently weaken the Volume's
		// declared intent.
		effectiveRO := vol.ReadOnly || m.ReadOnly
		if vol.ReadOnly && !m.ReadOnly {
			// Defensive: explicit projection ReadOnly=false on an
			// RO volume is a likely orchestrator bug. Allow but
			// force RO; caller intent (ReadOnly: false) is ignored
			// in favor of volume's RO. We don't error here because
			// the JSON default for bool is false — users who don't
			// set ReadOnly shouldn't see a hard fail. But we LOG.
			s.logger.Warn("volume_mount ignored projection ReadOnly=false on read-only volume",
				"volume_id", m.VolumeID, "target", m.Target,
			)
		}

		tgtClean := filepath.Clean(m.Target)
		hostTarget := tgtClean
		if cleanedChroot != "" {
			hostTarget = filepath.Clean(filepath.Join(cleanedChroot, tgtClean))
			if !pathInside(hostTarget, cleanedChroot) {
				return nil, fmt.Errorf("supervisor: volume_mounts[%d]: target %q escapes chroot %q", i, m.Target, cleanedChroot)
			}
		} else {
			// No chroot ⇒ Target lands on the host filesystem
			// directly. Without an allowlist guard, an orchestrator
			// could pass Target: "/etc" and overlay system files.
			// Reject anything not under AllowedTargetPrefixes.
			if !targetUnderAllowlist(hostTarget, s.AllowedTargetPrefixes) {
				return nil, fmt.Errorf("supervisor: volume_mounts[%d]: target %q not under any allowed prefix; configure Supervisor.AllowedTargetPrefixes or set Sandbox.Chroot", i, hostTarget)
			}
		}
		if _, dup := seenTargets[hostTarget]; dup {
			return nil, fmt.Errorf("supervisor: volume_mounts[%d]: duplicate target %q", i, hostTarget)
		}
		seenTargets[hostTarget] = struct{}{}

		out = append(out, resolvedMount{
			VolumeID:   m.VolumeID,
			SubPath:    filepath.Clean(m.SubPath),
			HostTarget: hostTarget,
			ReadOnly:   effectiveRO,
		})
	}
	return out, nil
}

// targetUnderAllowlist reports whether hostTarget is under any
// configured allowed prefix AND is not inside any forbidden system
// directory. Empty allowlist ⇒ everything denied. The forbidden
// set is enforced UNCONDITIONALLY — operator misconfig (e.g.
// AllowedTargetPrefixes=["/"]) cannot unblock these.
//
// Pentest review (H7): operators who write a too-broad allowlist
// should still not be able to overlay /etc, /proc, etc. The
// supervisor refuses to mount over the host base system regardless
// of allowlist contents.
func targetUnderAllowlist(hostTarget string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return false
	}
	cleaned := filepath.Clean(hostTarget)
	if isForbiddenHostPath(cleaned) {
		return false
	}
	for _, prefix := range allowlist {
		if !filepath.IsAbs(prefix) {
			continue
		}
		cleanedPrefix := filepath.Clean(prefix)
		// A "/"-prefix allowlist entry would match everything;
		// rejected unconditionally because it negates the whole
		// allowlist mechanism. Operator must list real prefixes.
		if cleanedPrefix == "/" {
			continue
		}
		if pathInside(cleaned, cleanedPrefix) {
			return true
		}
	}
	return false
}

// forbiddenTargetPrefixes lists host directories that VolumeMount
// Target must never land under, regardless of AllowedTargetPrefixes.
// These are the standard system locations whose tenant overlay
// either breaks the host (creekd itself, libc) or is a classic
// privilege-escalation pivot (/etc/sudoers, /proc, /sys, /dev,
// /boot, /root).
var forbiddenTargetPrefixes = []string{
	"/proc", "/sys", "/dev", "/etc", "/usr", "/bin", "/sbin",
	"/lib", "/lib64", "/lib32", "/boot", "/root",
}

// isForbiddenHostPath reports whether path is at or beneath any
// forbidden host prefix. Caller passes a cleaned absolute path.
func isForbiddenHostPath(cleaned string) bool {
	for _, forbidden := range forbiddenTargetPrefixes {
		if pathInside(cleaned, forbidden) {
			return true
		}
	}
	return false
}

// pathInside reports whether child is at or below parent after
// cleaning both. Used as the chroot containment check.
func pathInside(child, parent string) bool {
	if parent == "" {
		return true
	}
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
