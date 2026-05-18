// Package main is the entry point for the creekd binary.
//
// creekd is the host-side supervisor for Creek applications. Each app
// runs as a child process; this binary spawns them, watches their
// lifecycle, routes HTTP traffic to them, and enforces resource limits.
//
// Phase 1 implementation status: skeleton only. See docs/ROADMAP.md.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

const version = "0.0.0-dev"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("creekd %s starting", version)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Trap SIGINT / SIGTERM for graceful shutdown.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		log.Printf("received %v, shutting down", sig)
		cancel()
	}()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "creekd: %v\n", err)
		os.Exit(1)
	}
	log.Printf("creekd stopped cleanly")
}

// run is the main daemon loop. M5.1 will replace this stub with actual
// supervisor logic; M5.2-M5.7 build on top.
func run(ctx context.Context) error {
	log.Printf("creekd skeleton; M5.1 supervisor not yet implemented")
	<-ctx.Done()
	return nil
}
