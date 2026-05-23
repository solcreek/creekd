// Package manifest defines the process-level deployment manifest
// adapters write into .creek-creekd/manifest.json and creekd reads
// via `creekctl up --from`.
//
// It is intentionally framework-neutral: Next.js, SvelteKit, Astro,
// Hono, or a hand-written Bun server all describe the same few
// process fields here. Framework-specific metadata is preserved as
// optional informational fields that creekd treats as opaque.
//
// This package is the **canonical Go side of the contract**. The
// matching TypeScript types live at packages/creekd-manifest/ under
// the same repo so the two languages can't drift; CI runs the same
// testdata corpus through both validators.
package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/solcreek/creekd/internal/runtime"
)

// Manifest is the process-level manifest shape that creekctl can
// translate into SpawnRequest / DeployRequest.
//
// Top-level field set is considered stable and is **strictly
// validated** — unknown top-level keys are rejected to catch typos
// like "entryPont" before they cause cryptic spawn-time failures.
// Adapters that need to carry extra metadata should put it inside
// the `adapter` object; creekd-side extension space (a future
// top-level `metadata map[string]any`) is reserved.
//
// Future *canonical* fields can be added here as optional `omitempty`
// without breaking older adapters (they simply won't write them); a
// newer creekd reading an older manifest still works.
type Manifest struct {
	Version         int              `json:"version"`
	Framework       string           `json:"framework,omitempty"`
	Target          string           `json:"target"`
	BuildID         string           `json:"buildId,omitempty"`
	NextVersion     string           `json:"nextVersion,omitempty"`
	Runtime         runtime.Runtime  `json:"runtime"`
	Entrypoint      string           `json:"entrypoint"`
	Port            int              `json:"port"`
	Env             []string         `json:"env,omitempty"`
	HealthCheckPath string           `json:"health_check_path,omitempty"`
	HasMiddleware   bool             `json:"hasMiddleware,omitempty"`
	HasPrerender    bool             `json:"hasPrerender,omitempty"`
	ServeDirs       []string         `json:"serveDirs,omitempty"`
	Adapter         *AdapterMetadata `json:"adapter,omitempty"`
}

// AdapterMetadata identifies which adapter wrote the manifest. Useful
// for support / debugging — creekd does not act on these values.
// Strict-validated: only `name` and `version` are accepted. If an
// adapter needs to carry extra information, a top-level
// `metadata map[string]any` extension slot is the planned escape
// hatch (not added pre-emptively — YAGNI).
type AdapterMetadata struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Load reads and validates a manifest from disk. Returns the parsed
// manifest plus the absolute project directory it came from
// (manifest.json lives inside .creek-creekd/ at the project root, so
// projectDir is the file's grandparent).
func Load(path string) (*Manifest, string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("--from: resolve path: %w", err)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, "", fmt.Errorf("--from: read %s: %w", absPath, err)
	}
	m, err := Decode(data)
	if err != nil {
		return nil, "", fmt.Errorf("--from: %s: %w", absPath, err)
	}

	// projectDir = dirname(dirname(manifestPath))
	manifestDir := filepath.Dir(absPath)
	projectDir := filepath.Dir(manifestDir)

	return m, projectDir, nil
}

// Decode validates a manifest from in-memory bytes. It is the same
// validator Load uses internally — exposed so callers that already
// have the bytes (corpus tests, future HTTP control-plane endpoints,
// adapter-side dry runs) don't have to round-trip through the
// filesystem.
//
// Errors are intentionally not prefixed with the source path; Load
// adds that context when called from a file. Direct callers can
// wrap with their own context.
func Decode(data []byte) (*Manifest, error) {
	// Strict decode: unknown top-level fields are rejected so a typo
	// like "entryPont" doesn't silently produce an empty Entrypoint
	// that fails downstream with a cryptic error. Adapter extensions
	// live inside the `adapter` object, not at the top level.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	if m.Version != 1 {
		return nil, fmt.Errorf("unsupported manifest version %d (only v1 is supported by this creekctl)", m.Version)
	}
	if m.Target != "creekd" {
		return nil, fmt.Errorf("target=%q is not 'creekd' — this manifest was written for a different deployment target", m.Target)
	}
	if !m.Runtime.Valid() {
		return nil, fmt.Errorf("runtime=%q is not 'bun', 'node', or 'deno'", m.Runtime)
	}
	if m.Entrypoint == "" {
		return nil, errors.New("missing entrypoint")
	}
	if err := validateEntrypoint(m.Entrypoint); err != nil {
		return nil, err
	}
	if m.Port <= 0 || m.Port > 65535 {
		return nil, fmt.Errorf("port=%d out of range (1..65535)", m.Port)
	}
	return &m, nil
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
