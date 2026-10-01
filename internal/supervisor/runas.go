package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RunAs is the host UID/GID an app's process runs as. The supervisor
// switches to it with `setpriv --reuid --regid --clear-groups` right
// before exec'ing the app (see sandbox.WrapSetpriv), after every
// namespace, cgroup and netns step that needs root has happened.
type RunAs struct {
	UID int
	GID int
}

// DefaultAppUIDBase is the first host UID the supervisor hands out when
// an app does not name one. It sits above the conventional
// /etc/subuid ranges (100000 + 65536 per user) and every regular or
// dynamic user range systemd reserves below 524288.
const DefaultAppUIDBase = 1_000_000

// ErrRunAsInUse is returned when an explicit RunAs names a UID another
// app already runs as. Two apps sharing a UID can read each other's
// /proc/<pid>/environ and files, which is exactly what per-app
// identities exist to prevent.
var ErrRunAsInUse = errors.New("run_as uid already used by another app")

// autoRunAsEnabled reports whether apps that name no RunAs get a
// dedicated UID: on Linux, when creekd can switch UIDs at all
// (CAP_SETUID + CAP_SETGID — root has them; the shipped systemd unit
// grants them to the creekd user), unless a test harness disabled the
// default sandbox or the operator set AppUIDBase to 0.
//
// It deliberately does not require CAP_SYS_ADMIN like the namespace
// defaults do: a non-root creekd without per-app UIDs runs every app as
// its own UID, and a process can read /proc/<pid>/environ of any
// process with the same UID — every other app's secrets, and creekd's
// own admin token.
func (s *Supervisor) autoRunAsEnabled() bool {
	return !s.DisableDefaultSandbox && s.AppUIDBase > 0 && s.PerAppUIDsAvailable()
}

// PerAppUIDsAvailable reports whether this creekd can run apps under
// their own UIDs: Linux with CAP_SETUID and CAP_SETGID effective.
// creekd warns at startup when it cannot.
func (s *Supervisor) PerAppUIDsAvailable() bool {
	return runtimeIsLinux() && hasSetIDCaps()
}

// hasSetIDCaps reports whether CAP_SETUID and CAP_SETGID are both in
// the effective set. Overridable for tests.
var hasSetIDCaps = readSetIDCapsFromProc

const (
	capKillBit   = 5 // CAP_KILL, <linux/capability.h>
	capSetgidBit = 6 // CAP_SETGID
	capSetuidBit = 7 // CAP_SETUID
)

// CanSignalApps reports whether creekd can signal an app that runs as
// another UID: root, or CAP_KILL. Without it, stopping or restarting a
// per-app UID app fails (the app stays running and registered), and a
// deploy promotes the new version while the old one keeps running
// unsupervised. creekd reports this at startup.
func (s *Supervisor) CanSignalApps() bool {
	return !runtimeIsLinux() || hasKillCap()
}

// hasKillCap reports whether CAP_KILL is effective. Overridable for tests.
var hasKillCap = func() bool {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	return parseCapBits(data, 1<<capKillBit)
}

func readSetIDCapsFromProc() bool {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	return parseSetIDCaps(data)
}

// parseSetIDCaps is the /proc/self/status parser behind hasSetIDCaps:
// true when CapEff has both CAP_SETGID and CAP_SETUID. Malformed or
// missing CapEff is false — no identity switch rather than a failing one.
func parseSetIDCaps(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "CapEff:")
		if !ok {
			continue
		}
		caps, err := strconv.ParseUint(strings.TrimSpace(rest), 16, 64)
		if err != nil {
			return false
		}
		want := uint64(1)<<capSetgidBit | uint64(1)<<capSetuidBit
		return caps&want == want
	}
	return false
}

// parseCapBits reports whether every bit in want is set in CapEff.
func parseCapBits(data []byte, want uint64) bool {
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "CapEff:")
		if !ok {
			continue
		}
		caps, err := strconv.ParseUint(strings.TrimSpace(rest), 16, 64)
		return err == nil && caps&want == want
	}
	return false
}

// resolveRunAsLocked settles cfg.RunAs before the app is created:
//
//   - RunAsRoot: no identity switch (the pre-per-app-UID behaviour).
//   - An explicit RunAs: validated (non-root, not used by another app).
//   - Otherwise, when auto RunAs is enabled: the next UID above the
//     high-water mark, persisted before it is handed out, so a UID is
//     never reused — a new app must not inherit files a deleted app
//     left behind.
//
// Caller holds s.mu.
func (s *Supervisor) resolveRunAsLocked(cfg *Config) error {
	if cfg.RunAsRoot {
		cfg.RunAs = nil
		return nil
	}
	if cfg.RunAs != nil {
		if !runtimeIsLinux() {
			return errors.New("supervisor: run_as needs Linux (setpriv)")
		}
		if !hasSetIDCaps() {
			return errors.New("supervisor: run_as needs CAP_SETUID and CAP_SETGID; creekd has neither root nor those capabilities")
		}
		if cfg.RunAs.UID <= 0 || cfg.RunAs.GID <= 0 {
			return errors.New("supervisor: run_as uid and gid must be > 0; use run_as_root to run as root")
		}
		if owner := s.runAsOwnerLocked(cfg.RunAs.UID, cfg.ID); owner != "" {
			return fmt.Errorf("supervisor: uid %d is used by app %q: %w", cfg.RunAs.UID, owner, ErrRunAsInUse)
		}
		// A pinned UID inside the allocation range is persisted like an
		// allocated one: otherwise, once its app is deleted and creekd
		// restarts, the file's older mark would let the UID be handed
		// to a new app, which would inherit the old one's files.
		if s.observeUIDLocked(cfg.RunAs.UID) {
			if err := s.persistUIDHighWater(s.uidHWM); err != nil {
				return fmt.Errorf("supervisor: persist uid high-water mark: %w", err)
			}
		}
		return nil
	}
	if !s.autoRunAsEnabled() {
		return nil
	}
	// A user-namespaced app's identity is its UID mapping: a host UID
	// from the allocator is not mapped inside the namespace, and
	// setresuid to it fails with EINVAL. Such apps get an identity only
	// through an explicit RunAs the operator has mapped.
	if cfg.Sandbox != nil && cfg.Sandbox.UserNamespace {
		return nil
	}
	next := s.AppUIDBase
	if s.uidHWM >= next {
		next = s.uidHWM + 1
	}
	for s.runAsOwnerLocked(next, cfg.ID) != "" {
		next++
	}
	if err := s.persistUIDHighWater(next); err != nil {
		return fmt.Errorf("supervisor: persist uid high-water mark: %w", err)
	}
	s.uidHWM = next
	cfg.RunAs = &RunAs{UID: next, GID: next}
	return nil
}

// runAsOwnerLocked returns the ID of a registered app, other than id
// itself or its blue-green counterpart, that runs as uid; "" if none.
// During a deploy v1 (id) and v2 (deployTempID(id)) legitimately share
// one UID. Caller holds s.mu.
func (s *Supervisor) runAsOwnerLocked(uid int, id string) string {
	base := strings.TrimSuffix(id, deployTempSuffix)
	for otherID, app := range s.apps {
		if strings.TrimSuffix(otherID, deployTempSuffix) == base {
			continue
		}
		if app.runAs != nil && app.runAs.UID == uid {
			return otherID
		}
	}
	return ""
}

// ObserveRunAs raises the high-water mark to a UID recorded in
// persisted state, before any app is restored. Without it, a lost
// high-water file plus a legacy entry restored first could be handed a
// UID that a later-restored app already owns on disk.
func (s *Supervisor) ObserveRunAs(cfg Config) {
	if cfg.RunAs == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observeUIDLocked(cfg.RunAs.UID)
}

// observeUIDLocked raises the in-memory mark to uid when uid is in the
// allocation range and above it; reports whether it did. Caller holds
// s.mu.
func (s *Supervisor) observeUIDLocked(uid int) bool {
	if s.AppUIDBase > 0 && uid >= s.AppUIDBase && uid > s.uidHWM {
		s.uidHWM = uid
		return true
	}
	return false
}

// SyncUIDHighWater persists the in-memory mark. Called after restore
// observed every persisted RunAs: if the mark file was lost or is
// older than state.json, a later delete + restart must still not hand
// those UIDs out again.
func (s *Supervisor) SyncUIDHighWater() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uidHWM == 0 {
		return nil
	}
	return s.persistUIDHighWater(s.uidHWM)
}

// inheritIdentity gives a blue-green v2 the identity of v1 unless the
// deploy names one: v1's UID (the files it wrote are owned by it), or
// root when v1 was spawned with RunAsRoot. Without the second case a
// deploy that omits run_as_root would move a root app onto a fresh UID
// that cannot read what v1 wrote.
func inheritIdentity(v2 *Config, v1 *App) {
	if v2.RunAs != nil || v2.RunAsRoot {
		return
	}
	if v1.runAsRoot {
		v2.RunAsRoot = true
		return
	}
	v2.RunAs = v1.RunAs()
}

// LoadUIDHighWater reads the persisted high-water mark from
// UIDStatePath. A missing file is a fresh host (mark 0); an unreadable
// or malformed one is an error, because guessing low would reuse UIDs.
func (s *Supervisor) LoadUIDHighWater() error {
	if s.UIDStatePath == "" {
		return nil
	}
	data, err := os.ReadFile(s.UIDStatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 0 {
		return fmt.Errorf("supervisor: malformed uid high-water mark in %s: %q", s.UIDStatePath, strings.TrimSpace(string(data)))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > s.uidHWM {
		s.uidHWM = n
	}
	return nil
}

// persistUIDHighWater writes uid to UIDStatePath durably: temp file,
// fsync, rename, fsync the directory. A mark lost to a power cut
// would let the next boot hand the same UID to a different app.
func (s *Supervisor) persistUIDHighWater(uid int) error {
	if s.UIDStatePath == "" {
		return nil
	}
	dir := filepath.Dir(s.UIDStatePath)
	tmp, err := os.CreateTemp(dir, ".app-uid-hwm-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.WriteString(strconv.Itoa(uid) + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.UIDStatePath); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
