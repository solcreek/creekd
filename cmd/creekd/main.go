// Package main is the entry point for the creekd binary.
//
// creekd is the host-side supervisor for Creek applications. Each app
// runs as a child process; this binary spawns them, watches their
// lifecycle, routes HTTP traffic to them, and enforces resource limits.
//
// M5.1 wires the supervisor package and exposes a manual smoke-test
// path via CREEK_SMOKE_TEST=1. The HTTP admin API and gRPC layer come
// in later sub-tasks.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/solcreek/creekd/internal/supervisor"
)

const version = "0.0.0-dev"

func main() {
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

	// M5.1 smoke-test path. When CREEK_SMOKE_TEST=1 we spawn a small
	// fleet of long-running children and demonstrate the supervisor
	// is working. M5.7 will replace this with the real deploy API.
	if os.Getenv("CREEK_SMOKE_TEST") == "1" {
		smokeTest(sup, logger)
	}

	logger.Info("creekd ready",
		"apps", len(sup.List()),
		"note", "HTTP admin API arrives in later sub-tasks",
	)

	<-ctx.Done()
	logger.Info("stopping all apps")
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sup.StopAll(stopCtx)

	// Give watch goroutines a moment to observe the stop.
	time.Sleep(200 * time.Millisecond)

	return nil
}

// smokeTest spawns a few example apps to exercise the supervisor on
// manual runs. Useful while M5.2-M5.7 are still under construction and
// there is no admin API yet.
func smokeTest(sup *supervisor.Supervisor, logger *slog.Logger) {
	count := 3
	if v := os.Getenv("CREEK_SMOKE_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			count = n
		}
	}
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("smoke-%d", i)
		app, err := sup.Spawn(supervisor.Config{
			ID:      id,
			Command: "sleep",
			Args:    []string{"3600"},
			Port:    9000 + i,
		})
		if err != nil {
			logger.Error("smoke spawn failed", "id", id, "err", err)
			continue
		}
		logger.Info("smoke app up",
			"id", app.ID,
			"pid", app.PID(),
			"port", app.Port,
		)
	}
}
