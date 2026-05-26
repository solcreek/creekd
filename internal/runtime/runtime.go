package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Runtime is the JS/TS engine used to execute a supervised app.
type Runtime string

const (
	Bun     Runtime = "bun"
	Node    Runtime = "node"
	Deno    Runtime = "deno"
	Workers Runtime = "workers"
)

// All returns every supported runtime in a stable order.
func All() []Runtime { return []Runtime{Bun, Node, Deno, Workers} }

// Valid reports whether r is one of the known runtimes.
func (r Runtime) Valid() bool {
	switch r {
	case Bun, Node, Deno, Workers:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (r Runtime) String() string { return string(r) }

// Parse returns the Runtime named by s (case-insensitive) or an error.
func Parse(s string) (Runtime, error) {
	switch Runtime(strings.ToLower(strings.TrimSpace(s))) {
	case Bun:
		return Bun, nil
	case Node:
		return Node, nil
	case Deno:
		return Deno, nil
	case Workers:
		return Workers, nil
	}
	return "", fmt.Errorf("runtime: unknown runtime %q", s)
}

// ErrEmptyEntry is returned by Command when entry is empty.
var ErrEmptyEntry = errors.New("runtime: empty entry")

// ErrNoSignal is returned by Detect when a directory has none of the
// recognised runtime fingerprints. Callers should treat this as
// "default to Node" only after explicit confirmation that the project
// is JS/TS in the first place.
var ErrNoSignal = errors.New("runtime: no detection signal")

// Command resolves a Runtime + entry script (+ optional extra args) to
// the executable and argv that exec.Cmd should run.
//
//   - Bun:  bun <entry> [args...]
//   - Node: node <entry> [args...]
//   - Deno: deno run -A <entry> [args...]
//
// Deno is invoked with -A (allow-all) because process isolation in
// Creek is enforced at the OS level (cgroup + UID + filesystem),
// not via Deno's permission model.
func Command(r Runtime, entry string, extraArgs []string) (string, []string, error) {
	if entry == "" {
		return "", nil, ErrEmptyEntry
	}
	if !r.Valid() {
		return "", nil, fmt.Errorf("runtime: invalid runtime %q", r)
	}
	switch r {
	case Bun:
		args := append([]string{entry}, extraArgs...)
		return "bun", args, nil
	case Node:
		args := append([]string{entry}, extraArgs...)
		return "node", args, nil
	case Deno:
		args := append([]string{"run", "-A", entry}, extraArgs...)
		return "deno", args, nil
	case Workers:
		args := append([]string{"serve", entry}, extraArgs...)
		return "workerd", args, nil
	}
	return "", nil, fmt.Errorf("runtime: unreachable %q", r)
}

// Detect inspects dir and returns the best-guess Runtime based on file
// fingerprints. Precedence (highest first):
//
//  1. deno.json or deno.jsonc   → Deno
//  2. bun.lockb                 → Bun
//  3. package.json with bun in
//     dev/dependencies          → Bun
//  4. package.json              → Node
//  5. nothing recognised        → "", ErrNoSignal
//
// Detect does not currently scan source files for `bun:sqlite` imports
// — manifests are sufficient signal for the v1 deploy flow and avoid
// the cost of parsing TypeScript.
func Detect(dir string) (Runtime, error) {
	if exists(filepath.Join(dir, "deno.json")) || exists(filepath.Join(dir, "deno.jsonc")) {
		return Deno, nil
	}
	if exists(filepath.Join(dir, "bun.lockb")) {
		return Bun, nil
	}
	pkgPath := filepath.Join(dir, "package.json")
	if !exists(pkgPath) {
		return "", ErrNoSignal
	}
	if usesBun(pkgPath) {
		return Bun, nil
	}
	return Node, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// usesBun returns true if the package.json at path declares any
// dependency whose name suggests Bun (bun-types, @types/bun, bun).
// Errors and missing dependency sections are treated as "no".
func usesBun(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	for _, deps := range []map[string]string{pkg.Dependencies, pkg.DevDependencies} {
		for name := range deps {
			switch name {
			case "bun", "bun-types", "@types/bun":
				return true
			}
		}
	}
	return false
}
