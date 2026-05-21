package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/solcreek/creekd/internal/creektoml"
	"github.com/solcreek/creekd/internal/lima"
)

type sandboxStatus struct {
	VM         string   `json:"vm"`
	Status     string   `json:"status"`
	Primitives []string `json:"primitives"`
	Ports      []port   `json:"ports"`
}

type port struct {
	Name  string `json:"name"`
	Guest int    `json:"guest"`
	Host  int    `json:"host"`
}

func runSandbox(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("sandbox", flag.ContinueOnError)
	var (
		autoInstall bool
		jsonOutput  bool
		stop        bool
		destroy     bool
	)
	fs.BoolVar(&autoInstall, "auto-install", false, "install Lima without prompting")
	fs.BoolVar(&jsonOutput, "json", false, "machine-readable output")
	fs.BoolVar(&stop, "stop", false, "stop the sandbox VM")
	fs.BoolVar(&destroy, "destroy", false, "destroy the sandbox VM")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: creekd sandbox [flags] [app-directory]\n\n")
		fmt.Fprintf(os.Stderr, "Provision a local development environment with real primitives\n")
		fmt.Fprintf(os.Stderr, "inside a Lima VM. Reads creek.toml to determine which primitives\n")
		fmt.Fprintf(os.Stderr, "to provision (database, cache, runtime).\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	appDir := fs.Arg(0)
	if appDir == "" {
		appDir = "."
	}
	appDir, _ = filepath.Abs(appDir)

	vm := lima.NewVM(lima.DefaultVMName, logger)

	if destroy {
		return vm.Destroy()
	}
	if stop {
		return vm.Stop()
	}

	if !jsonOutput {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "creekd sandbox")
		fmt.Fprintln(os.Stderr, "━━━━━━━━━━━━━━")
	}

	cfg, err := creektoml.Discover(appDir)
	if err != nil {
		return fmt.Errorf("creek.toml not found in %s: %w", appDir, err)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	if !jsonOutput {
		logStatus("creek.toml loaded", cfg.App.Name)
	}

	if !lima.Available() {
		if autoInstall {
			return fmt.Errorf("Lima (limactl) not found. Install with: brew install lima")
		}
		fmt.Fprintln(os.Stderr, "  Lima not found. Install? (Y/n) ")
		reader := bufio.NewReader(os.Stdin)
		reply, _ := reader.ReadString('\n')
		reply = strings.TrimSpace(reply)
		if reply != "" && !strings.EqualFold(reply, "y") {
			return fmt.Errorf("Lima required for sandbox mode")
		}
		return fmt.Errorf("please install Lima first: brew install lima")
	}

	ver, _ := lima.Version()
	if !jsonOutput {
		logStatus("Lima", ver)
	}

	exists, err := vm.Exists()
	if err != nil {
		return err
	}
	if !exists {
		if !jsonOutput {
			fmt.Fprintln(os.Stderr, "  Creating sandbox VM (first time, ~2 min)...")
		}
		vmCfg := lima.DefaultConfig()
		for _, p := range cfg.RequiredPrimitives() {
			vmCfg.AddPrimitive(p)
		}
		tmpFile := filepath.Join(os.TempDir(), "creek-sandbox.yaml")
		if err := vmCfg.WriteTo(tmpFile); err != nil {
			return err
		}
		defer os.Remove(tmpFile)
		if err := vm.Create(tmpFile); err != nil {
			return err
		}
		if !jsonOutput {
			logStatus("VM ready", "")
		}
	} else {
		running, err := vm.Running()
		if err != nil {
			return err
		}
		if !running {
			if !jsonOutput {
				fmt.Fprintln(os.Stderr, "  Starting sandbox VM...")
			}
			if err := vm.Start(); err != nil {
				return err
			}
		}
		if !jsonOutput {
			logStatus("VM running", "")
		}
	}

	primitives := cfg.RequiredPrimitives()
	for _, p := range primitives {
		if !jsonOutput {
			fmt.Fprintf(os.Stderr, "    %s → ", p)
		}
		if err := lima.Provision(vm, p); err != nil {
			return err
		}
		if !jsonOutput {
			fmt.Fprintln(os.Stderr, "✓")
		}
	}

	status := buildStatus(vm.Name, primitives, cfg)

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Port mappings:")
	for _, p := range status.Ports {
		fmt.Fprintf(os.Stderr, "    %-12s localhost:%d\n", p.Name, p.Host)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Sandbox ready.")
	fmt.Fprintln(os.Stderr)

	return nil
}

func buildStatus(vmName string, primitives []string, cfg *creektoml.Config) sandboxStatus {
	status := sandboxStatus{
		VM:         vmName,
		Status:     "running",
		Primitives: primitives,
	}

	status.Ports = append(status.Ports, port{Name: "app", Guest: 3000, Host: 13000})

	if cfg.Database.Driver == "postgres" {
		status.Ports = append(status.Ports, port{Name: "postgres", Guest: 5432, Host: 15432})
	}
	if cfg.Cache.Driver == "redis" {
		status.Ports = append(status.Ports, port{Name: "redis", Guest: 6379, Host: 16379})
	}

	return status
}

func logStatus(label, detail string) {
	if detail != "" {
		fmt.Fprintf(os.Stderr, "  ✓ %s (%s)\n", label, detail)
	} else {
		fmt.Fprintf(os.Stderr, "  ✓ %s\n", label)
	}
}
