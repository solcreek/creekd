package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/solcreek/creekd/internal/adminapi"
	"github.com/solcreek/creekd/internal/adminclient"
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
	"rm":      {Name: "rm", Description: "stop an app", Run: runRM},
	"restart": {Name: "restart", Description: "cycle an app", Run: runRestart},
	"reset":   {Name: "reset", Description: "clear crash-loop", Run: runReset},
	"deploy":  {Name: "deploy", Description: "blue-green deploy", Run: runDeploy},
	"logs":    {Name: "logs", Description: "tail per-app log", Run: runLogs},
}

// commonFlags holds the global knobs every subcommand accepts. They
// are registered on each subcommand's flag set rather than parsed
// globally so a typo before the subcommand still produces a clear
// usage message at that subcommand's scope.
type commonFlags struct {
	server string
	token  string
	json   bool
}

// register attaches --server / --token / --json onto fs and seeds
// defaults from environment.
func (cf *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&cf.server, "server", os.Getenv("CREEKCTL_SERVER"),
		"admin API base URL (or $CREEKCTL_SERVER)")
	fs.StringVar(&cf.token, "token", os.Getenv("CREEKCTL_TOKEN"),
		"bearer token (or $CREEKCTL_TOKEN)")
	fs.BoolVar(&cf.json, "json", false, "JSON output")
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
	if err := fs.Parse(argv); err != nil {
		return err
	}
	apps, err := cf.client().List(ctx)
	if err != nil {
		return err
	}
	if cf.json {
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
	if err := fs.Parse(rest); err != nil {
		return err
	}
	app, err := cf.client().Get(ctx, id)
	if err != nil {
		return err
	}
	if cf.json {
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
	var (
		command    = fs.String("command", "", "executable to run (explicit mode)")
		entry      = fs.String("entry", "", "entry script (with --runtime)")
		runtimeArg = fs.String("runtime", "", "bun|node|deno (with --entry)")
		port       = fs.Int("port", 0, "dispatch port the app listens on")
		args       stringSliceFlag
		env        stringSliceFlag
		netIso     = fs.Bool("net-isolation", false, "spawn inside a per-app netns")
	)
	fs.Var(&args, "arg", "argument passed to the command (repeat for multiple)")
	fs.Var(&env, "env", "environment variable KEY=VAL (repeat for multiple)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	req := adminapi.SpawnRequest{
		ID:           id,
		Command:      *command,
		Entry:        *entry,
		Runtime:      *runtimeArg,
		Args:         args,
		Env:          env,
		Port:         *port,
		NetIsolation: *netIso,
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
	app, err := cf.client().Restart(ctx, id, adminapi.RestartRequest{TimeoutMS: *timeoutMS})
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
	var (
		command    = fs.String("command", "", "executable to run (explicit mode)")
		entry      = fs.String("entry", "", "entry script (with --runtime)")
		runtimeArg = fs.String("runtime", "", "bun|node|deno")
		port       = fs.Int("port", 0, "v2 port (must differ from v1's)")
		args       stringSliceFlag
		env        stringSliceFlag
		readyMS    = fs.Int64("ready-timeout-ms", 0, "max wait for v2 to be healthy")
		netIso     = fs.Bool("net-isolation", false, "spawn v2 inside a netns")
	)
	fs.Var(&args, "arg", "argument (repeat for multiple)")
	fs.Var(&env, "env", "environment KEY=VAL (repeat for multiple)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	req := adminapi.DeployRequest{
		Command:        *command,
		Entry:          *entry,
		Runtime:        *runtimeArg,
		Args:           args,
		Env:            env,
		Port:           *port,
		ReadyTimeoutMS: *readyMS,
		NetIsolation:   *netIso,
	}
	app, err := cf.client().Deploy(ctx, id, req)
	if err != nil {
		return err
	}
	if cf.json {
		return writeJSON(w, app)
	}
	return writeAppDetail(w, app)
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

// --- output helpers -----------------------------------------------

// writeJSON encodes v as indented JSON to w.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeAppTable renders a list of apps as a tab-aligned table.
func writeAppTable(w io.Writer, apps []adminapi.AppView) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tPID\tPORT\tNET_IP\tUPTIME_MS\tRESTARTS\tHEALTH_FAILS")
	for _, a := range apps {
		netIP := a.NetIP
		if netIP == "" {
			netIP = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%d\t%d\t%d\n",
			a.ID, a.Status, a.PID, a.Port, netIP,
			a.UptimeMS, a.RestartCount, a.HealthFailures)
	}
	return tw.Flush()
}

// writeAppDetail renders a single app as aligned key/value pairs.
func writeAppDetail(w io.Writer, a *adminapi.AppView) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fields := [][2]string{
		{"id", a.ID},
		{"status", a.Status},
		{"pid", fmt.Sprintf("%d", a.PID)},
		{"port", fmt.Sprintf("%d", a.Port)},
		{"command", a.Command},
		{"args", strings.Join(a.Args, " ")},
		{"runtime", a.Runtime},
		{"net_ip", a.NetIP},
		{"uptime_ms", fmt.Sprintf("%d", a.UptimeMS)},
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
