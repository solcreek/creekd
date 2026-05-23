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
	"io"
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
// The nested `adapter` object is strict too; only `name` and
// `version` are allowed and both must be non-empty when present.
// Adapter-private extension space is deliberately NOT in the
// `adapter` object — a future top-level `metadata map[string]any`
// is the planned slot for that, added when concrete demand appears.
//
// Future *canonical* fields can be added here as optional
// `omitempty` without breaking newer-creekd-reads-older-manifest;
// the other direction — older creekd reading a manifest that
// carries a new field — requires a coordinated rollout (bump
// creekd first, then adapters) because strict mode rejects
// unknown keys.
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
	// that fails downstream with a cryptic error.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	// json.Decoder.Decode reads one value and stops. Without this
	// follow-up check, a payload like `{"valid":...}{"trailing":1}`
	// would be silently accepted on the Go side while TS's JSON.parse
	// rejects it.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		if err == nil {
			return nil, errors.New("unexpected trailing content after manifest object")
		}
		return nil, fmt.Errorf("trailing content after manifest: %w", err)
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
	if m.Adapter != nil {
		if m.Adapter.Name == "" {
			return nil, errors.New("adapter.name must be a non-empty string")
		}
		if m.Adapter.Version == "" {
			return nil, errors.New("adapter.version must be a non-empty string")
		}
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
// Platform-independent on purpose: filepath.IsAbs / filepath.Clean
// only treat the host OS's separator as meaningful, so they miss
// Windows drive paths on Linux/macOS and backslash traversal on any
// OS. We walk segments split on both `/` and `\` and detect the
// known absolute-path forms by hand so the Go and TS validators
// agree regardless of where they run.
func validateEntrypoint(ep string) error {
	// Posix-absolute.
	if strings.HasPrefix(ep, "/") {
		return fmt.Errorf("entrypoint %q must be relative to the project root, not absolute", ep)
	}
	// Windows UNC ("\\server\share" or, less common, "//server/share").
	if strings.HasPrefix(ep, `\\`) || strings.HasPrefix(ep, "//") {
		return fmt.Errorf("entrypoint %q must be relative to the project root, not absolute", ep)
	}
	// Windows drive-letter absolute: C:\, c:/, etc.
	if len(ep) >= 3 && ep[1] == ':' && (ep[2] == '/' || ep[2] == '\\') {
		c := ep[0]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			return fmt.Errorf("entrypoint %q must be relative to the project root, not absolute", ep)
		}
	}
	// Segment-walk with depth counter. Reject as soon as we'd
	// resolve above the project root.
	segments := strings.FieldsFunc(ep, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	depth := 0
	for _, seg := range segments {
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." {
			depth--
			if depth < 0 {
				return fmt.Errorf("entrypoint %q escapes the project directory via ..", ep)
			}
			continue
		}
		depth++
	}
	return nil
}
