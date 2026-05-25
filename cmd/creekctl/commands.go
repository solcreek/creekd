package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/solcreek/creekd/api/manifest"
	"github.com/solcreek/creekd/internal/adminclient"
	"github.com/solcreek/creekd/internal/apitypes"
	"github.com/solcreek/creekd/internal/hardening"
	"github.com/solcreek/creekd/internal/upgrade"
)

// subcommand is one CLI verb. Run parses argv (minus the verb itself)
// and writes successful output to w. Returning an error causes a
// non-zero exit; main.go formats the error to stderr — handlers
// should not write to os.Stderr directly.
//
// Output goes to the injected io.Writer rather than os.Stdout so
// tests can capture it via a bytes.Buffer. This follows the Go
// community pattern popularised by cobra/spf13 and kubectl.
type subcommand struct {
	Name        string
	Description string
	Run         func(ctx context.Context, w io.Writer, argv []string) error
}

var subcommands = map[string]*subcommand{
	"ps":      {Name: "ps", Description: "list all apps", Run: runPS},
	"get":     {Name: "get", Description: "show one app", Run: runGet},
	"up":      {Name: "up", Description: "spawn a new app", Run: runUp},
	"ensure":  {Name: "ensure", Description: "idempotent spawn (create if absent, no-op if running)", Run: runEnsure},
	"rm":      {Name: "rm", Description: "stop an app", Run: runRM},
	"restart": {Name: "restart", Description: "cycle an app", Run: runRestart},
	"reset":   {Name: "reset", Description: "clear crash-loop", Run: runReset},
	"deploy":  {Name: "deploy", Description: "blue-green deploy", Run: runDeploy},
	"logs":    {Name: "logs", Description: "tail per-app log", Run: runLogs},
	"events":  {Name: "events", Description: "stream app state transitions (SSE)", Run: runEvents},
	"stats":   {Name: "stats", Description: "show resource counters", Run: runStats},
}

func init() {
	subcommands["describe"] = &subcommand{
		Name: "describe", Description: "introspect command schema (agent-facing)", Run: runDescribe,
	}
	subcommands["db-reset"] = &subcommand{
		Name: "db-reset", Description: "drop and recreate app database (sandbox)", Run: runDBReset,
	}
	subcommands["exec"] = &subcommand{
		Name: "exec", Description: "run a one-off command with app env vars", Run: runExec,
	}
	subcommands["hardening-check"] = &subcommand{
		Name: "hardening-check", Description: "validate creekd.service against the canonical hardening set", Run: runHardeningCheck,
	}
	subcommands["self-upgrade"] = &subcommand{
		Name: "self-upgrade", Description: "verify + replace creekd/creekctl binaries from a GitHub release", Run: runSelfUpgrade,
	}
}

// commonFlags holds the global knobs every subcommand accepts. They
// are registered on each subcommand's flag set rather than parsed
// globally so a typo before the subcommand still produces a clear
// usage message at that subcommand's scope.
type commonFlags struct {
	server string
	token  string
	json   bool
	dryRun bool
}

// register attaches --server / --token / --json / --dry-run onto fs
// and seeds defaults from environment.
func (cf *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&cf.server, "server", os.Getenv("CREEKCTL_SERVER"),
		"admin API base URL (or $CREEKCTL_SERVER)")
	fs.StringVar(&cf.token, "token", os.Getenv("CREEKCTL_TOKEN"),
		"bearer token (or $CREEKCTL_TOKEN)")
	fs.BoolVar(&cf.json, "json", false, "JSON output")
	fs.BoolVar(&cf.dryRun, "dry-run", false, "validate inputs without executing")
}

// resolveJSON enables JSON output when --json is set, OUTPUT_FORMAT=json
// is in the environment, or NO_TTY=1 is set (for agent pipelines).
func (cf *commonFlags) resolveJSON() {
	if cf.json {
		return
	}
	if v := os.Getenv("OUTPUT_FORMAT"); strings.EqualFold(v, "json") {
		cf.json = true
		return
	}
	if os.Getenv("NO_TTY") == "1" {
		cf.json = true
	}
}

// client returns a configured adminclient ready to call.
func (cf *commonFlags) client() *adminclient.Client {
	return adminclient.New(adminclient.Config{Server: cf.server, Token: cf.token})
}

// newFlagSet returns a FlagSet that prints the subcommand-specific
// usage on -h / --help.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// limitsFlags registers the cgroup-limit flags shared by `up` and
// `deploy`. Values are stored as strings so the empty case can be
// distinguished from "0" and parsing failures surface at command
// time, not flag-parse time (Go's flag package can't easily
// report parse errors with custom logic).
type limitsFlags struct {
	memoryHigh string
	memoryMax  string
	pidsMax    int64
	cpuQuota   int64
	cpuPeriod  int64
}

func (lf *limitsFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&lf.memoryHigh, "memory-high", "",
		"soft memory cap (e.g. 256M, 1G). Triggers kernel throttle + reclaim without OOM-kill. Preferred for noisy-neighbor protection. 0/empty = no soft cap")
	fs.StringVar(&lf.memoryMax, "memory-max", "",
		"hard memory cap (e.g. 256M, 1G). Includes swap. Triggers OOM-kill on overrun. 0/empty = unlimited")
	fs.Int64Var(&lf.pidsMax, "pids-max", 0,
		"max number of tasks in the cgroup. 0 = unlimited")
	fs.Int64Var(&lf.cpuQuota, "cpu-quota-us", 0,
		"CPU quota in microseconds per --cpu-period-us. 0 = unlimited")
	fs.Int64Var(&lf.cpuPeriod, "cpu-period-us", 0,
		"CPU accounting period in microseconds. 0 = cgroup default (100ms)")
}

// toAPI returns the wire-format Limits, or nil when every field is
// zero/unset (which the API treats as "no cgroup at all").
func (lf *limitsFlags) toAPI() (*apitypes.Limits, error) {
	memHigh, err := parseSize(lf.memoryHigh)
	if err != nil {
		return nil, fmt.Errorf("--memory-high: %w", err)
	}
	memMax, err := parseSize(lf.memoryMax)
	if err != nil {
		return nil, fmt.Errorf("--memory-max: %w", err)
	}
	if memHigh == 0 && memMax == 0 && lf.pidsMax == 0 && lf.cpuQuota == 0 {
		return nil, nil
	}
	return &apitypes.Limits{
		MemoryHighBytes: ptr(memHigh),
		MemoryMaxBytes:  ptr(memMax),
		PidsMax:         ptr(lf.pidsMax),
		CpuQuotaUs:      ptr(lf.cpuQuota),
		CpuPeriodUs:     ptr(lf.cpuPeriod),
	}, nil
}

// sandboxFlags registers the namespace / chroot / NoNewPrivs flags
// shared by `up` and `deploy`. Every knob is opt-in; the resulting
// API object is nil unless at least one is set.
type sandboxFlags struct {
	pid        bool
	uts        bool
	ipc        bool
	mount      bool
	user       bool
	noNewPrivs bool
	chroot     string
}

func (sf *sandboxFlags) register(fs *flag.FlagSet) {
	fs.BoolVar(&sf.pid, "pid-namespace", false, "isolate PIDs (child sees itself as pid 1)")
	fs.BoolVar(&sf.uts, "uts-namespace", false, "isolate hostname/domainname")
	fs.BoolVar(&sf.ipc, "ipc-namespace", false, "isolate sysv IPC / posix MQ")
	fs.BoolVar(&sf.mount, "mount-namespace", false, "isolate mounts (compose with --chroot)")
	fs.BoolVar(&sf.user, "user-namespace", false, "isolate UIDs/GIDs (no mapping = root-in-ns maps to current real uid)")
	fs.BoolVar(&sf.noNewPrivs, "no-new-privs", false, "set PR_SET_NO_NEW_PRIVS via setpriv wrapper (sticky for life)")
	fs.StringVar(&sf.chroot, "chroot", "", "chroot the child into this directory (path must be absolute)")
}

func (sf *sandboxFlags) toAPI() *apitypes.Sandbox {
	if !sf.pid && !sf.uts && !sf.ipc && !sf.mount && !sf.user && !sf.noNewPrivs && sf.chroot == "" {
		return nil
	}
	return &apitypes.Sandbox{
		PidNamespace:   ptr(sf.pid),
		UtsNamespace:   ptr(sf.uts),
		IpcNamespace:   ptr(sf.ipc),
		MountNamespace: ptr(sf.mount),
		UserNamespace:  ptr(sf.user),
		NoNewPrivs:     ptr(sf.noNewPrivs),
		Chroot:         ptr(sf.chroot),
	}
}

// parseSize parses a human-friendly byte count: a bare integer
// (bytes) or an integer followed by K/M/G/T (binary, *1024). The
// optional "i"/"iB"/"B" suffix is accepted and ignored — "256M",
// "256Mi", "256MiB", "256MB" all mean 256*1024*1024.
//
// Empty string returns 0. Lowercase is fine.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	s = strings.ToUpper(s)
	s = strings.TrimSuffix(s, "B")
	s = strings.TrimSuffix(s, "I")
	unit := int64(1)
	switch {
	case strings.HasSuffix(s, "K"):
		unit = 1024
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "M"):
		unit = 1024 * 1024
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "G"):
		unit = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "T"):
		unit = 1024 * 1024 * 1024 * 1024
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("size must be non-negative, got %d", n)
	}
	return n * unit, nil
}

// splitID returns the first argv element when it is a positional
// (does not start with '-') and the remaining argv. Used to peel the
// <id> off the front before flag parsing so callers can write
// `creekctl up smoke --command sleep` in either order.
//
// Returns ("", argv) when argv is empty or starts with a flag —
// the caller decides whether the ID is required.
func splitID(argv []string) (string, []string) {
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		return argv[0], argv[1:]
	}
	return "", argv
}

// requireSplitID is splitID with an error when no ID was found.
func requireSplitID(argv []string) (string, []string, error) {
	id, rest := splitID(argv)
	if id == "" {
		return "", argv, errors.New("missing <id> argument")
	}
	return id, rest, nil
}

// --- ps -----------------------------------------------------------

func runPS(ctx context.Context, w io.Writer, argv []string) error {
	fs := newFlagSet("ps")
	var cf commonFlags
	cf.register(fs)
	fields := fs.String("fields", "", "comma-separated field names to include (JSON mode)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	cf.resolveJSON()
	apps, err := cf.client().List(ctx)
	if err != nil {
		return err
	}
	if cf.json {
		if *fields != "" {
			filtered, err := filterFields(apps, *fields)
			if err != nil {
				return err
			}
			return writeJSON(w, filtered)
		}
		return writeJSON(w, apps)
	}
	return writeAppTable(w, apps)
}

// --- get ----------------------------------------------------------

func runGet(ctx context.Context, w io.Writer, argv []string) error {
	id, rest, err := requireSplitID(argv)
	if err != nil {
		return err
	}
	fs := newFlagSet("get")
	var cf commonFlags
	cf.register(fs)
	fields := fs.String("fields", "", "comma-separated field names to include (JSON mode)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	cf.resolveJSON()
	app, err := cf.client().Get(ctx, id)
	if err != nil {
		return err
	}
	if cf.json {
		if *fields != "" {
			filtered, err := filterFields(app, *fields)
			if err != nil {
				return err
			}
			return writeJSON(w, filtered)
		}
		return writeJSON(w, app)
	}
	return writeAppEnvelope(w, app)
}

// --- up -----------------------------------------------------------

// stringSliceFlag implements flag.Value for repeated string flags
// (e.g. --env KEY=VAL --env OTHER=VAL).
type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }

func runUp(ctx context.Context, w io.Writer, argv []string) error {
	id, rest, err := requireSplitID(argv)
	if err != nil {
		return err
	}
	fs := newFlagSet("up")
	var cf commonFlags
	cf.register(fs)
	var lf limitsFlags
	lf.register(fs)
	var sf sandboxFlags
	sf.register(fs)
	var (
		command    = fs.String("command", "", "executable to run (explicit mode)")
		entry      = fs.String("entry", "", "entry script (with --runtime)")
		runtimeArg = fs.String("runtime", "", "bun|node|deno (with --entry)")
		port       = fs.Int("port", 0, "dispatch port the app listens on")
		fromPath   = fs.String("from", "", "path to a .creek-creekd/manifest.json (seeds runtime/entry/port; CLI flags override)")
		healthPath = fs.String("health-path", "", "HTTP path for the periodic liveness probe (default \"/\"; set to e.g. \"/healthz\" for strict readiness)")
		jsonInput  = fs.String("json-input", "", "raw SpawnRequest JSON (agent-facing; overrides individual flags)")
		args       stringSliceFlag
		env        stringSliceFlag
		netIso     = fs.Bool("net-isolation", false, "spawn inside a per-app netns")
	)
	fs.Var(&args, "arg", "argument passed to the command (repeat for multiple)")
	fs.Var(&env, "env", "environment variable KEY=VAL (repeat for multiple)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	var req apitypes.SpawnRequest
	if *jsonInput != "" {
		if err := json.Unmarshal([]byte(*jsonInput), &req); err != nil {
			return fmt.Errorf("--json-input: %w", err)
		}
		req.Id = id
	} else {
		limits, err := lf.toAPI()
		if err != nil {
			return err
		}
		req = apitypes.SpawnRequest{
			Id:              id,
			Command:         ptr(*command),
			Entry:           ptr(*entry),
			Runtime:         ptrRuntime(*runtimeArg),
			Args:            ptrSlice(args),
			Env:             ptrSlice(env),
			Port:            *port,
			Limits:          limits,
			NetIsolation:    ptr(*netIso),
			Sandbox:         sf.toAPI(),
			HealthCheckPath: ptr(*healthPath),
		}
		if *fromPath != "" {
			m, projectDir, err := manifest.Load(*fromPath)
			if err != nil {
				return err
			}
			applyManifestTo(&req, m, projectDir)
		}
	}
	if err := validateStringInputs(
		"command", derefStr(req.Command),
		"entry", derefStr(req.Entry),
		"runtime", derefRuntimeStr(req.Runtime),
		"health-path", derefStr(req.HealthCheckPath),
	); err != nil {
		return err
	}
	cf.resolveJSON()
	if cf.dryRun {
		return writeDryRun(w, "up", id, req, cf.json)
	}
	app, err := cf.client().Spawn(ctx, req)
	if err != nil {
		return err
	}
	if cf.json {
		return writeJSON(w, app)
	}
	return writeAppDetail(w, app)
}

// --- ensure ------------------------------------------------------

// runEnsure is the idempotent spawn verb: creates the app if absent,
// no-ops if already running. Agents use this to avoid the two-step
// "ps → branch → up" pattern.
func runEnsure(ctx context.Context, w io.Writer, argv []string) error {
	id, rest, err := requireSplitID(argv)
	if err != nil {
		return err
	}
	fs := newFlagSet("ensure")
	var cf commonFlags
	cf.register(fs)
	var lf limitsFlags
	lf.register(fs)
	var sf sandboxFlags
	sf.register(fs)
	var (
		command    = fs.String("command", "", "executable to run (explicit mode)")
		entry      = fs.String("entry", "", "entry script (with --runtime)")
		runtimeArg = fs.String("runtime", "", "bun|node|deno (with --entry)")
		port       = fs.Int("port", 0, "dispatch port the app listens on")
		fromPath   = fs.String("from", "", "path to manifest.json")
		healthPath = fs.String("health-path", "", "HTTP liveness probe path")
		jsonInput  = fs.String("json-input", "", "raw SpawnRequest JSON")
		args       stringSliceFlag
		env        stringSliceFlag
		netIso     = fs.Bool("net-isolation", false, "spawn inside a per-app netns")
	)
	fs.Var(&args, "arg", "argument (repeat for multiple)")
	fs.Var(&env, "env", "environment KEY=VAL (repeat for multiple)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	var req apitypes.SpawnRequest
	if *jsonInput != "" {
		if err := json.Unmarshal([]byte(*jsonInput), &req); err != nil {
			return fmt.Errorf("--json-input: %w", err)
		}
		req.Id = id
	} else {
		limits, err := lf.toAPI()
		if err != nil {
			return err
		}
		req = apitypes.SpawnRequest{
			Id:              id,
			Command:         ptr(*command),
			Entry:           ptr(*entry),
			Runtime:         ptrRuntime(*runtimeArg),
			Args:            ptrSlice(args),
			Env:             ptrSlice(env),
			Port:            *port,
			Limits:          limits,
			NetIsolation:    ptr(*netIso),
			Sandbox:         sf.toAPI(),
			HealthCheckPath: ptr(*healthPath),
		}
		if *fromPath != "" {
			m, projectDir, err := manifest.Load(*fromPath)
			if err != nil {
				return err
			}
			applyManifestTo(&req, m, projectDir)
		}
	}
	if err := validateStringInputs(
		"command", derefStr(req.Command),
		"entry", derefStr(req.Entry),
		"runtime", derefRuntimeStr(req.Runtime),
		"health-path", derefStr(req.HealthCheckPath),
	); err != nil {
		return err
	}
	cf.resolveJSON()
	if cf.dryRun {
		return writeDryRun(w, "ensure", id, req, cf.json)
	}
	client := cf.client()
	if _, serr := client.Spawn(ctx, req); serr != nil && !adminclient.IsAlreadyRunning(serr) {
		return serr
	}
	// Whether the spawn succeeded or the app was already running, Get
	// returns the envelope. ensure's output is therefore identical
	// across both paths — important for --json automation, which used
	// to receive an AppView on create vs an envelope on already-exists.
	envelope, gerr := client.Get(ctx, id)
	if gerr != nil {
		return gerr
	}
	if cf.json {
		return writeJSON(w, envelope)
	}
	return writeAppEnvelope(w, envelope)
}

// --- rm -----------------------------------------------------------

func runRM(ctx context.Context, w io.Writer, argv []string) error {
	id, rest, err := requireSplitID(argv)
	if err != nil {
		return err
	}
	fs := newFlagSet("rm")
	var cf commonFlags
	cf.register(fs)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	cf.resolveJSON()
	if cf.dryRun {
		return writeDryRun(w, "rm", id, nil, cf.json)
	}
	if err := cf.client().Stop(ctx, id); err != nil {
		return err
	}
	fmt.Fprintf(w, "stopped %s\n", id)
	return nil
}

// --- restart ------------------------------------------------------

func runRestart(ctx context.Context, w io.Writer, argv []string) error {
	id, rest, err := requireSplitID(argv)
	if err != nil {
		return err
	}
	fs := newFlagSet("restart")
	var cf commonFlags
	cf.register(fs)
	timeoutMS := fs.Int64("timeout-ms", 0, "max wait for new PID (0 = server default)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	cf.resolveJSON()
	app, err := cf.client().Restart(ctx, id, apitypes.RestartRequest{TimeoutMs: ptr(*timeoutMS)})
	if err != nil {
		return err
	}
	if cf.json {
		return writeJSON(w, app)
	}
	return writeAppDetail(w, app)
}

// --- reset --------------------------------------------------------

func runReset(ctx context.Context, w io.Writer, argv []string) error {
	id, rest, err := requireSplitID(argv)
	if err != nil {
		return err
	}
	fs := newFlagSet("reset")
	var cf commonFlags
	cf.register(fs)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	cf.resolveJSON()
	app, err := cf.client().Reset(ctx, id)
	if err != nil {
		return err
	}
	if cf.json {
		return writeJSON(w, app)
	}
	return writeAppDetail(w, app)
}

// --- deploy -------------------------------------------------------

func runDeploy(ctx context.Context, w io.Writer, argv []string) error {
	id, rest, err := requireSplitID(argv)
	if err != nil {
		return err
	}
	fs := newFlagSet("deploy")
	var cf commonFlags
	cf.register(fs)
	var lf limitsFlags
	lf.register(fs)
	var sf sandboxFlags
	sf.register(fs)
	var (
		command        = fs.String("command", "", "executable to run (explicit mode)")
		entry          = fs.String("entry", "", "entry script (with --runtime)")
		runtimeArg     = fs.String("runtime", "", "bun|node|deno")
		port           = fs.Int("port", 0, "v2 port (must differ from v1's)")
		fromPath       = fs.String("from", "", "path to a .creek-creekd/manifest.json (seeds runtime/entry/port; CLI flags override)")
		healthPath     = fs.String("health-path", "", "HTTP path for the periodic liveness probe (default \"/\"; set to e.g. \"/healthz\" for strict readiness)")
		jsonInput      = fs.String("json-input", "", "raw DeployRequest JSON (agent-facing; overrides individual flags)")
		releaseCmd     = fs.String("release", "", "command to run after v2 is healthy but before traffic swap (e.g., db migration)")
		releaseTimeout = fs.Int("release-timeout", 60, "release command timeout in seconds")
		args           stringSliceFlag
		env            stringSliceFlag
		readyMS        = fs.Int64("ready-timeout-ms", 0, "max wait for v2 to be healthy")
		netIso         = fs.Bool("net-isolation", false, "spawn v2 inside a netns")
	)
	fs.Var(&args, "arg", "argument (repeat for multiple)")
	fs.Var(&env, "env", "environment KEY=VAL (repeat for multiple)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	var req apitypes.DeployRequest
	if *jsonInput != "" {
		if err := json.Unmarshal([]byte(*jsonInput), &req); err != nil {
			return fmt.Errorf("--json-input: %w", err)
		}
	} else {
		limits, err := lf.toAPI()
		if err != nil {
			return err
		}
		req = apitypes.DeployRequest{
			Command:         ptr(*command),
			Entry:           ptr(*entry),
			Runtime:         ptrRuntime(*runtimeArg),
			Args:            ptrSlice(args),
			Env:             ptrSlice(env),
			Port:            *port,
			Limits:          limits,
			ReadyTimeoutMs:  ptr(*readyMS),
			NetIsolation:    ptr(*netIso),
			Sandbox:         sf.toAPI(),
			HealthCheckPath: ptr(*healthPath),
		}
		if *fromPath != "" {
			m, projectDir, err := manifest.Load(*fromPath)
			if err != nil {
				return err
			}
			applyManifestToDeploy(&req, m, projectDir)
		}
	}
	if err := validateStringInputs(
		"command", derefStr(req.Command),
		"entry", derefStr(req.Entry),
		"runtime", derefRuntimeStr(req.Runtime),
		"health-path", derefStr(req.HealthCheckPath),
	); err != nil {
		return err
	}
	cf.resolveJSON()
	if cf.dryRun {
		return writeDryRun(w, "deploy", id, req, cf.json)
	}
	app, err := cf.client().Deploy(ctx, id, req)
	if err != nil {
		return err
	}

	// Release phase: run command after v2 is healthy, before caller
	// considers deploy "done". Failure is reported but does NOT
	// auto-rollback — the blue-green swap already happened inside
	// supervisor.Deploy. Rollback requires a separate creekctl deploy
	// with the old config. This matches Heroku's behavior where
	// release failures are reported but the release is already live.
	var releaseResult *releaseOutput
	if *releaseCmd != "" {
		releaseResult, err = runRelease(ctx, cf, id, *releaseCmd, *releaseTimeout)
		if err != nil {
			if cf.json {
				return writeJSON(w, map[string]any{
					"app":     app,
					"release": releaseResult,
					"error":   "release_failed",
				})
			}
			fmt.Fprintf(w, "⚠ Release command failed: %v\n", err)
			fmt.Fprintf(w, "  The deploy completed but the release command did not succeed.\n")
			fmt.Fprintf(w, "  Rollback with: creekctl deploy %s ...(previous config)\n", id)
			return writeAppDetail(w, app)
		}
	}

	if cf.json {
		if releaseResult != nil {
			return writeJSON(w, map[string]any{
				"app":     app,
				"release": releaseResult,
			})
		}
		return writeJSON(w, app)
	}
	if releaseResult != nil {
		fmt.Fprintf(w, "✓ Release: %s (%dms)\n\n", releaseResult.Command, releaseResult.DurationMS)
	}
	return writeAppDetail(w, app)
}

type releaseOutput struct {
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	Output     string `json:"output,omitempty"`
}

func runRelease(ctx context.Context, cf commonFlags, appID, command string, timeoutSec int) (*releaseOutput, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(timeoutCtx, "bash", "-c", command)

	// Inherit app env vars via creekctl exec-style injection. Get
	// now returns the envelope shape; env + port live under .Spec.
	client := cf.client()
	app, err := client.Get(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("release: cannot get app env: %w", err)
	}
	cmd.Env = append(os.Environ(), derefSlice(app.Spec.Env)...)
	port := 0
	if app.Spec.Port != nil {
		port = *app.Spec.Port
	}
	cmd.Env = append(cmd.Env, fmt.Sprintf("PORT=%d", port))

	out, err := cmd.CombinedOutput()
	duration := time.Since(start).Milliseconds()

	result := &releaseOutput{
		Command:    command,
		DurationMS: duration,
		Output:     strings.TrimSpace(string(out)),
	}

	if err != nil {
		if timeoutCtx.Err() != nil {
			result.ExitCode = -1
			return result, fmt.Errorf("release: timed out after %ds", timeoutSec)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
		}
		return result, fmt.Errorf("release: exit %d: %s", result.ExitCode, result.Output)
	}

	result.ExitCode = 0
	return result, nil
}

// --- logs ---------------------------------------------------------

func runLogs(ctx context.Context, w io.Writer, argv []string) error {
	id, rest, err := requireSplitID(argv)
	if err != nil {
		return err
	}
	fs := newFlagSet("logs")
	var cf commonFlags
	cf.register(fs)
	tail := fs.Int("tail", 100, "lines to retrieve from end of log")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	body, err := cf.client().LogsTail(ctx, id, *tail)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, strings.NewReader(body))
	return err
}

// --- events -------------------------------------------------------

func runEvents(ctx context.Context, w io.Writer, argv []string) error {
	id, rest, err := requireSplitID(argv)
	if err != nil {
		return err
	}
	fs := newFlagSet("events")
	var cf commonFlags
	cf.register(fs)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	return cf.client().Events(ctx, id, func(line []byte) error {
		_, err := fmt.Fprintf(w, "%s\n", line)
		return err
	})
}

// --- exec ---------------------------------------------------------

// runExec runs a one-off command with the app's env vars injected.
// Equivalent to `heroku run` or `railway run`. The command inherits
// DATABASE_URL, REDIS_URL, etc. from the running sandbox.
func runExec(ctx context.Context, w io.Writer, argv []string) error {
	fs := newFlagSet("exec")
	var cf commonFlags
	cf.register(fs)
	appID := fs.String("app", "", "app ID to inherit env vars from (uses first app if empty)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	args := fs.Args()
	if len(args) == 0 {
		return fmt.Errorf("exec: missing command (e.g., creekctl exec -- rails console)")
	}

	// Get the app's env vars from the running supervisor. Get
	// returns the envelope shape (App); List returns []AppView. We
	// project both into a (envVars, port) pair locally.
	client := cf.client()
	var (
		appEnv []string
		port   int
	)

	if *appID != "" {
		envelope, gerr := client.Get(ctx, *appID)
		if gerr != nil {
			return gerr
		}
		appEnv = derefSlice(envelope.Spec.Env)
		if envelope.Spec.Port != nil {
			port = *envelope.Spec.Port
		}
	} else {
		apps, listErr := client.List(ctx)
		if listErr != nil {
			return listErr
		}
		if len(apps) == 0 {
			return fmt.Errorf("exec: no apps running")
		}
		appEnv = derefSlice(apps[0].Env)
		port = apps[0].Port
	}

	// Build env: inherit current env + inject app env vars (DATABASE_URL, etc)
	env := os.Environ()
	env = append(env, appEnv...)
	env = append(env, fmt.Sprintf("PORT=%d", port))

	// Execute the command
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = env
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// --- db-reset -----------------------------------------------------

func runDBReset(ctx context.Context, w io.Writer, argv []string) error {
	fs := newFlagSet("db-reset")
	var cf commonFlags
	cf.register(fs)
	dbURL := fs.String("database-url", os.Getenv("DATABASE_URL"), "database connection string (or $DATABASE_URL)")
	dbName := fs.String("db-name", "", "database name to reset (derived from URL if empty)")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	if *dbURL == "" {
		return fmt.Errorf("db-reset: DATABASE_URL not set and --database-url not provided")
	}

	cf.resolveJSON()

	name := *dbName
	if name == "" {
		// Extract db name from postgresql://.../<dbname>
		parts := strings.Split(*dbURL, "/")
		if len(parts) > 0 {
			name = strings.Split(parts[len(parts)-1], "?")[0]
		}
	}
	if name == "" {
		return fmt.Errorf("db-reset: cannot determine database name from URL")
	}

	// Build a connection URL pointing to the default 'postgres' DB for admin ops
	adminURL := strings.Replace(*dbURL, "/"+name, "/postgres", 1)

	if cf.dryRun {
		return writeDryRun(w, "db-reset", name, map[string]string{
			"database": name,
			"action":   "DROP DATABASE IF EXISTS + CREATE DATABASE",
		}, cf.json)
	}

	// Use pg driver via the admin API's exec endpoint, or shell out to psql
	// For Phase 1: shell out to psql which is available in the sandbox
	type resetResult struct {
		Database string `json:"database"`
		Status   string `json:"status"`
		Action   string `json:"action"`
	}

	// Try psql first (available in sandbox VM)
	dropCmd := fmt.Sprintf("psql '%s' -c 'DROP DATABASE IF EXISTS \"%s\"'", adminURL, name)
	createCmd := fmt.Sprintf("psql '%s' -c 'CREATE DATABASE \"%s\"'", adminURL, name)

	for _, cmd := range []string{dropCmd, createCmd} {
		out, err := exec.CommandContext(ctx, "bash", "-c", cmd).CombinedOutput()
		if err != nil {
			return fmt.Errorf("db-reset: %s: %w\n%s", cmd, err, out)
		}
	}

	result := resetResult{
		Database: name,
		Status:   "ok",
		Action:   "dropped and recreated",
	}

	if cf.json {
		return writeJSON(w, result)
	}
	fmt.Fprintf(w, "✓ Database %q reset (dropped + recreated)\n", name)
	return nil
}

// --- stats --------------------------------------------------------

func runStats(ctx context.Context, w io.Writer, argv []string) error {
	id, rest, err := requireSplitID(argv)
	if err != nil {
		return err
	}
	fs := newFlagSet("stats")
	var cf commonFlags
	cf.register(fs)
	fields := fs.String("fields", "", "comma-separated field names to include (JSON mode)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	cf.resolveJSON()
	s, err := cf.client().Stats(ctx, id)
	if err != nil {
		return err
	}
	if cf.json {
		if *fields != "" {
			filtered, err := filterFields(s, *fields)
			if err != nil {
				return err
			}
			return writeJSON(w, filtered)
		}
		return writeJSON(w, s)
	}
	return writeStatsDetail(w, s)
}

// --- input validation ---------------------------------------------

// rejectControlChars returns an error if s contains any byte below
// ASCII 0x20 (space). Agents sometimes hallucinate invisible
// characters in string values; rejecting them early prevents
// downstream confusion.
func rejectControlChars(label, s string) error {
	for i, b := range []byte(s) {
		if b < 0x20 {
			return fmt.Errorf("invalid %s: control character 0x%02x at byte %d", label, b, i)
		}
	}
	return nil
}

// validateStringInputs checks agent-facing string flags for control
// characters that would indicate hallucination.
func validateStringInputs(pairs ...string) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if err := rejectControlChars(pairs[i], pairs[i+1]); err != nil {
			return err
		}
	}
	return nil
}

// --- describe ----------------------------------------------------

type flagInfo struct {
	Name     string `json:"name"`
	Default  string `json:"default,omitempty"`
	Usage    string `json:"usage"`
	Required bool   `json:"required,omitempty"`
}

type commandInfo struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Flags       []flagInfo `json:"flags"`
}

func runDescribe(_ context.Context, w io.Writer, argv []string) error {
	if len(argv) == 0 {
		all := make([]commandInfo, 0, len(subcommands))
		for _, cmd := range subcommands {
			all = append(all, commandInfo{Name: cmd.Name, Description: cmd.Description})
		}
		return writeJSON(w, all)
	}
	name := argv[0]
	cmd, ok := subcommands[name]
	if !ok {
		return fmt.Errorf("unknown command %q", name)
	}
	fs := newFlagSet(name)
	var cf commonFlags
	cf.register(fs)
	if name == "up" || name == "deploy" {
		var lf limitsFlags
		lf.register(fs)
		var sf sandboxFlags
		sf.register(fs)
	}
	var flags []flagInfo
	fs.VisitAll(func(f *flag.Flag) {
		flags = append(flags, flagInfo{
			Name:    f.Name,
			Default: f.DefValue,
			Usage:   f.Usage,
		})
	})
	info := commandInfo{
		Name:        cmd.Name,
		Description: cmd.Description,
		Flags:       flags,
	}
	return writeJSON(w, info)
}

// runHardeningCheck validates a systemd unit file against the
// canonical hardening set defined by hardening.RequiredDirectives.
// Argv[0] is the path to the unit file; defaults to
// /etc/systemd/system/creekd.service. Exits 0 on clean, 1 on drift
// detected (matching shellcheck / lint convention).
//
// `creek host doctor` (when the laptop CLI lands in #23) wraps the
// same internal/hardening validator and surfaces the result as the
// `systemd_hardening_drift` error code; this creekctl subcommand
// is the on-host operator path.
func runHardeningCheck(_ context.Context, w io.Writer, argv []string) error {
	fs := newFlagSet("hardening-check")
	cf := &commonFlags{}
	cf.register(fs)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	cf.resolveJSON()
	path := "/etc/systemd/system/creekd.service"
	if rest := fs.Args(); len(rest) > 0 {
		path = rest[0]
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	drift, err := hardening.Validate(string(data))
	if err != nil {
		return err
	}
	if cf.json {
		if err := writeJSON(w, drift); err != nil {
			return err
		}
	} else if len(drift) == 0 {
		fmt.Fprintf(w, "%s: hardening clean (%d directives validated)\n",
			path, len(hardening.RequiredDirectives()))
	} else {
		fmt.Fprintf(w, "%s: %d hardening drift(s)\n", path, len(drift))
		for _, d := range drift {
			fmt.Fprintf(w, "  %s\n", d)
		}
	}
	if len(drift) > 0 {
		return fmt.Errorf("systemd_hardening_drift: %d directive(s) missing or weakened", len(drift))
	}
	return nil
}

// runSelfUpgrade downloads a tagged release tarball from GitHub,
// verifies it via cosign + SHA256 (internal/upgrade.Verifier), and
// atomically replaces the running creekctl + the sibling creekd
// binary via the rename trick.
//
// Flags:
//   --to=<version>       target tag (default: latest)
//   --release-base=<url> override GitHub Releases URL prefix
//                        (test hook; production callers omit)
//   --creekd=<path>      override creekd binary path
//                        (default: sibling of creekctl in the
//                        same directory)
//
// Exits with upgrade_signature_invalid on a verification REJECTION
// (cosign signature mismatch or tarball SHA256 mismatch). Other
// failure modes (cosign not installed / not executable / timed out,
// network errors, missing checksums entry) surface as their own
// non-mapped errors so the operator can fix the install instead of
// chasing a security alarm. The new binaries are only moved into
// place AFTER both checks pass; any failure leaves the existing
// binaries untouched.
func runSelfUpgrade(_ context.Context, w io.Writer, argv []string) error {
	fs := newFlagSet("self-upgrade")
	cf := &commonFlags{}
	cf.register(fs)
	to := fs.String("to", "", "target release tag (e.g. v0.0.5); empty = latest")
	releaseBase := fs.String("release-base", "https://github.com/solcreek/creekd/releases/download", "release download URL prefix (test hook)")
	creekdPath := fs.String("creekd", "", "creekd binary path (default: sibling of creekctl)")
	creekctlPath := fs.String("creekctl", "", "creekctl binary path (default: os.Executable; override for tests)")
	latestURL := fs.String("latest-url", "https://github.com/solcreek/creekd/releases/latest", "URL whose final redirect names the latest tag (test hook)")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	version := *to
	if version == "" {
		v, err := resolveLatestTag(*latestURL)
		if err != nil {
			return err
		}
		version = v
	}
	fmt.Fprintf(w, "==> target %s\n", version)

	osName, arch, err := detectOSArch()
	if err != nil {
		return err
	}
	verNoV := strings.TrimPrefix(version, "v")
	tarName := fmt.Sprintf("creekd_%s_%s_%s.tar.gz", verNoV, osName, arch)

	tmp, err := os.MkdirTemp("", "creekd-selfupgrade-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// Pull the three artifacts cosign needs + the tarball. Ordered
	// (not a map) so log output is deterministic and the tarball —
	// the largest fetch and the one most likely to surface a
	// network issue — comes first.
	downloads := []struct{ name, url string }{
		{tarName, fmt.Sprintf("%s/%s/%s", *releaseBase, version, tarName)},
		{"checksums.txt", fmt.Sprintf("%s/%s/checksums.txt", *releaseBase, version)},
		{"checksums.txt.sig", fmt.Sprintf("%s/%s/checksums.txt.sig", *releaseBase, version)},
		{"checksums.txt.pem", fmt.Sprintf("%s/%s/checksums.txt.pem", *releaseBase, version)},
	}
	for _, d := range downloads {
		fmt.Fprintf(w, "==> downloading %s\n", d.name)
		if err := downloadFile(d.url, filepath.Join(tmp, d.name)); err != nil {
			return fmt.Errorf("download %s: %w", d.name, err)
		}
	}

	// Two-layer verify. Either layer's failure leaves the local
	// binaries untouched.
	fmt.Fprintln(w, "==> verifying cosign + SHA256")
	v := upgrade.New()
	if cosign := os.Getenv("CREEKCTL_COSIGN_PATH"); cosign != "" {
		// Test hook + operator escape hatch (e.g. a wrapper script
		// that adds Rekor offline-bundle support not yet in cosign
		// upstream). Default is the cosign binary on PATH.
		v.CosignPath = cosign
	}
	if err := v.Verify(
		filepath.Join(tmp, tarName), tarName,
		filepath.Join(tmp, "checksums.txt.sig"),
		filepath.Join(tmp, "checksums.txt.pem"),
		filepath.Join(tmp, "checksums.txt"),
	); err != nil {
		if errors.Is(err, upgrade.ErrSignatureInvalid) {
			return fmt.Errorf("upgrade_signature_invalid: %w", err)
		}
		return err
	}
	fmt.Fprintln(w, "==> verification passed")

	// Extract tarball into tmp. New binaries land at tmp/creekd
	// and tmp/creekctl alongside the original tarball.
	if err := extractTarGz(filepath.Join(tmp, tarName), tmp); err != nil {
		return fmt.Errorf("extract tarball: %w", err)
	}

	// Resolve install paths.
	ctlPath := *creekctlPath
	if ctlPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve creekctl path: %w", err)
		}
		ctlPath = exe
	}
	dPath := *creekdPath
	if dPath == "" {
		dPath = filepath.Join(filepath.Dir(ctlPath), "creekd")
	}

	// Stash the pre-upgrade creekd so creekctl-swap failure can
	// roll creekd back. Reserve a unique stash path via CreateTemp
	// (a predictable name would silently clobber any operator file
	// at that path), then populate it with CopyFile — hard link
	// fast path on the common case, byte-copy fallback otherwise.
	stashFile, err := os.CreateTemp(filepath.Dir(dPath), filepath.Base(dPath)+".pre-upgrade-*")
	if err != nil {
		return fmt.Errorf("reserve stash path: %w", err)
	}
	stashPath := stashFile.Name()
	stashFile.Close()
	defer os.Remove(stashPath)
	if err := upgrade.CopyFile(dPath, stashPath); err != nil {
		return fmt.Errorf("stash pre-upgrade creekd: %w", err)
	}

	fmt.Fprintf(w, "==> replacing %s\n", dPath)
	if err := upgrade.SwapBinary(filepath.Join(tmp, "creekd"), dPath); err != nil {
		return err
	}
	fmt.Fprintf(w, "==> replacing %s\n", ctlPath)
	if err := upgrade.SwapBinary(filepath.Join(tmp, "creekctl"), ctlPath); err != nil {
		// Best-effort rollback; surface the original error either way.
		if rbErr := upgrade.SwapBinary(stashPath, dPath); rbErr != nil {
			fmt.Fprintf(w, "WARN: creekctl upgrade failed AND rollback of %s also failed: %v\n", dPath, rbErr)
		} else {
			fmt.Fprintf(w, "==> creekctl swap failed; rolled back %s to pre-upgrade\n", dPath)
		}
		return err
	}
	fmt.Fprintf(w, "==> upgraded to %s\n", version)
	_ = cf // commonFlags currently unused; reserved for future --json output
	return nil
}

// resolveLatestTag follows GitHub's /releases/latest redirect to
// pick up the tag without parsing the API. Mirrors install.sh's
// no-jq strategy.
func resolveLatestTag(url string) (string, error) {
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 30 * time.Second,
	}
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve latest: %w", err)
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("resolve latest: no Location header in response (status %d)", resp.StatusCode)
	}
	tag := loc[strings.LastIndex(loc, "/")+1:]
	if !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("resolve latest: redirect target %q does not look like a tag", loc)
	}
	return tag, nil
}

func detectOSArch() (string, string, error) {
	var osName, arch string
	switch runtime.GOOS {
	case "linux":
		osName = "linux"
	case "darwin":
		osName = "darwin"
	default:
		return "", "", fmt.Errorf("self-upgrade: unsupported OS %q (linux + darwin only)", runtime.GOOS)
	}
	switch runtime.GOARCH {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	default:
		return "", "", fmt.Errorf("self-upgrade: unsupported arch %q (amd64 + arm64 only)", runtime.GOARCH)
	}
	return osName, arch, nil
}

// downloadFile fetches url into dst with a bounded timeout. Self-
// upgrade artifacts are tens of MB so the cap is generous, but it
// MUST exist — Go's default client has no timeout and a stalled
// TCP connection would hang the upgrade forever.
func downloadFile(url, dst string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = out.Close()
		return err
	}
	// Explicit close to surface late flush errors (NFS commit,
	// disk-full on buffered writes) — defer would discard the
	// return value.
	return out.Close()
}

// extractTarGz untars src into dstDir. Skips non-regular entries
// (we only want creekd + creekctl + the bundled docs). The
// HasPrefix(filepath.Clean(...)) check handles ".." and absolute
// paths but NOT pre-existing symlinks inside dstDir — safe here
// only because the caller always passes a fresh MkdirTemp; do not
// reuse against a persistent dstDir without O_NOFOLLOW + per-
// component lstat.
func extractTarGz(src, dstDir string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		out := filepath.Join(dstDir, hdr.Name)
		// filepath.Clean normalises ".." segments; HasPrefix refuses
		// any entry whose cleaned path is outside dstDir. (Path-
		// string only — see the function comment for the symlink
		// caveat.)
		clean := filepath.Clean(out)
		if !strings.HasPrefix(clean, dstDir+string(os.PathSeparator)) && clean != dstDir {
			return fmt.Errorf("tarball entry %q escapes dest dir", hdr.Name)
		}
		if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
			return err
		}
		w, err := os.OpenFile(clean, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, tr); err != nil {
			_ = w.Close()
			return err
		}
		// Surface late flush errors so a partially-written
		// creekd/creekctl can't slip past extraction and into
		// SwapBinary.
		if err := w.Close(); err != nil {
			return err
		}
	}
	return nil
}

// --- field filtering -----------------------------------------------

// filterFields takes a JSON-serializable value and returns a filtered
// version containing only the specified fields. Works on both single
// objects and slices. Used by --fields to protect agent context windows.
func filterFields(v any, fields string) (any, error) {
	if fields == "" {
		return v, nil
	}
	wanted := make(map[string]bool)
	for _, f := range strings.Split(fields, ",") {
		f = strings.TrimSpace(f)
		if f != "" {
			wanted[f] = true
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	// Try as array first
	var arr []map[string]any
	if err := json.Unmarshal(data, &arr); err == nil {
		result := make([]map[string]any, len(arr))
		for i, obj := range arr {
			filtered := make(map[string]any)
			for k, val := range obj {
				if wanted[k] {
					filtered[k] = val
				}
			}
			result[i] = filtered
		}
		return result, nil
	}
	// Try as single object
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return v, nil
	}
	filtered := make(map[string]any)
	for k, val := range obj {
		if wanted[k] {
			filtered[k] = val
		}
	}
	return filtered, nil
}

// --- output helpers -----------------------------------------------

// writeDryRun prints what a mutating command would do without executing.
func writeDryRun(w io.Writer, verb, id string, payload any, jsonMode bool) error {
	type dryRunOutput struct {
		DryRun  bool   `json:"dry_run"`
		Command string `json:"command"`
		ID      string `json:"id"`
		Payload any    `json:"payload,omitempty"`
	}
	out := dryRunOutput{
		DryRun:  true,
		Command: verb,
		ID:      id,
		Payload: payload,
	}
	if jsonMode {
		return writeJSON(w, out)
	}
	fmt.Fprintf(w, "dry-run: would %s %s\n", verb, id)
	if payload != nil {
		data, _ := json.MarshalIndent(payload, "  ", "  ")
		fmt.Fprintf(w, "  payload: %s\n", data)
	}
	return nil
}

// writeJSON encodes v as indented JSON to w.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeAppTable renders a list of apps as a tab-aligned table.
func writeAppTable(w io.Writer, apps []apitypes.AppView) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tPID\tPORT\tNET_IP\tUPTIME_MS\tRESTARTS\tHEALTH_FAILS")
	for _, a := range apps {
		netIP := derefStr(a.NetIp)
		if netIP == "" {
			netIP = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%d\t%d\t%d\n",
			a.Id, a.Status, a.Pid, a.Port, netIP,
			a.UptimeMs, a.RestartCount, a.HealthFailures)
	}
	return tw.Flush()
}

// writeStatsDetail renders a StatsView as aligned key/value pairs.
// Bytes are shown in MiB and CPU usage in milliseconds for human
// readability; the JSON form still carries raw integers for tools.
func writeStatsDetail(w io.Writer, s *apitypes.StatsView) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "id\t%s\n", s.Id)
	fmt.Fprintf(tw, "cgroup_enabled\t%t\n", s.CgroupEnabled)
	if !s.CgroupEnabled {
		fmt.Fprintln(tw, "note\t(spawn the app with cgroup limits to see resource counters)")
		return tw.Flush()
	}
	memMax := derefInt64(s.MemoryMaxBytes)
	if memMax > 0 {
		fmt.Fprintf(tw, "memory_used\t%s / %s\n",
			humanBytes(derefInt64(s.MemoryCurrentBytes)), humanBytes(memMax))
	} else {
		fmt.Fprintf(tw, "memory_used\t%s (unlimited)\n",
			humanBytes(derefInt64(s.MemoryCurrentBytes)))
	}
	fmt.Fprintf(tw, "pids_current\t%d\n", derefInt64(s.PidsCurrent))
	fmt.Fprintf(tw, "cpu_total\t%s\n", humanMicros(derefInt64(s.CpuUsageUsec)))
	fmt.Fprintf(tw, "oom_kills\t%d\n", derefInt64(s.OomKills))
	if readErr := derefStr(s.ReadErr); readErr != "" {
		fmt.Fprintf(tw, "read_err\t%s\n", readErr)
	}
	return tw.Flush()
}

// humanBytes renders b in MiB with one decimal place. Bytes <= 1 KiB
// are shown literal so the operator sees the actual count rather
// than rounded "0.0 MiB".
func humanBytes(b int64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%d B", b)
	case b < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(b)/1024)
	default:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1024*1024))
	}
}

// humanMicros renders µs counters as ms / s for readability. CPU
// usage rolls into seconds within minutes for active apps.
func humanMicros(us int64) string {
	switch {
	case us < 1000:
		return fmt.Sprintf("%d µs", us)
	case us < 1_000_000:
		return fmt.Sprintf("%.1f ms", float64(us)/1000)
	default:
		return fmt.Sprintf("%.2f s", float64(us)/1_000_000)
	}
}

// writeAppDetail renders a single app as aligned key/value pairs.
func writeAppDetail(w io.Writer, a *apitypes.AppView) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fields := [][2]string{
		{"id", a.Id},
		{"status", string(a.Status)},
		{"pid", fmt.Sprintf("%d", a.Pid)},
		{"port", fmt.Sprintf("%d", a.Port)},
		{"command", a.Command},
		{"args", strings.Join(derefSlice(a.Args), " ")},
		{"runtime", derefRuntimeStr(a.Runtime)},
		{"net_ip", derefStr(a.NetIp)},
		{"uptime_ms", fmt.Sprintf("%d", a.UptimeMs)},
		{"restart_count", fmt.Sprintf("%d", a.RestartCount)},
		{"health_failures", fmt.Sprintf("%d", a.HealthFailures)},
	}
	for _, f := range fields {
		if f[1] == "" {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\n", f[0], f[1])
	}
	return tw.Flush()
}

// writeAppEnvelope renders the k8s-style App envelope as a flat
// human-readable table. Used by the GetApp CLI path (`creekctl get
// <id>`) since that's the only handler currently returning the
// envelope shape; other paths still flow through writeAppDetail.
//
// When more handlers move to the envelope (per implementation order)
// this becomes the default and writeAppDetail can be retired.
func writeAppEnvelope(w io.Writer, a *apitypes.App) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	command := derefStr(a.Spec.Command)
	port := 0
	if a.Spec.Port != nil {
		port = *a.Spec.Port
	}
	fields := [][2]string{
		{"name", a.Metadata.Name},
		{"uid", a.Metadata.Uid.String()},
		{"generation", fmt.Sprintf("%d", a.Metadata.Generation)},
		{"resource_version", a.Metadata.ResourceVersion},
		{"conditions", summarizeConditions(a.Status.Conditions)},
		{"observed_generation", fmt.Sprintf("%d", a.Status.ObservedGeneration)},
		{"pid", fmt.Sprintf("%d", a.Status.CurrentPid)},
		{"port", fmt.Sprintf("%d", port)},
		{"command", command},
		{"args", strings.Join(derefSlice(a.Spec.Args), " ")},
		{"runtime", derefStr(a.Spec.Runtime)},
		{"net_ip", derefStr(a.Status.NetIp)},
		{"uptime_ms", fmt.Sprintf("%d", a.Status.UptimeMs)},
		{"restart_count", fmt.Sprintf("%d", a.Status.RestartCount)},
		{"health_failures", fmt.Sprintf("%d", a.Status.HealthFailures)},
	}
	for _, f := range fields {
		if f[1] == "" {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\n", f[0], f[1])
	}
	return tw.Flush()
}

// summarizeConditions renders the conditions slice as a single
// "Ready=True Progressing=False ..." line for tabular human display.
// Machine consumers should use --output=json which dumps the full
// {type, status, lastTransitionTime, reason, message} tuple.
func summarizeConditions(conds []apitypes.Condition) string {
	if len(conds) == 0 {
		return ""
	}
	parts := make([]string, 0, len(conds))
	for _, c := range conds {
		parts = append(parts, fmt.Sprintf("%s=%s", c.Type, c.Status))
	}
	return strings.Join(parts, " ")
}

// --- pointer helpers for apitypes -----------------------------------

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// ptrSlice returns a *[]string from a []string. Returns nil for empty/nil
// slices so omitempty works as expected.
func ptrSlice(s []string) *[]string {
	if len(s) == 0 {
		return nil
	}
	return &s
}

// ptrRuntime converts a raw runtime string to *apitypes.Runtime.
// Returns nil for empty strings so omitempty elides the field.
func ptrRuntime(s string) *apitypes.Runtime {
	if s == "" {
		return nil
	}
	r := apitypes.Runtime(s)
	return &r
}

// derefStr returns the string behind p, or "" if p is nil.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// derefRuntimeStr converts *apitypes.Runtime to a plain string.
func derefRuntimeStr(p *apitypes.Runtime) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

// derefInt64 returns the int64 behind p, or 0 if p is nil.
func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// derefSlice returns the []string behind p, or nil if p is nil.
func derefSlice(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}
