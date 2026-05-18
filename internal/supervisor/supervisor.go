// Package supervisor manages the lifecycle of child application
// processes.
//
// M5.1 — child-process spawn + basic supervision
//
// Each app is spawned as an isolated child process. A goroutine watches
// each child via cmd.Wait(); on exit the supervisor logs the reason and
// performs a naive immediate restart (after a 100 ms delay to avoid
// tight loops). Stop() removes the app from the registry, so its watch
// goroutine will not restart it.
//
// Restart policy hardening (exponential backoff, crash-loop detection)
// is M5.2. Health probing and graceful shutdown drain are M5.3.
// Multi-runtime dispatch is M5.4. This package contains only what M5.1
// promises.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/solcreek/creekd/internal/runtime"
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
	Command string   // executable, e.g. "bun"
	Args    []string // arguments, e.g. ["server.ts"]
	Runtime runtime.Runtime // M5.4: "bun" | "node" | "deno"
	Entry   string   // M5.4: entry script for Runtime resolution
	Port    int      // assigned dispatch port, passed as PORT env var
	Env     []string // additional environment variables
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
}

// HealthFailures returns the cumulative count of failed health probes
// since this App was spawned.
func (a *App) HealthFailures() int64 {
	return atomic.LoadInt64(&a.healthFailures)
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

// setStatus updates just the status under App.mu.
func (a *App) setStatus(status Status) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status = status
}

// snapshotCmd returns the current exec.Cmd reference under lock so the
// caller can wait/signal without racing with restarts.
func (a *App) snapshotCmd() *exec.Cmd {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cmd
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

	// Stdout / Stderr are inherited by each child process. Production
	// uses os.Stdout / os.Stderr (M5.6 will replace with per-app log
	// capture). Tests should set to io.Discard to avoid leaking pipes
	// into the test process.
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
}

// HealthChecker probes a running supervised app and returns nil if it
// is healthy. M5.3b uses the result to decide whether to escalate to a
// process restart.
type HealthChecker interface {
	Check(ctx context.Context, app *App) error
}

// HTTPHealthChecker performs `GET http://127.0.0.1:<port><Path>` and
// considers the app healthy iff the response status is 2xx.
type HTTPHealthChecker struct {
	Path   string
	Client *http.Client
}

// Check implements HealthChecker.
func (h *HTTPHealthChecker) Check(ctx context.Context, app *App) error {
	path := h.Path
	if path == "" {
		path = "/health"
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", app.Port, path)
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health: unexpected status %d", resp.StatusCode)
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
	}
}

// computeBackoff returns the delay before the (count+1)-th restart.
// count is the number of restarts already in this window.
// Sequence (with default settings): 1s, 2s, 4s, 8s, 16s, 30s, 30s, ...
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

// Spawn starts a new supervised app. Returns ErrAlreadyRunning if an
// app with the same ID is already in the registry.
func (s *Supervisor) Spawn(cfg Config) (*App, error) {
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
		ID:      cfg.ID,
		Runtime: rt,
		Command: cmd,
		Args:    args,
		Port:    cfg.Port,
		done:    make(chan struct{}),
	}

	if err := s.startLocked(app, cfg.Env); err != nil {
		return nil, err
	}

	s.apps[cfg.ID] = app
	go s.watch(app, cfg.Env)
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

// startLocked spawns the OS process. Caller must hold s.mu. Mutates
// app via setState/setStatus which take App.mu internally.
func (s *Supervisor) startLocked(app *App, extraEnv []string) error {
	app.setStatus(StatusStarting)

	cmd := exec.Command(app.Command, app.Args...)
	env := append(os.Environ(), fmt.Sprintf("PORT=%d", app.Port))
	if len(extraEnv) > 0 {
		env = append(env, extraEnv...)
	}
	cmd.Env = env

	// M5.6 will replace these with structured log capture. For M5.1-M5.2
	// the supervisor forwards the child's output to its configured
	// writers (default os.Stdout / os.Stderr; tests use io.Discard).
	cmd.Stdout = s.Stdout
	cmd.Stderr = s.Stderr
	cmd.WaitDelay = s.WaitDelay

	if err := cmd.Start(); err != nil {
		app.setStatus(StatusCrashed)
		return fmt.Errorf("supervisor: starting %q: %w", app.ID, err)
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
// Closes app.done when it terminates (for graceful shutdown wait).
func (s *Supervisor) watch(app *App, extraEnv []string) {
	defer close(app.done)

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
		// process was running, treat exit as terminal.
		s.mu.RLock()
		_, tracked := s.apps[app.ID]
		s.mu.RUnlock()
		if !tracked {
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
		// Re-check tracking after the delay — Stop() may have raced.
		if _, tracked := s.apps[app.ID]; !tracked {
			s.mu.Unlock()
			app.setStatus(StatusStopped)
			return
		}

		if err := s.startLocked(app, extraEnv); err != nil {
			s.logger.Error("restart failed",
				"id", app.ID,
				"err", err,
			)
			delete(s.apps, app.ID)
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

	if err := s.startLocked(app, nil); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: reset start failed: %w", err)
	}
	s.mu.Unlock()

	go s.watch(app, nil)
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
			"id", id, "err", err,
		)
	}

	s.logger.Info("app stop requested",
		"id", id,
		"timeout", timeout,
	)

	if timeout <= 0 {
		// No graceful window — proceed straight to SIGKILL.
		return s.escalateAndWait(id, cmd, done)
	}

	select {
	case <-done:
		s.logger.Info("app stopped gracefully", "id", id)
		return nil
	case <-time.After(timeout):
		s.logger.Warn("graceful shutdown timeout; escalating to SIGKILL",
			"id", id, "timeout", timeout,
		)
		return s.escalateAndWait(id, cmd, done)
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
)
