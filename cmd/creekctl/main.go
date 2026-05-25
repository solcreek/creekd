// Package main is the entry point for the creekctl binary — the
// admin CLI for self-hosted creekd platforms.
//
// creekctl is the platform-operator counterpart to the tenant-facing
// creek CLI. It speaks the creekd admin HTTP/JSON API directly and
// only exposes operations that an operator (not an app developer)
// would perform: listing every app on the host, spawning / removing
// processes, blue-green deploying a new version, restarting an app,
// clearing a crash-loop, and tailing per-app logs.
//
// Connection knobs (each can also be supplied as a flag):
//
//	CREEKCTL_SERVER  base URL (default http://127.0.0.1:9080)
//	CREEKCTL_TOKEN   bearer token; required when creekd was started
//	                  with CREEKD_ADMIN_TOKEN set
//
// Output format is human-readable by default; pass --json to any
// subcommand for the raw AppView (or array thereof) suitable for
// piping to jq.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// version is stamped at build time via -ldflags '-X main.version=...'.
// Falls back to "0.0.0-dev" for plain `go build`.
var version = "0.0.0-dev"

func main() {
	if len(os.Args) < 2 {
		usageAndExit(2)
	}
	sub := os.Args[1]
	rest := os.Args[2:]

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cmd, ok := subcommands[sub]
	if !ok {
		switch sub {
		case "-h", "--help", "help":
			usageAndExit(0)
		case "version", "-v", "--version":
			fmt.Println(version)
			return
		default:
			fmt.Fprintf(os.Stderr, "creekctl: unknown command %q\n\n", sub)
			usageAndExit(2)
		}
		return
	}
	if err := cmd.Run(ctx, os.Stdout, rest); err != nil {
		fmt.Fprintf(os.Stderr, "creekctl %s: %v\n", sub, err)
		os.Exit(1)
	}
}

// usageAndExit prints the top-level help and exits with code.
func usageAndExit(code int) {
	fmt.Fprintln(os.Stderr, `creekctl — admin CLI for self-hosted creekd platforms.

Usage:
  creekctl <command> [flags]

Commands:
  ps                 list all apps on the host
  get <id>           show one app
  up <id>            spawn a new app (flags: --command, --port, ...)
  ensure <id>        idempotent spawn (create if absent, no-op if running)
  rm <id>            stop and de-register an app
  restart <id>       cycle an app's process in place
  reset <id>         clear crash-loop and resume
  deploy <id>        blue-green deploy a new version (flags: --port, ...)
  logs <id>          tail the per-app log (--tail N)
  events <id>        stream app state transitions (SSE)
  stats <id>         show cgroup-tracked resource counters
  exec -- <cmd>      run a one-off command with app env vars
  db-reset           drop and recreate app database (sandbox)
  describe [cmd]     introspect command schema as JSON (agent-facing)
  hardening-check    validate creekd.service against the hardening set
  version            print version
  help               this message

Global flags (also accepted as env vars):
  --server URL        admin API base (CREEKCTL_SERVER, default 127.0.0.1:9080)
  --token TOKEN       bearer token (CREEKCTL_TOKEN)
  --json              machine-readable output (auto-enabled when stdout is not a TTY)
  --dry-run           validate inputs without executing (mutating commands)

Run "creekctl <command> --help" for the flags each subcommand accepts.`)
	os.Exit(code)
}
