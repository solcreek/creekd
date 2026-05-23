// Package supervisor manages the lifecycle of child application
// processes.
//
// Each app is spawned as a direct child of the supervisor (no
// container daemon, no init shim). A per-app watch goroutine drives
// the lifecycle: it observes the child via cmd.Wait(), records exit,
// applies the restart policy (exponential backoff + crash-loop
// detection inside RestartWindow), and re-spawns. Stop() removes the
// app from the registry so its watcher won't restart it; StopAll()
// drains every supervised process during graceful shutdown.
//
// Beyond bare spawning the package owns:
//
//   - Multi-runtime resolution (Bun / Node / Deno entry → exec via
//     internal/runtime) alongside the explicit Command + Args mode.
//   - Health probing through a pluggable HealthChecker, with
//     consecutive-failure → SIGKILL → restart escalation gated by
//     HealthCheckFailureThreshold.
//   - Blue-green Deploy: spawn v2 under a synthetic temp registry
//     key, await healthy, atomically swap the canonical key and the
//     dispatch route, then drain v1.
//   - cgroup v2 limits via internal/cgroup (memory + swap=0, pids,
//     cpu.max) attached via CLONE_INTO_CGROUP at clone3 time.
//   - Linux sandbox composition (PID / UTS / IPC / mount / user
//     namespaces, chroot, NoNewPrivs via setpriv wrap) via
//     internal/sandbox.
//   - Per-app network namespace + veth + bridge + masquerade via
//     internal/network. NetIP propagates to dispatch routing and
//     health probes for net-isolated apps.
//   - Per-app log capture and rotation via internal/logs.
//
// The package is intentionally a single file rather than carved
// into health/deploy/network sub-packages; the abstractions don't
// pay for themselves yet and the lifecycle is the natural unit. The
// public API surface (Spawn, Stop, Restart, Reset, Deploy, Get) is
// small even though the implementation isn't.
package supervisor

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/solcreek/creekd/internal/cgroup"
	"github.com/solcreek/creekd/internal/dispatch"
	"github.com/solcreek/creekd/internal/logs"
	"github.com/solcreek/creekd/internal/network"
	"github.com/solcreek/creekd/internal/runtime"
	"github.com/solcreek/creekd/internal/sandbox"
)

// Status is the lifecycle state of a supervised application.
type Status int

const (
	StatusUnknown Status = iota
	StatusStarting
	StatusRunning
	StatusCrashed
	StatusCrashLooping // suspended: too many crashes in a short window
	StatusStopped      // terminal: removed from registry, will not restart
	StatusUnhealthy    // running but failing health probes
)

// String returns the human-readable status name (for logs and admin API).
func (s Status) String() string {
	switch s {
	case StatusStarting:
		return "starting"
	case StatusRunning:
		return "running"
	case StatusCrashed:
		return "crashed"
	case StatusCrashLooping:
		return "crash-looping"
	case StatusStopped:
		return "stopped"
	case StatusUnhealthy:
		return "unhealthy"
	default:
		return "unknown"
	}
}

// Config describes how to spawn a supervised app.
//
// Two ways to specify what to run:
//
//   - Explicit Command + Args (low-level escape hatch, used by tests
//     and any caller that has already resolved the binary).
//   - Runtime + Entry: the supervisor resolves via runtime.Command to
//     pick "bun <entry>", "node <entry>", or "deno run -A <entry>".
//
// If both are set, Command + Args wins. Args passed alongside an
// explicit Command are used verbatim; extra Args alongside Runtime +
// Entry are appended after the entry script.
type Config struct {
	ID      string
	Command string          // executable, e.g. "bun"
	Args    []string        // arguments, e.g. ["server.ts"]
	Runtime runtime.Runtime // M5.4: "bun" | "node" | "deno"
	Entry   string          // M5.4: entry script for Runtime resolution
	Port    int             // assigned dispatch port, passed as PORT env var
	Env     []string        // additional environment variables

	// CgroupLimits opts into cgroup v2 enforcement (M5.5). When set
	// AND the supervisor has CgroupParent configured AND the host is
	// Linux, the child is spawned via CLONE_INTO_CGROUP under a
	// dedicated sub-cgroup with these limits applied. Nil or a
	// non-Linux host falls back to standard exec.
	CgroupLimits *cgroup.Limits

	// Sandbox opts into Linux namespace isolation + optional chroot.
	// Composes with CgroupLimits in a single clone3: cgroup attach,
	// CLONE_NEW* flags, and Chroot are applied to the same child.
	//
	// Special case: when VolumeMounts is non-empty AND Sandbox is
	// either nil OR present with all fields at the zero value, the
	// supervisor automatically defaults MountNamespace, PIDNamespace,
	// and NoNewPrivs to true at Spawn time. Pentest review identified
	// "tenant runs as root in host mount NS by default" as the #1
	// issue — defaults flip from opt-in to opt-out for stateful
	// workloads. Callers who want host-NS visibility for a stateful
	// app must explicitly set at least one Sandbox field to take
	// ownership of the policy.
	Sandbox *sandbox.Spec

	// NetIsolation opts into per-app network namespace + veth wiring.
	// When true AND Supervisor.NetSubnet + NetBridgeName are
	// configured AND the host is Linux, the supervisor:
	//   1. allocates an IP from the configured subnet pool
	//   2. creates a persistent netns at /var/run/netns/<appID>
	//   3. creates a veth pair, attaches host side to the bridge,
	//      moves container side into the netns, configures the IP +
	//      default route
	//   4. spawns the child via `ip netns exec` so it inherits the
	//      configured netns
	// Cleanup undoes all of the above when the app is stopped. The
	// container IP is exposed via App.NetIP for dispatch routing.
	NetIsolation bool

	// HealthCheckPath overrides the supervisor-wide health probe path
	// for this one app. Empty falls back to the supervisor default
	// ("/" — any HTTP server responds to it). Set this to a specific
	// readiness endpoint (e.g. "/healthz", "/-/ready") when the app
	// exposes a meaningful health route that should distinguish
	// "alive" from "ready to serve traffic".
	HealthCheckPath string

	// VolumeMounts declares per-app bind mounts that reference
	// supervisor-registered Volumes. Each entry binds Volume[VolumeID]
	// (optionally a SubPath inside it) onto Target so the child sees
	// a stable filesystem path that survives process restart.
	// Linux-only; non-Linux hosts reject any non-empty VolumeMounts
	// at Spawn.
	//
	// VolumeID must reference a Volume previously declared via
	// Supervisor.RegisterVolume. The two-layer split exists because
	// Volume lifecycle (host-side mount, MS_PRIVATE propagation
	// isolation, openat2-anchored fd) is owned by the supervisor and
	// decoupled from any one app's lifecycle; VolumeMount is the
	// per-app projection.
	VolumeMounts []VolumeMount
}

// VolumeMount projects a supervisor-registered Volume into one
// supervised app's filesystem view. See docs/RFC-stateful-substrate.md
// extension 1 and the volume.go doc comment for the two-layer
// rationale.
type VolumeMount struct {
	// VolumeID references a Volume previously declared via
	// Supervisor.RegisterVolume. Spawn fails with ErrVolumeNotFound
	// when the volume is missing.
	VolumeID string

	// SubPath optionally narrows the bind to a subdirectory of the
	// referenced volume. Must be relative, no "..", no leading "/".
	// Empty means bind the whole volume.
	SubPath string

	// Target is the path the child process sees. Must be absolute.
	// When Config.Sandbox.Chroot is set the bind is placed at
	// <Chroot>/<Target> on the host so the child observes it at
	// Target inside the chroot.
	Target string

	// ReadOnly overrides the Volume's default ReadOnly for this one
	// projection. The override is one-way: a RW Volume can be
	// projected RO, but an RO Volume cannot be projected RW. The
	// underlying bind is hardened with MS_NOSUID|MS_NODEV
	// regardless — see resolveAndValidate.
	ReadOnly bool
}

// CloneConfig returns a deep copy of cfg. The shallow struct copy
// Go does on assignment would leave the slice/pointer fields
// (Args, Env, CgroupLimits, Sandbox, VolumeMounts) aliasing the
// original — meaning a later caller mutating cfg.Env or
// cfg.Sandbox.UIDMappings would silently corrupt anything else
// holding the "same" Config (notably state.Store's persisted
// snapshot).
//
// Callers that take ownership of a Config — Store on insertion, Store
// on read-back — must clone before keeping a reference. Keep this in
// sync with the Config struct: when a new slice or pointer field is
// added above, deep-copy it here too.
func CloneConfig(cfg Config) Config {
	out := cfg
	out.Args = append([]string(nil), cfg.Args...)
	out.Env = append([]string(nil), cfg.Env...)
	out.VolumeMounts = append([]VolumeMount(nil), cfg.VolumeMounts...)
	if cfg.CgroupLimits != nil {
		limits := *cfg.CgroupLimits
		out.CgroupLimits = &limits
	}
	if cfg.Sandbox != nil {
		spec := *cfg.Sandbox
		spec.UIDMappings = append([]sandbox.IDMap(nil), cfg.Sandbox.UIDMappings...)
		spec.GIDMappings = append([]sandbox.IDMap(nil), cfg.Sandbox.GIDMappings...)
		out.Sandbox = &spec
	}
	return out
}

// App is one supervised application instance.
//
// Exported fields (ID, Runtime, Command, Args, Port) are immutable
// after Spawn and safe to read without locking. Mutable runtime state
// (cmd, status, startedAt, restarts) is guarded by App.mu; access via
// the accessor methods.
type App struct {
	ID      string
	Runtime runtime.Runtime // empty when Spawn was called with explicit Command
	Command string
	Args    []string
	Port    int

	// HealthCheckPath is the per-app override for the HTTP health
	// probe path. Empty means use the supervisor-wide default. Set
	// at Spawn time from Config; immutable across this App's life.
	HealthCheckPath string

	// env holds the extra environment variables from Config.Env.
	// Used by every restart path (watch goroutine + Reset) so the
	// child sees the same KEY=VAL pairs across its lifetime. A
	// defensive copy is taken at Spawn time so mutating the caller's
	// slice after the fact doesn't silently change restart behaviour.
	env []string

	mu        sync.RWMutex
	cmd       *exec.Cmd
	status    Status
	startedAt time.Time

	// restarts holds timestamps of recent restarts. Trimmed to entries
	// within RestartWindow on every record. Used for crash-loop
	// detection.
	restarts []time.Time

	// done is closed when the watch goroutine terminates. Callers can
	// select on it to know when the app has fully stopped (used by
	// graceful shutdown for SIGTERM → SIGKILL escalation).
	done chan struct{}

	// healthFailures is a monotonic counter of failed health probes
	// across this app's lifetime. Exposed via HealthFailures(). Read
	// and written via sync/atomic.
	healthFailures int64

	// sup is a back-reference to the owning Supervisor, used by
	// setStatus to emit events. Set at Spawn time.
	sup *Supervisor

	// rotator captures stdout/stderr to per-app log files when LogDir
	// is configured. Nil when LogDir is empty (tests use Stdout/Stderr
	// writers directly). Owned by the App for its full lifetime;
	// closed during Stop.
	rotator *logs.Rotator

	// cg is the per-app cgroup, allocated when CgroupLimits is set
	// and the host supports it. Lives for the App's lifetime; removed
	// in stopApp after the process exits.
	cg *cgroup.Cgroup
	// cgLimits records the limits originally applied, so the same
	// limits are re-applied across restarts (cgroup is reused, but
	// kernel may reset some files on certain operations).
	cgLimits *cgroup.Limits

	// sandbox stores the namespace/chroot spec to apply on every
	// (re)start. Nil disables isolation.
	sandbox *sandbox.Spec

	// Net* hold the per-app network namespace + veth pair + allocated
	// container IP when Config.NetIsolation was set at Spawn. NetIP
	// is exported so callers (notably the admin API) can route
	// dispatch traffic to the container-side address.
	NetIP      net.IP
	NetGateway net.IP
	netNS      *network.Namespace
	veth       *network.VethPair

	// volumeRefs holds the IDs of every Volume this app references
	// via Config.VolumeMounts at Spawn time. Used by
	// UnregisterVolume to refuse removal of in-use Volumes. Frozen
	// at Spawn; safe to read without locking after.
	volumeRefs []string
}

// HealthFailures returns the cumulative count of failed health probes
// since this App was spawned.
func (a *App) HealthFailures() int64 {
	return atomic.LoadInt64(&a.healthFailures)
}

// Cgroup returns the per-app cgroup handle, or nil when the app was
// spawned without CgroupLimits (no enforcement, no kernel-side
// accounting). Callers should treat nil as "cgroup_enabled=false"
// and fall back to OS-level state — uptime, restart count, etc.
func (a *App) Cgroup() *cgroup.Cgroup {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cg
}

// renameUnderLock updates App.ID. Used by Deploy to re-key v2 from its
// temporary registry key to the canonical app ID under the supervisor's
// registry lock. Outside of Deploy, App.ID is immutable; callers that
// hold A.PID()/A.Port and other fields by stale value see no harm —
// the renamed App is the same process with a different label.
func (a *App) renameUnderLock(newID string) {
	a.mu.Lock()
	a.ID = newID
	a.mu.Unlock()
}

// PID returns the OS process id, or 0 if the process is not running.
func (a *App) PID() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.cmd == nil || a.cmd.Process == nil {
		return 0
	}
	return a.cmd.Process.Pid
}

// Status returns the current lifecycle state.
func (a *App) Status() Status {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.status
}

// Uptime returns time since the current process started.
func (a *App) Uptime() time.Duration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.startedAt.IsZero() {
		return 0
	}
	return time.Since(a.startedAt)
}

// setState mutates the runtime state under App.mu.
func (a *App) setState(cmd *exec.Cmd, status Status, startedAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cmd = cmd
	a.status = status
	a.startedAt = startedAt
}

// setStatus updates just the status under App.mu and emits an event
// if the supervisor's EventBus is wired up.
func (a *App) setStatus(status Status) {
	a.mu.Lock()
	a.status = status
	a.mu.Unlock()
	if a.sup != nil {
		a.sup.emit(Event{
			Type:         EventStatusChanged,
			AppID:        a.ID,
			Status:       status.String(),
			PID:          a.PID(),
			RestartCount: a.RestartCount(),
			Timestamp:    time.Now(),
		})
	}
}

// emit publishes an event to the supervisor's EventBus. Nil-safe.
func (s *Supervisor) emit(e Event) {
	if s.Events != nil {
		s.Events.Publish(e)
	}
}

// snapshotCmd returns the current exec.Cmd reference under lock so the
// caller can wait/signal without racing with restarts.
func (a *App) snapshotCmd() *exec.Cmd {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cmd
}

// Env returns a defensive copy of the app's environment variables.
func (a *App) Env() []string {
	return append([]string(nil), a.env...)
}

// RestartCount returns the number of restarts observed within the
// supervisor's RestartWindow.
func (a *App) RestartCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.restarts)
}

// recordRestart appends now to the restart log and trims entries older
// than window. Returns the resulting count for crash-loop comparison.
func (a *App) recordRestart(now time.Time, window time.Duration) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.restarts = append(a.restarts, now)
	cutoff := now.Add(-window)
	// Trim in place: find first entry not before cutoff.
	i := 0
	for ; i < len(a.restarts); i++ {
		if !a.restarts[i].Before(cutoff) {
			break
		}
	}
	if i > 0 {
		a.restarts = a.restarts[i:]
	}
	return len(a.restarts)
}

// clearRestarts wipes the restart log. Used by Reset() to clear
// crash-loop state.
func (a *App) clearRestarts() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.restarts = nil
}

// resetDone replaces the done channel with a fresh one. The previous
// channel was already closed by the prior watch goroutine; this prepares
// the App for a new watch goroutine (used by Reset()). Caller must
// ensure the prior watch goroutine has exited before calling this.
func (a *App) resetDone() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.done = make(chan struct{})
}

// waitDone returns the current done channel under lock. Used by stop
// paths so they observe whichever channel the active watch will close.
func (a *App) waitDone() chan struct{} {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.done
}

// Supervisor owns the registry of running apps and the goroutines that
// watch them.
type Supervisor struct {
	mu     sync.RWMutex
	apps   map[string]*App
	logger *slog.Logger

	// Restart policy (M5.2). Tuned for tests via the constructor.
	InitialBackoff     time.Duration // first restart delay (default 1s)
	MaxBackoff         time.Duration // cap on backoff doubling (default 30s)
	RestartWindow      time.Duration // sliding window for crash-loop detection (default 60s)
	CrashLoopThreshold int           // restarts in window before suspending (default 5)

	// Graceful shutdown (M5.3a). How long Stop waits between SIGTERM and
	// SIGKILL escalation. Default 30s for production; tests use shorter.
	GracefulShutdownTimeout time.Duration

	// LogDir is the root directory for per-app log files (M5.6). When
	// set, each app's stdout/stderr is captured through a logs.Rotator
	// at <LogDir>/<appID>/current.log instead of being forwarded
	// directly to Stdout/Stderr. Stdout/Stderr remain the fallback for
	// tests and smoke runs that don't want disk capture.
	LogDir string

	// Stdout / Stderr are the writers each child's stdout/stderr is
	// forwarded to when LogDir is empty. Production typically sets
	// LogDir; tests use io.Discard here to avoid leaking pipes.
	Stdout io.Writer
	Stderr io.Writer

	// WaitDelay bounds how long exec.Cmd.Wait waits for I/O drain after
	// the child exits. Set non-zero to ensure tests don't hang when
	// pipes are inherited.
	WaitDelay time.Duration

	// Health probe (M5.3b).
	// HealthCheckInterval is the period between probes. Zero disables
	// the probe goroutine entirely.
	HealthCheckInterval time.Duration
	// HealthCheckTimeout caps each individual probe.
	HealthCheckTimeout time.Duration
	// HealthCheckFailureThreshold is the number of consecutive failing
	// probes required before the supervisor restarts the app. One success
	// resets the counter.
	HealthCheckFailureThreshold int
	// HealthChecker is the probe implementation. Defaults to an HTTP
	// GET against /health on the app's PORT. Tests override with a mock.
	HealthChecker HealthChecker

	// CgroupParent is the cgroup v2 slice name that owns all per-app
	// sub-cgroups (M5.5). Empty disables cgroup enforcement entirely.
	// Example: "creekd.slice". Only meaningful on Linux; on other
	// platforms the cgroup package is a no-op shim.
	CgroupParent string

	// DefaultMemoryHigh is the daemon-wide floor for memory.high. When
	// non-zero AND CgroupParent is set, every Spawn that did not
	// receive an explicit CgroupLimits.MemoryHigh has this value
	// injected. Explicit per-app values always win — this sets the
	// policy floor, not the ceiling.
	//
	// The whole-fleet effect is opt-out noisy-neighbor protection:
	// even apps spawned with cfg.CgroupLimits == nil end up inside a
	// per-app cgroup with the soft cap applied.
	//
	// 0 disables; the daemon falls back to "only enforce what the
	// caller requested". Recommended production value: 256 MiB (see
	// examples/cgroup-memory-tuning/RESULTS.md for the empirical
	// justification).
	DefaultMemoryHigh int64

	// DefaultMemoryMax is the daemon-wide floor for memory.max — the
	// hard cap that triggers a cgroup-scoped OOM kill when crossed.
	// Same opt-out semantics as DefaultMemoryHigh: applied whenever
	// the caller didn't pass an explicit MemoryMax and CgroupParent
	// is configured.
	//
	// In practice memory.high catches every realistic allocation
	// pattern (see examples/cgroup-memory-tuning Phase 4-5); this
	// hard cap is the safety net for the rare case where reclaim
	// genuinely can't keep up. Recommended production value: 1 GiB
	// (≈3.6× the empirical worst-case peak under memory.high=256M).
	DefaultMemoryMax int64

	// NetSubnet + NetBridgeName configure per-app network namespace
	// support (M5.10). Both must be set for Config.NetIsolation to
	// have any effect. NetSubnet is an IPv4 CIDR like "10.42.0.0/24";
	// the first usable address becomes the bridge gateway, the rest
	// are allocated to apps. NetBridgeName names the kernel bridge
	// interface (e.g. "creekd0") shared by all apps' veth peers.
	NetSubnet     string
	NetBridgeName string

	// VolumeRoot is the base directory under which relative
	// Volume.BackingPath values are resolved AND the containment
	// anchor for all volume operations. Empty means VolumeMounts
	// are disabled entirely (every RegisterVolume call fails).
	//
	// The supervisor pins an O_PATH fd of this directory at first
	// volume registration and uses openat2(RESOLVE_BENEATH | …) for
	// every subsequent path resolution. This is the load-bearing
	// security primitive — caller-supplied BackingPath strings
	// cannot escape this directory via symlinks, "..", or TOCTOU.
	//
	// Recommended layout: VolumeRoot = "/var/lib/creekd/volumes",
	// callers register Volumes with BackingPath = "<tenant>/<vol>".
	VolumeRoot string

	// AllowedTargetPrefixes restricts where VolumeMounts may be
	// bound on the HOST when the app has no Sandbox.Chroot. Without
	// this, an orchestrator could pass Target: "/etc" and overlay
	// system files. With Sandbox.Chroot set, the chroot itself
	// provides containment and this allowlist is bypassed.
	//
	// Each entry must be absolute and is matched as a prefix after
	// filepath.Clean. Empty means "no host targets allowed" — the
	// only legal use of VolumeMounts is then inside a chroot.
	//
	// Recommended Phase 1 value: ["/data", "/var/lib/app"] for the
	// common "no-chroot, stateless container conventions" case.
	AllowedTargetPrefixes []string

	// volumes is the registry of declared Volumes. Lifecycle is
	// decoupled from individual app spawns — a Volume registered
	// once is referenced by VolumeID from any app's VolumeMounts.
	// Protected by volumesMu.
	volumesMu sync.RWMutex
	volumes   map[string]*Volume

	// volumeRootFD is the pinned O_PATH fd of VolumeRoot. Opened
	// lazily on first RegisterVolume. All subsequent path resolution
	// happens via openat2 anchored here.
	volumeRootOnce sync.Once
	volumeRootFD   int // -1 when not yet opened
	volumeRootErr  error

	// cgMgr is lazily constructed from CgroupParent on first use.
	cgMgrOnce sync.Once
	cgMgr     *cgroup.Manager

	// netOnce + friends lazily set up the bridge, pool, and NAT rule
	// on the first NetIsolation spawn.
	netOnce   sync.Once
	netPool   *network.IPPool
	netBridge *network.Bridge
	netErr    error

	// Events is the pub/sub bus for app state transitions. Subscribers
	// (e.g. the admin API SSE endpoint) receive events for all apps.
	// Nil-safe: if unset, no events are emitted.
	Events *EventBus
}

// cgroupManager returns the lazily-constructed cgroup manager, or nil
// when CgroupParent is empty (enforcement disabled).
func (s *Supervisor) cgroupManager() *cgroup.Manager {
	if s.CgroupParent == "" {
		return nil
	}
	s.cgMgrOnce.Do(func() {
		s.cgMgr = cgroup.NewManager(s.CgroupParent)
	})
	return s.cgMgr
}

// applyCgroupDefaults injects DefaultMemoryHigh and DefaultMemoryMax
// into cfg.CgroupLimits when the supervisor has defaults configured
// and the caller hasn't set those fields explicitly. Returns the
// (possibly mutated) cfg.
//
// Skipped entirely when CgroupParent is empty (no slice to hold the
// limits). Each default is applied independently: a caller can set
// MemoryHigh explicitly and still get the daemon-wide MemoryMax
// default, or vice-versa. Explicit per-app values always win.
func (s *Supervisor) applyCgroupDefaults(cfg Config) Config {
	if s.CgroupParent == "" {
		return cfg
	}
	if s.DefaultMemoryHigh <= 0 && s.DefaultMemoryMax <= 0 {
		return cfg
	}
	if cfg.CgroupLimits == nil {
		cfg.CgroupLimits = &cgroup.Limits{
			MemoryHigh: s.DefaultMemoryHigh,
			MemoryMax:  s.DefaultMemoryMax,
		}
		return cfg
	}
	lim := *cfg.CgroupLimits
	mutated := false
	if lim.MemoryHigh == 0 && s.DefaultMemoryHigh > 0 {
		lim.MemoryHigh = s.DefaultMemoryHigh
		mutated = true
	}
	if lim.MemoryMax == 0 && s.DefaultMemoryMax > 0 {
		lim.MemoryMax = s.DefaultMemoryMax
		mutated = true
	}
	if mutated {
		cfg.CgroupLimits = &lim
	}
	return cfg
}

// setupAppNetwork allocates an IP, creates the netns, and wires the
// veth pair for an app whose Config.NetIsolation was set. On any
// step's failure, partial state is rolled back before returning the
// error. Mutates app.netNS / app.veth / app.NetIP / app.NetGateway
// in place; caller is responsible for reverse-engineering on later
// failures via teardownAppNetwork.
func (s *Supervisor) setupAppNetwork(app *App) error {
	ready, err := s.netReady()
	if err != nil {
		return fmt.Errorf("supervisor: net subsystem: %w", err)
	}
	if !ready {
		return errors.New("supervisor: NetIsolation requires both NetSubnet and NetBridgeName")
	}

	ip, err := s.netPool.Allocate()
	if err != nil {
		return fmt.Errorf("supervisor: allocate ip: %w", err)
	}

	ns := &network.Namespace{Name: app.ID}
	if err := ns.Create(); err != nil {
		s.netPool.Release(ip)
		return fmt.Errorf("supervisor: create netns: %w", err)
	}

	suffix := shortHash(app.ID)
	veth := &network.VethPair{
		HostName:      "cvh" + suffix,
		ContainerName: "cvc" + suffix,
		Bridge:        s.netBridge,
		Namespace:     ns,
		ContainerIP:   ip,
		Gateway:       s.netPool.Gateway(),
		PrefixLen:     s.netPool.Mask(),
	}
	if err := veth.Setup(); err != nil {
		_ = ns.Delete()
		s.netPool.Release(ip)
		return fmt.Errorf("supervisor: veth setup: %w", err)
	}

	app.NetIP = ip
	app.NetGateway = s.netPool.Gateway()
	app.netNS = ns
	app.veth = veth
	return nil
}

// teardownAppNetwork undoes setupAppNetwork in reverse: removes the
// veth pair (kernel reaps the container side automatically), deletes
// the netns, releases the IP back to the pool. Idempotent and
// failure-tolerant.
func (s *Supervisor) teardownAppNetwork(app *App) {
	if app.veth != nil {
		if err := app.veth.Teardown(); err != nil {
			s.logger.Warn("veth teardown failed", "id", app.ID, "err", err)
		}
		app.veth = nil
	}
	if app.netNS != nil {
		if err := app.netNS.Delete(); err != nil {
			s.logger.Warn("netns delete failed", "id", app.ID, "err", err)
		}
		app.netNS = nil
	}
	if app.NetIP != nil && s.netPool != nil {
		s.netPool.Release(app.NetIP)
		app.NetIP = nil
		app.NetGateway = nil
	}
}

// shortHash returns 8 hex chars of an FNV-32 hash of s. Used for
// generating veth interface names that fit IFNAMSIZ (15 chars).
// Deterministic per app ID, so a restart with the same ID picks the
// same names — useful when leftover state from a crash needs
// best-effort cleanup.
func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))[:8]
}

// netReady reports whether per-app network isolation is configured
// and brings up the shared bridge + MASQUERADE rule on first use.
// Returns (false, nil) when NetSubnet/NetBridgeName are empty,
// (false, err) on setup failure (subsequent calls return the same
// cached error), and (true, nil) once the bridge is ready.
func (s *Supervisor) netReady() (bool, error) {
	if s.NetSubnet == "" || s.NetBridgeName == "" {
		return false, nil
	}
	s.netOnce.Do(func() {
		pool, err := network.NewIPPool(s.NetSubnet)
		if err != nil {
			s.netErr = fmt.Errorf("net pool: %w", err)
			return
		}
		s.netPool = pool
		s.netBridge = &network.Bridge{Name: s.NetBridgeName, Pool: pool}
		if err := s.netBridge.Ensure(); err != nil {
			s.netErr = fmt.Errorf("net bridge: %w", err)
			return
		}
		if err := network.EnsureMasquerade(pool.CIDR(), s.NetBridgeName); err != nil {
			s.netErr = fmt.Errorf("net masquerade: %w", err)
			return
		}
		s.logger.Info("net subsystem ready",
			"subnet", s.NetSubnet, "bridge", s.NetBridgeName,
			"gateway", pool.Gateway().String(),
		)
	})
	if s.netErr != nil {
		return false, s.netErr
	}
	return true, nil
}

// HealthChecker probes a running supervised app and returns nil if it
// is healthy. M5.3b uses the result to decide whether to escalate to a
// process restart.
type HealthChecker interface {
	Check(ctx context.Context, app *App) error
}

// HTTPHealthChecker performs `GET http://127.0.0.1:<port><Path>` and
// considers the app **alive** iff the connection succeeds and the
// server returns any non-5xx response within the timeout. 4xx (e.g.
// 404 when the app doesn't expose Path) does NOT count as a failure —
// the goal is "is the HTTP server responsive?", not "does this
// specific endpoint exist?". Apps that want strict readiness checks
// can configure a specific endpoint via per-app HealthCheckPath +
// expose it with the response code they want.
//
// This is a deliberately lenient default. The earlier strict 2xx-only
// behaviour false-positive-killed every app that didn't expose
// `/health`, which is most apps in practice (the convention is split
// between /health, /healthz, /-/health, none, etc.).
type HTTPHealthChecker struct {
	Path   string
	Client *http.Client
}

// Check implements HealthChecker.
func (h *HTTPHealthChecker) Check(ctx context.Context, app *App) error {
	// Per-app override wins over the supervisor-wide default; both
	// empty falls back to "/" (the most permissive endpoint — any
	// HTTP server that accepts requests will answer it).
	path := app.HealthCheckPath
	if path == "" {
		path = h.Path
	}
	if path == "" {
		path = "/"
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	// For net-isolated apps the listener is in a private netns at
	// app.NetIP, not at the host's 127.0.0.1. Hitting 127.0.0.1 here
	// would always 404/timeout — the same gap the dispatch router
	// crosses via the bridge. For shared-network apps app.NetIP is
	// nil and we fall back to the host loopback.
	host := "127.0.0.1"
	if app.NetIP != nil {
		host = app.NetIP.String()
	}
	url := fmt.Sprintf("http://%s:%d%s", host, app.Port, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 500 {
		return fmt.Errorf("health: server error %d", resp.StatusCode)
	}
	return nil
}

// New returns a fresh Supervisor with production defaults.
func New(logger *slog.Logger) *Supervisor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{
		apps:                        make(map[string]*App),
		volumes:                     make(map[string]*Volume),
		volumeRootFD:                -1,
		logger:                      logger,
		InitialBackoff:              1 * time.Second,
		MaxBackoff:                  30 * time.Second,
		RestartWindow:               60 * time.Second,
		CrashLoopThreshold:          5,
		GracefulShutdownTimeout:     30 * time.Second,
		Stdout:                      os.Stdout,
		Stderr:                      os.Stderr,
		WaitDelay:                   0,
		HealthCheckInterval:         10 * time.Second,
		HealthCheckTimeout:          2 * time.Second,
		HealthCheckFailureThreshold: 3,
		HealthChecker:               &HTTPHealthChecker{},
		Events:                      NewEventBus(),
	}
}

// computeBackoff returns the delay before the (count+1)-th restart.
// count is the number of restarts already in this window.
// Sequence (with default settings): 1s, 2s, 4s, 8s, 16s, 30s, 30s, ...
// runtimeIsLinux reports whether we're on Linux (where namespace
// isolation actually works). On macOS/Windows, sandbox.Apply returns
// ErrUnsupported, so we skip default isolation to avoid breaking dev.
func runtimeIsLinux() bool {
	return goos == "linux"
}

// applyDefaultSandbox mutates cfg.Sandbox in place to apply the
// secure-by-default isolation policy. Two paths:
//
//  1. Privileged Linux (canApplyDefaultSandbox): default PID +
//     NoNewPrivs for every app, plus Mount namespace when VolumeMounts
//     are present. Pentest review flagged cross-tenant /proc
//     visibility and setuid escalation as the things this guards
//     against.
//  2. Anywhere else (macOS dev, non-root CI, unprivileged hosts):
//     only apply the legacy "stateful apps get full isolation"
//     default when VolumeMounts is non-empty. Stateless apps stay
//     unsandboxed because applying CLONE_NEWPID without
//     CAP_SYS_ADMIN fails at clone() with EPERM and surfaces as a
//     misleading setpriv error.
//
// Callers can opt out of any defaulting by passing an explicit
// non-zero Sandbox (any field true wins — the auto-flip only fires
// when Sandbox is nil or has every field at zero value).
func applyDefaultSandbox(cfg *Config) {
	if canApplyDefaultSandbox() {
		if cfg.Sandbox == nil {
			cfg.Sandbox = &sandbox.Spec{
				PIDNamespace: true,
				NoNewPrivs:   true,
			}
			if len(cfg.VolumeMounts) > 0 {
				cfg.Sandbox.MountNamespace = true
			}
		} else if !cfg.Sandbox.Any() {
			cfg.Sandbox.PIDNamespace = true
			cfg.Sandbox.NoNewPrivs = true
			if len(cfg.VolumeMounts) > 0 {
				cfg.Sandbox.MountNamespace = true
			}
		}
		return
	}
	// Unprivileged path: legacy stateful-only default.
	if len(cfg.VolumeMounts) == 0 {
		return
	}
	if cfg.Sandbox == nil {
		cfg.Sandbox = &sandbox.Spec{
			MountNamespace: true,
			PIDNamespace:   true,
			NoNewPrivs:     true,
		}
		return
	}
	if !cfg.Sandbox.Any() {
		cfg.Sandbox.MountNamespace = true
		cfg.Sandbox.PIDNamespace = true
		cfg.Sandbox.NoNewPrivs = true
	}
}

// canApplyDefaultSandbox reports whether the supervisor is running
// with the privilege actually required to enforce the secure-by-
// default sandbox (PID namespace via CLONE_NEWPID, NoNewPrivs via
// the setpriv wrapper which itself runs under our clone).
//
// We default isolation ON for the production scenario (creekd
// running as root on Linux — the only place it can manage cgroups,
// netns, and the full sandbox surface anyway). For everything else
// — dev on macOS, CI on a non-privileged runner, integration tests
// that spawn a real creekd as the test user — we skip the defaults
// rather than try to apply them and fail at clone time with a
// "fork/exec /usr/bin/setpriv: operation not permitted" that
// misleadingly blames setpriv when the actual failure is
// CLONE_NEWPID rejected for lack of CAP_SYS_ADMIN.
//
// Callers who want defaults applied in an unprivileged context
// (e.g. with user namespaces) should pass an explicit Sandbox.
func canApplyDefaultSandbox() bool {
	if !runtimeIsLinux() {
		return false
	}
	return geteuid() == 0
}

var goos = goruntime.GOOS

// geteuid is overridable for tests. Default delegates to os.Geteuid.
var geteuid = func() int { return os.Geteuid() }

// filterSupervisorEnv removes CREEKD_* and CREEKCTL_* env vars from
// the parent's environment before passing to child processes. Prevents
// admin tokens and internal config from leaking to tenant apps.
func filterSupervisorEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, kv := range env {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if strings.HasPrefix(key, "CREEKD_") || strings.HasPrefix(key, "CREEKCTL_") {
			continue
		}
		filtered = append(filtered, kv)
	}
	return filtered
}

func (s *Supervisor) computeBackoff(count int) time.Duration {
	if count <= 0 {
		return s.InitialBackoff
	}
	d := s.InitialBackoff
	for i := 0; i < count; i++ {
		d *= 2
		if d >= s.MaxBackoff {
			return s.MaxBackoff
		}
	}
	return d
}

// Spawn starts a new supervised app. The ID is validated against
// ValidateID before any side effects — log dir creation, cgroup
// slice setup, netns allocation, registry insertion. Returns
// ErrInvalidID on grammar failure, ErrAlreadyRunning if an app with
// the same ID is already in the registry.
//
// External callers (admin API, state-file restore) should call this
// method. Internal callers that need to spawn under a synthetic
// non-grammar ID — Deploy's two-process blue-green flip uses
// deployTempID() which contains "__" — call spawnUnchecked directly.
// Admin API and restore paths also call ValidateID at their own
// entry point as an explicit guard; the duplicated check here is
// cheap (one string scan) and means new external callers can't
// accidentally skip validation.
func (s *Supervisor) Spawn(cfg Config) (*App, error) {
	if err := ValidateID(cfg.ID); err != nil {
		return nil, err
	}
	return s.spawnUnchecked(cfg)
}

// spawnUnchecked is the validation-free spawn primitive. Callers
// must ensure the ID is either grammar-valid (via ValidateID) OR a
// supervisor-internal synthetic ID (e.g. deployTempID). The only
// hard requirement here is non-empty — derived names (log dir,
// cgroup, netns) still need *some* string.
func (s *Supervisor) spawnUnchecked(cfg Config) (*App, error) {
	if cfg.ID == "" {
		return nil, errors.New("supervisor: empty app id")
	}

	cmd, args, rt, err := resolveExec(cfg)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.apps[cfg.ID]; exists {
		return nil, fmt.Errorf("supervisor: app %q already running: %w", cfg.ID, ErrAlreadyRunning)
	}

	app := &App{
		ID:              cfg.ID,
		Runtime:         rt,
		Command:         cmd,
		Args:            args,
		Port:            cfg.Port,
		HealthCheckPath: cfg.HealthCheckPath,
		env:             append([]string(nil), cfg.Env...),
		done:            make(chan struct{}),
		volumeRefs:      volumeIDs(cfg.VolumeMounts),
		sup:             s,
	}
	applyDefaultSandbox(&cfg)

	if cfg.Sandbox != nil {
		// Defensive copy so a mutation of cfg.Sandbox by the caller
		// after Spawn does not silently affect restarts.
		spec := *cfg.Sandbox
		app.sandbox = &spec
	}

	if s.LogDir != "" {
		rot, err := logs.NewRotator(s.LogDir, cfg.ID, logs.Options{})
		if err != nil {
			return nil, fmt.Errorf("supervisor: log rotator: %w", err)
		}
		app.rotator = rot
	}

	cfg = s.applyCgroupDefaults(cfg)

	if cfg.CgroupLimits != nil {
		mgr := s.cgroupManager()
		if mgr == nil {
			if app.rotator != nil {
				_ = app.rotator.Close()
			}
			return nil, errors.New("supervisor: CgroupLimits set but Supervisor.CgroupParent is empty")
		}
		cg, err := mgr.Create(cfg.ID, *cfg.CgroupLimits)
		if err != nil {
			if app.rotator != nil {
				_ = app.rotator.Close()
			}
			return nil, fmt.Errorf("supervisor: create cgroup: %w", err)
		}
		app.cg = cg
		// Copy the limits so a later mutation of cfg.CgroupLimits by
		// the caller does not silently affect restarts.
		lim := *cfg.CgroupLimits
		app.cgLimits = &lim
	}

	if cfg.NetIsolation {
		if err := s.setupAppNetwork(app); err != nil {
			if app.rotator != nil {
				_ = app.rotator.Close()
			}
			if app.cg != nil {
				_ = app.cg.Remove()
			}
			return nil, err
		}
	}

	// Bind-mount declared volumes after netns wiring (which mutates
	// no filesystem state) and before exec. Each VolumeMount
	// references a Volume already registered via RegisterVolume;
	// the bind happens in the host mount namespace under
	// MS_PRIVATE propagation so tenant-side mount events don't
	// leak. Failures here roll back the network namespace and
	// cgroup but NOT prior successful binds — those are idempotent
	// and safe to leave; the next Spawn observes them via
	// mountinfo identity check and reuses.
	chrootDir := ""
	if cfg.Sandbox != nil {
		chrootDir = cfg.Sandbox.Chroot
	}
	if err := s.applyVolumeMounts(cfg.VolumeMounts, chrootDir); err != nil {
		s.teardownAppNetwork(app)
		if app.rotator != nil {
			_ = app.rotator.Close()
		}
		if app.cg != nil {
			_ = app.cg.Remove()
		}
		return nil, err
	}

	if err := s.startLocked(app, app.env); err != nil {
		s.teardownAppNetwork(app)
		if app.rotator != nil {
			_ = app.rotator.Close()
		}
		if app.cg != nil {
			_ = app.cg.Remove()
		}
		return nil, err
	}

	s.apps[cfg.ID] = app
	go s.watch(app, app.env)
	if s.healthEnabled() {
		go s.probe(app, app.waitDone())
	}
	return app, nil
}

// resolveExec converts Config's two input modes (explicit Command+Args,
// or Runtime+Entry) into the executable + argv that startLocked needs.
// Explicit Command wins when both are set. Returns the resolved Runtime
// (or "" when an explicit Command was used).
func resolveExec(cfg Config) (string, []string, runtime.Runtime, error) {
	if cfg.Command != "" {
		return cfg.Command, cfg.Args, cfg.Runtime, nil
	}
	if cfg.Runtime == "" {
		return "", nil, "", errors.New("supervisor: empty command (set Command or Runtime+Entry)")
	}
	if !cfg.Runtime.Valid() {
		return "", nil, "", fmt.Errorf("supervisor: invalid runtime %q", cfg.Runtime)
	}
	if cfg.Entry == "" {
		return "", nil, "", errors.New("supervisor: empty entry for Runtime mode")
	}
	cmd, args, err := runtime.Command(cfg.Runtime, cfg.Entry, cfg.Args)
	if err != nil {
		return "", nil, "", fmt.Errorf("supervisor: resolve runtime: %w", err)
	}
	return cmd, args, cfg.Runtime, nil
}

// healthEnabled reports whether the probe goroutine should be started.
func (s *Supervisor) healthEnabled() bool {
	return s.HealthCheckInterval > 0 &&
		s.HealthCheckFailureThreshold > 0 &&
		s.HealthChecker != nil
}

// isTracked returns true iff the supervisor's registry still has app
// at its own ID *by pointer identity*. Deploy re-keys a different App
// under an existing ID; without pointer identity, the displaced App's
// watch goroutine would incorrectly treat itself as still tracked and
// try to restart on top of the new owner's slot.
func (s *Supervisor) isTracked(app *App) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cur, ok := s.apps[app.ID]
	return ok && cur == app
}

// startLocked spawns the OS process. Caller must hold s.mu. Mutates
// app via setState/setStatus which take App.mu internally.
func (s *Supervisor) startLocked(app *App, extraEnv []string) error {
	app.setStatus(StatusStarting)

	cmd := exec.Command(app.Command, app.Args...)

	// Wrap the cmd from the inside out:
	//   1. setpriv --no-new-privs (if Spec.NoNewPrivs) — the
	//      innermost wrapper applies prctl(PR_SET_NO_NEW_PRIVS, 1)
	//      then exec's the real binary
	//   2. ip netns exec (if Sandbox.NetNamespace via setupAppNetwork
	//      wired app.netNS) — setns into the configured network
	//      namespace, then exec's the rest of the chain
	// iproute2's `ip` and util-linux's `setpriv` both replace
	// themselves with exec, so cmd.Process.Pid is the leaf binary
	// throughout.
	if app.sandbox != nil && app.sandbox.NoNewPrivs {
		cmd = sandbox.WrapNoNewPrivs(cmd)
	}
	if app.netNS != nil {
		origPath := cmd.Path
		origArgs := cmd.Args
		cmd = exec.Command("ip", append([]string{
			"netns", "exec", app.netNS.Name, origPath,
		}, origArgs[1:]...)...)
	}

	env := filterSupervisorEnv(os.Environ())
	env = append(env, fmt.Sprintf("PORT=%d", app.Port))
	if len(extraEnv) > 0 {
		env = append(env, extraEnv...)
	}
	cmd.Env = env

	// When the app has a log rotator (LogDir is set), capture
	// stdout/stderr through it; each line is JSON-wrapped and lands in
	// <LogDir>/<appID>/current.log. Otherwise forward to the
	// supervisor's configured writers (default os.Stdout / os.Stderr).
	if app.rotator != nil {
		cmd.Stdout = app.rotator.Stdout()
		cmd.Stderr = app.rotator.Stderr()
	} else {
		cmd.Stdout = s.Stdout
		cmd.Stderr = s.Stderr
	}
	cmd.WaitDelay = s.WaitDelay

	// Apply namespace + chroot isolation (M5.9) BEFORE attaching the
	// cgroup fd. Both mutate SysProcAttr; sandbox.Apply sets
	// Cloneflags / Chroot, attachCgroup adds UseCgroupFD / CgroupFD.
	// Order doesn't affect correctness — they're additive — but doing
	// sandbox first surfaces an unsupported-platform error early,
	// before any cgroup fd is opened.
	if app.sandbox != nil {
		if err := sandbox.Apply(cmd, *app.sandbox); err != nil {
			app.setStatus(StatusCrashed)
			return fmt.Errorf("supervisor: apply sandbox: %w", err)
		}
	}

	// M5.5: if this app has a cgroup, spawn the child *inside* it via
	// CLONE_INTO_CGROUP so enforcement is active from the first
	// instruction — no race window where the child runs un-capped.
	// We open the fd here, hand it to the kernel via SysProcAttr,
	// and close it after Start (the kernel duplicated it during clone3).
	var cgFD *os.File
	if app.cg != nil {
		fd, err := app.cg.OpenFD()
		if err != nil {
			app.setStatus(StatusCrashed)
			return fmt.Errorf("supervisor: open cgroup fd: %w", err)
		}
		cgFD = fd
		attachCgroup(cmd, int(fd.Fd()))
	}

	if err := cmd.Start(); err != nil {
		if cgFD != nil {
			_ = cgFD.Close()
		}
		app.setStatus(StatusCrashed)
		return fmt.Errorf("supervisor: starting %q: %w", app.ID, err)
	}
	if cgFD != nil {
		_ = cgFD.Close()
	}

	app.setState(cmd, StatusRunning, time.Now())

	s.logger.Info("app spawned",
		"id", app.ID,
		"pid", cmd.Process.Pid,
		"port", app.Port,
	)
	return nil
}

// watch blocks on cmd.Wait() and restarts the process on exit, applying
// exponential backoff and crash-loop detection. Runs in its own goroutine.
// Closes app.done when it terminates (for graceful shutdown wait). Also
// closes the log rotator if watch is exiting AND the app is no longer
// tracked — covering crash-restart-failure paths where watch removes
// the app from the registry itself (Stop closes the rotator on its own).
//
// Tracking is checked by *pointer identity*, not by ID key alone:
// Deploy may re-key a different App under the same ID, and v1's watch
// must observe that as "not tracked" rather than restarting on top of
// v2's registry slot.
func (s *Supervisor) watch(app *App, extraEnv []string) {
	defer func() {
		close(app.done)
		if !s.isTracked(app) && app.rotator != nil {
			_ = app.rotator.Close() // idempotent
		}
	}()

	for {
		cmd := app.snapshotCmd()
		if cmd == nil {
			s.logger.Error("watch: cmd snapshot nil; aborting", "id", app.ID)
			return
		}
		err := cmd.Wait()
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}

		// If Stop() removed this app from the registry while the
		// process was running, treat exit as terminal. Pointer-identity
		// check tolerates Deploy re-keying a different App at the same
		// ID.
		if !s.isTracked(app) {
			app.setStatus(StatusStopped)
			s.logger.Info("app exited (stopped)",
				"id", app.ID,
				"exit_code", exitCode,
			)
			return
		}

		app.setStatus(StatusCrashed)

		// Record restart and check for crash-loop.
		restartCount := app.recordRestart(time.Now(), s.RestartWindow)
		if restartCount > s.CrashLoopThreshold {
			app.setStatus(StatusCrashLooping)
			s.logger.Warn("app entered crash-loop; suspending restart",
				"id", app.ID,
				"restarts_in_window", restartCount,
				"window", s.RestartWindow,
				"threshold", s.CrashLoopThreshold,
				"hint", "call Supervisor.Reset(id) to resume",
			)
			return
		}

		backoff := s.computeBackoff(restartCount - 1)
		s.logger.Info("app exited; restarting",
			"id", app.ID,
			"exit_code", exitCode,
			"err", err,
			"restart_count", restartCount,
			"backoff", backoff,
		)

		time.Sleep(backoff)

		s.mu.Lock()
		// Re-check tracking after the delay — Stop() may have raced,
		// or Deploy may have re-keyed this ID to a different App.
		if cur, ok := s.apps[app.ID]; !ok || cur != app {
			s.mu.Unlock()
			app.setStatus(StatusStopped)
			return
		}

		if err := s.startLocked(app, extraEnv); err != nil {
			s.logger.Error("restart failed",
				"id", app.ID,
				"err", err,
			)
			// Only delete if we still own this slot.
			if cur, ok := s.apps[app.ID]; ok && cur == app {
				delete(s.apps, app.ID)
			}
			s.mu.Unlock()
			app.setStatus(StatusStopped)
			return
		}
		s.mu.Unlock()

		// Loop and Wait() on the new process.
	}
}

// Reset clears the crash-loop state for a suspended app and re-spawns
// it. Returns an error if the app is not registered or not currently
// crash-looping.
func (s *Supervisor) Reset(id string) error {
	s.mu.Lock()
	app, exists := s.apps[id]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: app %q not found: %w", id, ErrNotFound)
	}
	if app.Status() != StatusCrashLooping {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: app %q not crash-looping (status=%s): %w",
			id, app.Status(), ErrNotCrashLooping)
	}

	// Wait for the prior watch goroutine to finish closing its done
	// channel. Status flipped to CrashLooping just before that watch
	// returned, so this is a very short wait — but it must happen
	// before we replace the channel, or the new watch's defer will
	// panic on a not-yet-closed-by-prior-watch channel.
	prevDone := app.waitDone()
	s.mu.Unlock()
	<-prevDone
	s.mu.Lock()

	// Verify the app is still registered and still crash-looping.
	if cur, ok := s.apps[id]; !ok || cur != app {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: app %q vanished during reset: %w", id, ErrNotFound)
	}
	if app.Status() != StatusCrashLooping {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: app %q not crash-looping (status=%s): %w",
			id, app.Status(), ErrNotCrashLooping)
	}

	app.clearRestarts()
	app.resetDone()

	if err := s.startLocked(app, app.env); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: reset start failed: %w", err)
	}
	s.mu.Unlock()

	go s.watch(app, app.env)
	if s.healthEnabled() {
		go s.probe(app, app.waitDone())
	}
	s.logger.Info("app reset; resuming", "id", id)
	return nil
}

// probe runs the health-check loop for one app. It exits when done is
// closed (i.e. when the watch goroutine has terminated). On
// HealthCheckFailureThreshold consecutive failures it SIGKILLs the
// current process; the existing watch logic observes the exit, records
// it as a crash, and restarts via the usual backoff path.
func (s *Supervisor) probe(app *App, done <-chan struct{}) {
	ticker := time.NewTicker(s.HealthCheckInterval)
	defer ticker.Stop()

	var failures int
	ready := false
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}

		// Only probe a running app. Skip while it is starting, crashed,
		// or in the middle of a restart cycle.
		if app.Status() != StatusRunning && app.Status() != StatusUnhealthy {
			failures = 0
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), s.HealthCheckTimeout)
		err := s.HealthChecker.Check(ctx, app)
		cancel()

		if err == nil {
			if !ready {
				ready = true
				s.emit(Event{
					Type:      EventReady,
					AppID:     app.ID,
					Status:    "ready",
					PID:       app.PID(),
					Port:      app.Port,
					URL:       fmt.Sprintf("http://127.0.0.1:%d", app.Port),
					Timestamp: time.Now(),
				})
				s.logger.Info("app ready", "id", app.ID, "port", app.Port)
			}
			if failures > 0 {
				s.logger.Info("app recovered", "id", app.ID, "previous_failures", failures)
				if app.Status() == StatusUnhealthy {
					app.setStatus(StatusRunning)
				}
			}
			failures = 0
			continue
		}

		failures++
		atomic.AddInt64(&app.healthFailures, 1)
		s.emit(Event{
			Type:           EventHealthFailure,
			AppID:          app.ID,
			HealthFailures: atomic.LoadInt64(&app.healthFailures),
			Timestamp:      time.Now(),
		})
		s.logger.Warn("health check failed",
			"id", app.ID,
			"attempt", failures,
			"threshold", s.HealthCheckFailureThreshold,
			"err", err,
		)

		if failures < s.HealthCheckFailureThreshold {
			app.setStatus(StatusUnhealthy)
			continue
		}

		s.logger.Error("health check threshold exceeded; killing for restart",
			"id", app.ID,
			"failures", failures,
		)
		cmd := app.snapshotCmd()
		if cmd != nil && cmd.Process != nil {
			if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
				s.logger.Debug("SIGKILL after unhealthy failed", "id", app.ID, "err", err)
			}
		}
		// Reset the counter; the watch goroutine will handle restart
		// (and crash-loop bookkeeping). Probe will resume once the new
		// process reaches StatusRunning.
		failures = 0
	}
}

// AppLogPath returns the absolute path of the per-app log file when
// LogDir is configured, or "" when log capture is disabled. The path
// is computed from LogDir + appID and is valid whether or not the
// underlying file currently exists.
func (s *Supervisor) AppLogPath(appID string) string {
	if s.LogDir == "" || appID == "" {
		return ""
	}
	return filepath.Join(s.LogDir, appID, "current.log")
}

// Restart cycles the named app's process in place: the current child
// receives SIGTERM, the existing watch goroutine observes the exit
// and applies its standard restart path (same Command/Args/Env/Port,
// same App identity), and Restart blocks until a new PID is in
// StatusRunning or timeout elapses.
//
// Unlike Deploy, Restart does NOT take a Router — the appID's
// dispatch route stays pointed at the same App pointer the whole
// time, so traffic is briefly absorbed by the OS during the gap
// between SIGTERM and the new process binding. The same backoff and
// crash-loop accounting that govern an organic crash apply here, so
// repeated forced restarts can eventually trip the crash-loop
// suspension just like a flapping app.
//
// timeout caps the wait for the new PID; zero uses a 10s default.
// Note that the active backoff stacks across restarts within
// RestartWindow — a fresh app restarts almost instantly, but the
// third restart in quick succession may wait 4s before the new
// process appears, so timeout must be generous enough to absorb the
// backoff at the current restart_count or Restart will return a
// timeout error even though the supervisor will still complete the
// relaunch in the background.
func (s *Supervisor) Restart(id string, timeout time.Duration) (*App, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	s.mu.RLock()
	app, ok := s.apps[id]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("supervisor: app %q not found: %w", id, ErrNotFound)
	}

	cmd := app.snapshotCmd()
	if cmd == nil || cmd.Process == nil {
		return nil, fmt.Errorf("supervisor: app %q has no running process", id)
	}
	oldPID := app.PID()

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return nil, fmt.Errorf("supervisor: restart signal: %w", err)
	}

	// Poll for new PID at StatusRunning. The watch goroutine's existing
	// backoff path drives the relaunch; we just wait it out.
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !s.isTracked(app) {
			return nil, fmt.Errorf("supervisor: app %q removed during restart", id)
		}
		switch app.Status() {
		case StatusCrashLooping:
			return nil, fmt.Errorf("supervisor: app %q entered crash-loop during restart: %w",
				id, ErrNotCrashLooping)
		case StatusRunning:
			if app.PID() != oldPID {
				return app, nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return app, fmt.Errorf("supervisor: restart of %q timed out after %v", id, timeout)
}

// Stop gracefully stops the named app: SIGTERM, wait up to
// GracefulShutdownTimeout for exit, then SIGKILL if still alive.
// Blocks until the app's watch goroutine has fully terminated.
func (s *Supervisor) Stop(id string) error {
	return s.StopWithTimeout(id, s.GracefulShutdownTimeout)
}

// StopWithTimeout is like Stop but with a caller-specified timeout for
// graceful exit. If the timeout elapses before the process exits, the
// supervisor sends SIGKILL and waits for the watch goroutine to
// terminate.
func (s *Supervisor) StopWithTimeout(id string, timeout time.Duration) error {
	s.mu.Lock()
	app, exists := s.apps[id]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: app %q not running: %w", id, ErrNotFound)
	}
	delete(s.apps, id)
	s.mu.Unlock()
	return s.stopApp(app, timeout)
}

// stopApp gracefully terminates the given App. Caller is responsible
// for having removed the App from the registry (or never registering
// it) before invoking — stopApp does no registry bookkeeping. This is
// the inner half of StopWithTimeout and is reused by Deploy to wind
// down the v1 process after the registry swap.
func (s *Supervisor) stopApp(app *App, timeout time.Duration) error {
	// Close the log rotator after the watch goroutine has finished
	// draining the child's pipes (signalled by done close), then
	// remove the per-app cgroup directory and tear down the network
	// namespace + veth pair. All best-effort — failures are logged
	// but don't fail the stop.
	defer func() {
		if app.rotator != nil {
			if err := app.rotator.Close(); err != nil {
				s.logger.Warn("log rotator close failed", "id", app.ID, "err", err)
			}
		}
		if app.cg != nil {
			if err := app.cg.Remove(); err != nil {
				s.logger.Warn("cgroup remove failed", "id", app.ID, "err", err)
			}
		}
		s.teardownAppNetwork(app)
	}()

	cmd := app.snapshotCmd()
	done := app.waitDone()
	if cmd == nil || cmd.Process == nil {
		// Process never started; watch may already have exited.
		// Wait briefly for done to close in case watch is mid-cleanup.
		select {
		case <-done:
		case <-time.After(1 * time.Second):
		}
		return nil
	}

	// Phase 1: SIGTERM and wait for graceful exit.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		s.logger.Debug("SIGTERM failed (process likely gone)",
			"id", app.ID, "err", err,
		)
	}

	s.logger.Info("app stop requested",
		"id", app.ID,
		"timeout", timeout,
	)

	if timeout <= 0 {
		// No graceful window — proceed straight to SIGKILL.
		return s.escalateAndWait(app.ID, cmd, done)
	}

	select {
	case <-done:
		s.logger.Info("app stopped gracefully", "id", app.ID)
		return nil
	case <-time.After(timeout):
		s.logger.Warn("graceful shutdown timeout; escalating to SIGKILL",
			"id", app.ID, "timeout", timeout,
		)
		return s.escalateAndWait(app.ID, cmd, done)
	}
}

// escalateAndWait sends SIGKILL and blocks until the watch goroutine
// closes done. Used when graceful exit fails or is bypassed.
func (s *Supervisor) escalateAndWait(id string, cmd *exec.Cmd, done <-chan struct{}) error {
	if cmd.Process != nil {
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			s.logger.Debug("SIGKILL failed (process likely gone)",
				"id", id, "err", err,
			)
		}
	}
	<-done
	s.logger.Info("app stopped (SIGKILL)", "id", id)
	return nil
}

// StopAll gracefully stops every supervised app concurrently. Honours
// the context deadline: if ctx has a deadline, each app gets at most
// the remaining time as its graceful window before SIGKILL escalation.
func (s *Supervisor) StopAll(ctx context.Context) {
	s.mu.RLock()
	ids := make([]string, 0, len(s.apps))
	for id := range s.apps {
		ids = append(ids, id)
	}
	s.mu.RUnlock()

	timeout := s.GracefulShutdownTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < timeout {
			timeout = remaining
		}
	}
	if timeout < 0 {
		timeout = 0
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := s.StopWithTimeout(id, timeout); err != nil {
				s.logger.Warn("stop during shutdown failed", "id", id, "err", err)
			}
		}(id)
	}
	wg.Wait()
}

// List returns a snapshot of currently registered apps.
func (s *Supervisor) List() []*App {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*App, 0, len(s.apps))
	for _, a := range s.apps {
		out = append(out, a)
	}
	return out
}

// Get returns the named app, or nil if not registered.
func (s *Supervisor) Get(id string) *App {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.apps[id]
}

// Errors returned by the supervisor package.
var (
	ErrAlreadyRunning  = errors.New("app already running")
	ErrNotFound        = errors.New("app not found")
	ErrNotCrashLooping = errors.New("app not in crash-loop state")
	ErrPortConflict    = errors.New("port conflict")
	ErrDeployConflict  = errors.New("deploy: concurrent change detected")
	ErrDeployUnhealthy = errors.New("deploy: v2 never became healthy")
)

// DeployConfig describes a blue-green replacement of an existing app.
// Config.ID identifies the app being replaced; Config.Port is v2's new
// port (must differ from v1's). The supervisor spawns v2 alongside v1,
// waits up to ReadyTimeout for v2 to pass health checks, atomically
// flips the registry + dispatch route, and gracefully stops v1.
type DeployConfig struct {
	Config
	// ReadyTimeout caps how long Deploy waits for v2 to pass its first
	// health check. Default 30s. The HealthChecker configured on the
	// Supervisor is used. If no HealthChecker is set, Deploy assumes
	// v2 is healthy after a brief settle.
	ReadyTimeout time.Duration
	// PollInterval is how often Deploy retries the health check while
	// waiting. Default 200ms.
	PollInterval time.Duration
	// GracefulV1Timeout caps the SIGTERM-then-SIGKILL window for the
	// retired v1. Defaults to s.GracefulShutdownTimeout.
	GracefulV1Timeout time.Duration
}

// deployTempID returns the registry key used for v2 during the
// deployment window. Exposed so tests can assert intermediate state.
func deployTempID(id string) string { return id + "__deploying" }

// Deploy performs a blue-green replacement of the app named cfg.ID:
//
//  1. Spawn v2 on cfg.Port under a temporary registry key.
//  2. Poll the supervisor's HealthChecker until v2 passes or
//     ReadyTimeout elapses. On timeout: kill v2 and return
//     ErrDeployUnhealthy; v1 is untouched.
//  3. Atomically remove v1 from the registry, rename v2's registry
//     key to cfg.ID, and call router.Set(cfg.ID, v2.Port).
//  4. Gracefully stop v1 (SIGTERM → SIGKILL escalation).
//
// On success returns the new App (v2). Router may be nil — in that
// case the route flip is skipped and Deploy is purely a process
// replacement.
//
// Deploy holds no supervisor-wide lock for the duration; only short
// critical sections during registry mutation. v1's own probe and
// watch goroutines continue running until Deploy stops it in step 4.
func (s *Supervisor) Deploy(ctx context.Context, router *dispatch.Router, cfg DeployConfig) (*App, error) {
	if cfg.ID == "" {
		return nil, errors.New("deploy: empty app id")
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 30 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 200 * time.Millisecond
	}
	if cfg.GracefulV1Timeout <= 0 {
		cfg.GracefulV1Timeout = s.GracefulShutdownTimeout
	}

	// Snapshot v1.
	s.mu.RLock()
	v1, ok := s.apps[cfg.ID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("deploy: app %q not found: %w", cfg.ID, ErrNotFound)
	}
	if cfg.Port == v1.Port {
		return nil, fmt.Errorf("deploy: v2 port %d must differ from v1: %w", cfg.Port, ErrPortConflict)
	}

	// Spawn v2 under a temp ID so it has its own registry slot,
	// watch goroutine, and pipe drainage. The same temp ID is what
	// we'll use to clean up if v2 turns out unhealthy. tempID
	// deliberately contains "__" (deployTempID adds the suffix) so
	// it fails ValidateID — go through spawnUnchecked instead.
	tempID := deployTempID(cfg.ID)
	v2Cfg := cfg.Config
	v2Cfg.ID = tempID
	v2, err := s.spawnUnchecked(v2Cfg)
	if err != nil {
		return nil, fmt.Errorf("deploy: spawn v2: %w", err)
	}

	// Wait for v2 to become healthy. If the deadline elapses, kill v2
	// (Stop removes it from the registry) and surface a sentinel error.
	if err := s.waitDeployHealthy(ctx, v2, cfg); err != nil {
		_ = s.Stop(tempID)
		return nil, err
	}

	// Atomic registry swap: ensure v1 is still the one we snapshotted,
	// then rename v2's registry key from tempID to cfg.ID.
	s.mu.Lock()
	if cur, ok := s.apps[cfg.ID]; !ok || cur != v1 {
		s.mu.Unlock()
		_ = s.Stop(tempID)
		return nil, ErrDeployConflict
	}
	delete(s.apps, tempID)
	delete(s.apps, cfg.ID)
	v2.renameUnderLock(cfg.ID)
	s.apps[cfg.ID] = v2
	s.mu.Unlock()

	// Flip the route. We do this AFTER the registry swap so that any
	// admin-API list or get call observes the new app at the canonical
	// ID before traffic shifts.
	//
	// SetAddr (not Set) so the v2 NetIP propagates for net-isolated
	// apps. Set defaults the host to 127.0.0.1 — fine for shared-
	// network apps but wrong for net-iso, where the listener lives in
	// a private netns at v2.NetIP. Pre-fix, deploys of net-iso apps
	// silently routed traffic to the host loopback (no listener).
	if router != nil {
		host := ""
		if v2.NetIP != nil {
			host = v2.NetIP.String()
		}
		if err := router.SetAddr(cfg.ID, host, v2.Port); err != nil {
			// Router refused (e.g., bad port). Roll back: put v1 back,
			// drop v2. This is best-effort — failures here mean a
			// half-deployed state, so we log loudly.
			s.logger.Error("deploy: router set failed; attempting rollback",
				"id", cfg.ID, "err", err)
			s.mu.Lock()
			delete(s.apps, cfg.ID)
			s.apps[cfg.ID] = v1
			s.mu.Unlock()
			_ = s.stopApp(v2, cfg.GracefulV1Timeout)
			return nil, fmt.Errorf("deploy: router.SetAddr: %w", err)
		}
	}

	// v1 is no longer in the registry; wind it down directly. Logs
	// failure but does not undo the deploy — v2 is now authoritative.
	if err := s.stopApp(v1, cfg.GracefulV1Timeout); err != nil {
		s.logger.Warn("deploy: v1 stop failed", "id", cfg.ID, "err", err)
	}

	s.logger.Info("deploy complete",
		"id", cfg.ID,
		"old_pid", v1.PID(),
		"new_pid", v2.PID(),
		"new_port", v2.Port,
	)
	return v2, nil
}

// waitDeployHealthy polls the supervisor's HealthChecker against app
// until it returns nil, ReadyTimeout elapses, or ctx is cancelled.
// When no HealthChecker is configured the function settles briefly
// and returns nil — the caller is treating "process started" as
// healthy.
func (s *Supervisor) waitDeployHealthy(ctx context.Context, app *App, cfg DeployConfig) error {
	if s.HealthChecker == nil {
		// Best-effort settle so the OS finishes setting up the socket.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		return nil
	}

	checkTimeout := s.HealthCheckTimeout
	if checkTimeout <= 0 {
		checkTimeout = 2 * time.Second
	}
	deadline := time.Now().Add(cfg.ReadyTimeout)
	var lastErr error
	for {
		checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
		err := s.HealthChecker.Check(checkCtx, app)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: last error: %v", ErrDeployUnhealthy, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.PollInterval):
		}
	}
}
