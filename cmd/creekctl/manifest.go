package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/solcreek/creekd/internal/adminapi"
)

// CreekdManifest is the process-level manifest shape that creekctl can
// translate into SpawnRequest / DeployRequest. It is intentionally
// framework-neutral: Next.js, Astro, Hono, or a hand-written Bun server
// should all describe the same few process fields here.
//
// Future adapter-specific metadata is ignored so an older creekctl keeps
// working against a newer manifest.
type CreekdManifest struct {
	Version         int      `json:"version"`
	Framework       string   `json:"framework,omitempty"`
	Target          string   `json:"target"`
	BuildID         string   `json:"buildId,omitempty"`
	Runtime         string   `json:"runtime"`
	Entrypoint      string   `json:"entrypoint"`
	Port            int      `json:"port"`
	Env             []string `json:"env,omitempty"`
	HealthCheckPath string   `json:"health_check_path,omitempty"`
}

// loadManifest reads and validates a CreekdManifest from path. Returns
// the parsed manifest plus the absolute project directory it came
// from. The project directory is the manifest file's grandparent
// (manifest.json lives inside .creek-creekd/ at the project root).
func loadManifest(path string) (*CreekdManifest, string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("--from: resolve path: %w", err)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, "", fmt.Errorf("--from: read %s: %w", absPath, err)
	}
	var m CreekdManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, "", fmt.Errorf("--from: parse %s: %w", absPath, err)
	}

	if m.Version != 1 {
		return nil, "", fmt.Errorf("--from: %s: unsupported manifest version %d (only v1 is supported by this creekctl)",
			absPath, m.Version)
	}
	if m.Target != "creekd" {
		return nil, "", fmt.Errorf("--from: %s: target=%q is not 'creekd' — this manifest was written for a different deployment target",
			absPath, m.Target)
	}
	switch m.Runtime {
	case "bun", "node", "deno":
		// ok
	default:
		return nil, "", fmt.Errorf("--from: %s: runtime=%q is not 'bun', 'node', or 'deno'", absPath, m.Runtime)
	}
	if m.Entrypoint == "" {
		return nil, "", errors.New("--from: missing entrypoint")
	}
	if err := validateEntrypoint(m.Entrypoint); err != nil {
		return nil, "", fmt.Errorf("--from: %s: %w", absPath, err)
	}
	if m.Port <= 0 || m.Port > 65535 {
		return nil, "", fmt.Errorf("--from: port=%d out of range (1..65535)", m.Port)
	}

	// projectDir = dirname(dirname(manifestPath))
	manifestDir := filepath.Dir(absPath)
	projectDir := filepath.Dir(manifestDir)

	return &m, projectDir, nil
}

// validateEntrypoint rejects entrypoints that could resolve outside
// the manifest's project directory. The adapter always writes a path
// relative to the project root (e.g. ".next/standalone/server.js"),
// so anything absolute or containing parent-dir traversal is either
// a corrupted manifest, a supply-chain compromise of the adapter, or
// a hand-edited file pointing somewhere unsafe.
//
// Even under creekctl's local trust model (the user runs it on their
// own dev machine against their own manifest), guarding here means
// that a future use case where manifests cross trust boundaries —
// e.g. a hosted control plane consuming customer-provided manifests
// — doesn't need a second layer of defence to be added later.
func validateEntrypoint(ep string) error {
	if filepath.IsAbs(ep) {
		return fmt.Errorf("entrypoint %q must be relative to the project root, not absolute", ep)
	}
	clean := filepath.Clean(ep)
	// Clean turns "./a/../../b" into "../b"; any leading ".." (or the
	// literal "..") in the cleaned form means the path escapes
	// projectDir once joined.
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, `..\`) {
		return fmt.Errorf("entrypoint %q escapes the project directory via ..", ep)
	}
	return nil
}

// applyManifestTo seeds the SpawnRequest from a manifest. CLI flags
// retain priority: any non-zero field in req is left alone; only
// fields the user did NOT set on the command line get filled in
// from the manifest.
//
// The supervisor itself injects PORT=<app.Port> into the child env
// at spawn time (see internal/supervisor/supervisor.go: startLocked),
// so runtime entrypoints that read process.env.PORT bind to the right
// port automatically. We don't add it here.
//
// projectDir is the manifest's project root, used to resolve the
// relative `entrypoint` into an absolute path.
func applyManifestTo(req *adminapi.SpawnRequest, m *CreekdManifest, projectDir string) {
	if req.Runtime == "" {
		req.Runtime = m.Runtime
	}
	if req.Entry == "" {
		req.Entry = filepath.Join(projectDir, m.Entrypoint)
	}
	if req.Port == 0 {
		req.Port = m.Port
	}
	if len(req.Env) == 0 && len(m.Env) > 0 {
		req.Env = append([]string(nil), m.Env...)
	}
	if req.HealthCheckPath == "" {
		req.HealthCheckPath = m.HealthCheckPath
	}
}

// applyManifestToDeploy is the redeploy mirror of applyManifestTo —
// same precedence (CLI flags win), same three fields seeded
// (runtime / entry / port). Used when `creekctl deploy --from` is
// invoked to push a fresh adapter manifest at an already-running
// app. Kept as a separate function instead of a generic over both
// request types because the duplication is six lines and avoiding
// it would require an interface or a wrapper that obscures the
// CLI flag precedence rule.
func applyManifestToDeploy(req *adminapi.DeployRequest, m *CreekdManifest, projectDir string) {
	if req.Runtime == "" {
		req.Runtime = m.Runtime
	}
	if req.Entry == "" {
		req.Entry = filepath.Join(projectDir, m.Entrypoint)
	}
	if req.Port == 0 {
		req.Port = m.Port
	}
	if len(req.Env) == 0 && len(m.Env) > 0 {
		req.Env = append([]string(nil), m.Env...)
	}
	if req.HealthCheckPath == "" {
		req.HealthCheckPath = m.HealthCheckPath
	}
}
