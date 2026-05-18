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
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
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
	default:
		return "unknown"
	}
}

// Config describes how to spawn a supervised app.
//
// M5.4 will add Runtime to select bun / node / deno explicitly; for now
// the supervisor assumes the Entry command is directly executable
// (typically "bun some-file.ts").
type Config struct {
	ID      string
	Command string   // executable, e.g. "bun"
	Args    []string // arguments, e.g. ["server.ts"]
	Port    int      // assigned dispatch port, passed as PORT env var
	Env     []string // additional environment variables
}

// App is one supervised application instance.
//
// Exported fields (ID, Command, Args, Port) are immutable after Spawn
// and safe to read without locking. Mutable runtime state (cmd, status,
// startedAt, restarts) is guarded by App.mu; access via the accessor
// methods.
type App struct {
	ID      string
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
}

// New returns a fresh Supervisor with production defaults.
func New(logger *slog.Logger) *Supervisor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{
		apps:               make(map[string]*App),
		logger:             logger,
		InitialBackoff:     1 * time.Second,
		MaxBackoff:         30 * time.Second,
		RestartWindow:      60 * time.Second,
		CrashLoopThreshold: 5,
		Stdout:             os.Stdout,
		Stderr:             os.Stderr,
		WaitDelay:          0,
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
	if cfg.Command == "" {
		return nil, errors.New("supervisor: empty command")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.apps[cfg.ID]; exists {
		return nil, fmt.Errorf("supervisor: app %q already running: %w", cfg.ID, ErrAlreadyRunning)
	}

	app := &App{
		ID:      cfg.ID,
		Command: cfg.Command,
		Args:    cfg.Args,
		Port:    cfg.Port,
	}

	if err := s.startLocked(app, cfg.Env); err != nil {
		return nil, err
	}

	s.apps[cfg.ID] = app
	go s.watch(app, cfg.Env)
	return app, nil
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
func (s *Supervisor) watch(app *App, extraEnv []string) {
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

	app.clearRestarts()

	if err := s.startLocked(app, nil); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: reset start failed: %w", err)
	}
	s.mu.Unlock()

	go s.watch(app, nil)
	s.logger.Info("app reset; resuming", "id", id)
	return nil
}

// Stop signals the named app to exit and removes it from the registry.
// The watch goroutine will observe the de-registration and not restart.
//
// M5.3 will add graceful shutdown (SIGTERM → drain → SIGKILL fallback).
// For M5.1 we send SIGTERM and return immediately.
func (s *Supervisor) Stop(id string) error {
	s.mu.Lock()
	app, exists := s.apps[id]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: app %q not running: %w", id, ErrNotFound)
	}
	delete(s.apps, id)
	s.mu.Unlock()

	cmd := app.snapshotCmd()
	if cmd != nil && cmd.Process != nil {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			// Process may already be dead; that's OK.
			s.logger.Debug("signal failed (process likely gone)",
				"id", id,
				"err", err,
			)
		}
	}

	s.logger.Info("app stop requested", "id", id)
	return nil
}

// StopAll signals every supervised app to exit. Used during shutdown.
func (s *Supervisor) StopAll(ctx context.Context) {
	s.mu.RLock()
	ids := make([]string, 0, len(s.apps))
	for id := range s.apps {
		ids = append(ids, id)
	}
	s.mu.RUnlock()

	for _, id := range ids {
		if err := s.Stop(id); err != nil {
			s.logger.Warn("stop during shutdown failed", "id", id, "err", err)
		}
	}
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
