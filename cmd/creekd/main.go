// Package main is the entry point for the creekd binary.
//
// creekd is the host-side supervisor for Creek applications. Each app
// runs as a child process; this binary spawns them, watches their
// lifecycle, routes HTTP traffic to them, and enforces resource limits.
//
// Environment knobs (Phase 1):
//
//	CREEKD_ADMIN_ADDR    listen address for the admin HTTP/JSON API
//	                     (default 127.0.0.1:9080)
//	CREEKD_ADMIN_TOKEN   bearer token required on admin requests;
//	                     empty disables auth (only safe for loopback)
//	CREEKD_DISPATCH_ADDR listen address for the public dispatch proxy
//	                     (default 127.0.0.1:9000); empty disables it
//	CREEKD_LOG_DIR       per-app log capture root; empty forwards
//	                     child stdout/stderr to creekd's own writers
//	CREEKD_CGROUP_PARENT cgroup v2 slice owning per-app sub-cgroups;
//	                     empty disables cgroup enforcement
//	CREEKD_DEBUG_PPROF   "1" mounts /debug/pprof/* on the admin
//	                     listener, gated by the same bearer token
//	CREEKD_STATE_DIR     directory holding state.json (persisted app
//	                     set); empty disables persistence. When set,
//	                     creekd re-spawns every recorded app at
//	                     startup before the listeners open
//	CREEKD_NET_SUBNET    IPv4 CIDR for per-app network namespaces
//	                     (e.g. 10.42.0.0/24). Required when any app
//	                     uses --net-isolation; ignored otherwise.
//	CREEKD_NET_BRIDGE_NAME
//	                     bridge interface to attach per-app veth pairs
//	                     to (e.g. creekbr0). Pairs with CREEKD_NET_SUBNET;
//	                     both must be set together
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/solcreek/creekd/internal/adminapi"
	"github.com/solcreek/creekd/internal/dispatch"
	"github.com/solcreek/creekd/internal/state"
	"github.com/solcreek/creekd/internal/supervisor"
)

// version is stamped at build time via -ldflags '-X main.version=...'.
// Falls back to "0.0.0-dev" for plain `go build`.
var version = "0.0.0-dev"

// handleVersionFlag prints the build version to out and returns true
// when args[1] is one of --version / -v / version. Used by install.sh
// and operator tooling to identify a binary without booting the
// daemon. Without this, `creekd --version` falls through to the
// daemon path, binds ports, and hangs the caller's command
// substitution forever.
func handleVersionFlag(args []string, out io.Writer) bool {
	if len(args) < 2 {
		return false
	}
	switch args[1] {
	case "--version", "-v", "version":
		fmt.Fprintln(out, version)
		return true
	}
	return false
}

func main() {
	if handleVersionFlag(os.Args, os.Stdout) {
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	logger.Info("creekd starting", "version", version)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Trap SIGINT / SIGTERM for graceful shutdown.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		logger.Info("shutdown signal received", "signal", sig.String())
		cancel()
	}()

	if err := run(ctx, logger); err != nil {
		fmt.Fprintf(os.Stderr, "creekd: %v\n", err)
		os.Exit(1)
	}
	logger.Info("creekd stopped cleanly")
}

func run(ctx context.Context, logger *slog.Logger) error {
	sup := supervisor.New(logger)
	configureSupervisorFromEnv(sup)

	router := dispatch.NewRouter()

	// Load persisted state (if enabled) and replay each declared
	// app through Spawn so the platform survives creekd's own
	// restart. Failures per-app are logged and skipped — one
	// broken entry must not block the rest from coming back.
	var store *state.Store
	if dir := os.Getenv("CREEKD_STATE_DIR"); dir != "" {
		var err error
		store, err = state.NewStore(filepath.Join(dir, "state.json"))
		if err != nil {
			return fmt.Errorf("state: %w", err)
		}
		restored := restoreFromState(logger, sup, router, store)
		logger.Info("state restore complete",
			"declared", len(store.Apps()),
			"restored", restored,
		)
	}

	// Public dispatch listener (data plane).
	var dispatchSrv *http.Server
	if addr := envOr("CREEKD_DISPATCH_ADDR", "127.0.0.1:9000"); addr != "" {
		dispatchSrv = &http.Server{
			Addr:              addr,
			Handler:           router,
			ReadHeaderTimeout: 10 * time.Second,
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("dispatch listen %s: %w", addr, err)
		}
		go func() {
			logger.Info("dispatch listening", "addr", ln.Addr().String())
			if err := dispatchSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("dispatch server error", "err", err)
			}
		}()
	}

	// Admin listener (control plane).
	adminAddr := envOr("CREEKD_ADMIN_ADDR", "127.0.0.1:9080")
	adminToken := os.Getenv("CREEKD_ADMIN_TOKEN")
	if adminToken == "" && !isLoopback(adminAddr) {
		return fmt.Errorf("admin: refusing to listen on %s without CREEKD_ADMIN_TOKEN", adminAddr)
	}
	adminServer := adminapi.New(sup, router, adminToken)
	if store != nil {
		adminServer.SetStore(store)
	}
	if os.Getenv("CREEKD_DEBUG_PPROF") == "1" {
		adminServer.EnablePprof()
		logger.Info("pprof endpoints mounted",
			"prefix", "/debug/pprof/",
			"auth", adminToken != "",
		)
	}
	adminSrv := &http.Server{
		Addr:              adminAddr,
		Handler:           adminServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	adminLn, err := net.Listen("tcp", adminAddr)
	if err != nil {
		return fmt.Errorf("admin listen %s: %w", adminAddr, err)
	}
	go func() {
		logger.Info("admin api listening",
			"addr", adminLn.Addr().String(),
			"auth", adminToken != "",
		)
		if err := adminSrv.Serve(adminLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("admin server error", "err", err)
		}
	}()

	logger.Info("creekd ready")

	<-ctx.Done()
	logger.Info("stopping listeners + apps")

	// Drain listeners first so no new admin/data traffic lands while
	// supervised processes are winding down.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if dispatchSrv != nil {
		_ = dispatchSrv.Shutdown(shutdownCtx)
	}
	_ = adminSrv.Shutdown(shutdownCtx)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	sup.StopAll(stopCtx)

	// Give watch goroutines a moment to observe the stop.
	time.Sleep(200 * time.Millisecond)

	return nil
}

// configureSupervisorFromEnv copies CREEKD_* environment values
// onto sup. Extracted so the env → field plumbing is unit-testable
// without booting real listeners; keeping it out of run() keeps
// that function focused on listener wiring.
func configureSupervisorFromEnv(sup *supervisor.Supervisor) {
	if v := os.Getenv("CREEKD_LOG_DIR"); v != "" {
		sup.LogDir = v
	}
	if v := os.Getenv("CREEKD_CGROUP_PARENT"); v != "" {
		sup.CgroupParent = v
	}
	// Per-app network namespace requires both knobs. Either-one-set
	// is rejected at Spawn time; we don't pre-check here because the
	// daemon may run with no net-iso apps and that's fine.
	if v := os.Getenv("CREEKD_NET_SUBNET"); v != "" {
		sup.NetSubnet = v
	}
	if v := os.Getenv("CREEKD_NET_BRIDGE_NAME"); v != "" {
		sup.NetBridgeName = v
	}
}

// envOr returns os.Getenv(key) or fallback when empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// restoreFromState re-spawns every app recorded in store and
// re-registers each on the dispatch router. Per-app failures are
// logged and skipped so one corrupted entry can't block the rest.
// Returns the count of apps that came back up.
func restoreFromState(logger *slog.Logger, sup *supervisor.Supervisor,
	router *dispatch.Router, store *state.Store) int {
	restored := 0
	for _, cfg := range store.Apps() {
		// Defense in depth: reject obviously-malformed IDs from the
		// state file even though the daemon would never write them
		// itself. Protects against hand-edited / corrupted state.json
		// where an attacker-controlled ID could become a cgroup
		// slice name, log directory, or netns name.
		if err := supervisor.ValidateID(cfg.ID); err != nil {
			logger.Error("restore: skipping invalid id",
				"id", cfg.ID, "err", err,
			)
			continue
		}
		app, err := sup.Spawn(cfg)
		if err != nil {
			logger.Error("restore: spawn failed",
				"id", cfg.ID, "err", err,
			)
			continue
		}
		host := ""
		if app.NetIP != nil {
			host = app.NetIP.String()
		}
		if err := router.SetAddr(cfg.ID, host, cfg.Port); err != nil {
			logger.Error("restore: dispatch set failed",
				"id", cfg.ID, "err", err,
			)
			// Best-effort: tear down the app we just spawned so the
			// platform doesn't accumulate orphan registrations.
			_ = sup.Stop(cfg.ID)
			continue
		}
		restored++
		logger.Info("restored", "id", cfg.ID, "pid", app.PID(), "port", cfg.Port)
	}
	return restored
}

// isLoopback returns true if addr's host portion resolves to a
// loopback IP. Used to gate unauthenticated admin listeners.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Treat malformed addresses as non-loopback so we err on the
		// side of demanding a token.
		return false
	}
	if host == "" {
		// Empty host means "any interface" — not loopback.
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Hostnames: refuse to call them loopback without resolution.
		// "localhost" is the common case; accept it explicitly.
		return host == "localhost"
	}
	return ip.IsLoopback()
}
