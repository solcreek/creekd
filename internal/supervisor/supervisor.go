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
	StatusStopped // terminal: removed from registry, will not restart
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
// Exported fields are safe to read while the app is alive. Internal
// fields are guarded by the supervisor's mu.
type App struct {
	ID      string
	Command string
	Args    []string
	Port    int

	cmd       *exec.Cmd
	status    Status
	startedAt time.Time
}

// PID returns the OS process id, or 0 if the process is not running.
func (a *App) PID() int {
	if a.cmd == nil || a.cmd.Process == nil {
		return 0
	}
	return a.cmd.Process.Pid
}

// Status returns the current lifecycle state.
func (a *App) Status() Status { return a.status }

// Uptime returns time since the current process started.
func (a *App) Uptime() time.Duration {
	if a.startedAt.IsZero() {
		return 0
	}
	return time.Since(a.startedAt)
}

// Supervisor owns the registry of running apps and the goroutines that
// watch them.
type Supervisor struct {
	mu     sync.RWMutex
	apps   map[string]*App
	logger *slog.Logger

	// restartDelay is the time waited before re-spawning a crashed app.
	// M5.1 uses a fixed 100 ms; M5.2 will replace this with exponential
	// backoff + crash-loop detection.
	restartDelay time.Duration
}

// New returns a fresh Supervisor.
func New(logger *slog.Logger) *Supervisor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{
		apps:         make(map[string]*App),
		logger:       logger,
		restartDelay: 100 * time.Millisecond,
	}
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

// startLocked spawns the OS process. Caller must hold s.mu.
func (s *Supervisor) startLocked(app *App, extraEnv []string) error {
	app.status = StatusStarting

	cmd := exec.Command(app.Command, app.Args...)
	env := append(os.Environ(), fmt.Sprintf("PORT=%d", app.Port))
	if len(extraEnv) > 0 {
		env = append(env, extraEnv...)
	}
	cmd.Env = env

	// M5.6 will replace these with structured log capture. For M5.1 we
	// forward the child's output to creekd's stdout/stderr so manual
	// testing works.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		app.status = StatusCrashed
		return fmt.Errorf("supervisor: starting %q: %w", app.ID, err)
	}

	app.cmd = cmd
	app.status = StatusRunning
	app.startedAt = time.Now()

	s.logger.Info("app spawned",
		"id", app.ID,
		"pid", cmd.Process.Pid,
		"port", app.Port,
	)
	return nil
}

// watch blocks on cmd.Wait() and restarts the process on exit until the
// app is explicitly Stop'd. Runs in its own goroutine.
func (s *Supervisor) watch(app *App, extraEnv []string) {
	for {
		err := app.cmd.Wait()
		exitCode := -1
		if app.cmd.ProcessState != nil {
			exitCode = app.cmd.ProcessState.ExitCode()
		}

		s.mu.Lock()
		// If Stop() removed this app from the registry while the
		// process was running, treat exit as terminal.
		if _, tracked := s.apps[app.ID]; !tracked {
			app.status = StatusStopped
			s.mu.Unlock()
			s.logger.Info("app exited (stopped)",
				"id", app.ID,
				"exit_code", exitCode,
			)
			return
		}

		app.status = StatusCrashed
		s.logger.Info("app exited; restarting",
			"id", app.ID,
			"exit_code", exitCode,
			"err", err,
			"delay", s.restartDelay,
		)
		s.mu.Unlock()

		time.Sleep(s.restartDelay)

		s.mu.Lock()
		// Re-check tracking after the delay — Stop() may have raced.
		if _, tracked := s.apps[app.ID]; !tracked {
			app.status = StatusStopped
			s.mu.Unlock()
			return
		}

		if err := s.startLocked(app, extraEnv); err != nil {
			s.logger.Error("restart failed",
				"id", app.ID,
				"err", err,
			)
			delete(s.apps, app.ID)
			app.status = StatusStopped
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		// Loop and Wait() on the new process.
	}
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
	cmd := app.cmd
	s.mu.Unlock()

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
	ErrAlreadyRunning = errors.New("app already running")
	ErrNotFound       = errors.New("app not found")
)
