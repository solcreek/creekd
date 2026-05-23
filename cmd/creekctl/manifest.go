package main

// Manifest-aware seeding helpers for creekctl's up/ensure/deploy:
// fold a parsed manifest into the admin API request types,
// preserving CLI-flag precedence.

import (
	"path/filepath"

	"github.com/solcreek/creekd/api/manifest"
	"github.com/solcreek/creekd/internal/adminapi"
)

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
func applyManifestTo(req *adminapi.SpawnRequest, m *manifest.Manifest, projectDir string) {
	if req.Runtime == "" {
		req.Runtime = string(m.Runtime)
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
// same precedence (CLI flags win), same fields seeded. Used when
// `creekctl deploy --from` is invoked to push a fresh adapter
// manifest at an already-running app. Kept as a separate function
// instead of a generic over both request types because the
// duplication is six lines and avoiding it would require an
// interface or a wrapper that obscures the CLI flag precedence rule.
func applyManifestToDeploy(req *adminapi.DeployRequest, m *manifest.Manifest, projectDir string) {
	if req.Runtime == "" {
		req.Runtime = string(m.Runtime)
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
