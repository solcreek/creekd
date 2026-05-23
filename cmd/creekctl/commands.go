package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/solcreek/creekd/api/manifest"
	"github.com/solcreek/creekd/internal/adminclient"
	"github.com/solcreek/creekd/internal/apitypes"
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
	return writeAppDetail(w, app)
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
	app, err := client.Spawn(ctx, req)
	if err != nil && adminclient.IsAlreadyRunning(err) {
		app, err = client.Get(ctx, id)
	}
	if err != nil {
		return err
	}
	if cf.json {
		return writeJSON(w, app)
	}
	return writeAppDetail(w, app)
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

	// Inherit app env vars via creekctl exec-style injection
	client := cf.client()
	app, err := client.Get(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("release: cannot get app env: %w", err)
	}
	cmd.Env = append(os.Environ(), derefSlice(app.Env)...)
	cmd.Env = append(cmd.Env, fmt.Sprintf("PORT=%d", app.Port))

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

	// Get the app's env vars from the running supervisor
	client := cf.client()
	var app *apitypes.AppView
	var err error

	if *appID != "" {
		app, err = client.Get(ctx, *appID)
	} else {
		apps, listErr := client.List(ctx)
		if listErr != nil {
			return listErr
		}
		if len(apps) == 0 {
			return fmt.Errorf("exec: no apps running")
		}
		app = &apps[0]
	}
	if err != nil {
		return err
	}

	// Build env: inherit current env + inject app env vars (DATABASE_URL, etc)
	env := os.Environ()
	env = append(env, derefSlice(app.Env)...)
	env = append(env, fmt.Sprintf("PORT=%d", app.Port))

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
		{"runtime", derefStr(a.Runtime)},
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
