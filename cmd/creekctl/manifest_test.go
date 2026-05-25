package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/solcreek/creekd/api/manifest"
	"github.com/solcreek/creekd/internal/apitypes"
)

// Manifest parsing / validation behaviour is covered in
// api/manifest/manifest_test.go. The tests below only cover this
// package's job: folding a parsed manifest into the adminapi
// request types with CLI-flag precedence.

func TestApplyManifestSeedsEmptyRequest(t *testing.T) {
	projectDir := t.TempDir()
	m := &manifest.Manifest{
		Version:         1,
		Framework:       "nextjs",
		Target:          "creekd",
		Runtime:         "bun",
		Entrypoint:      ".next/standalone/server.js",
		Port:            18900,
		Env:             []string{"NODE_ENV=production"},
		HealthCheckPath: "/healthz",
	}
	req := apitypes.SpawnRequest{Id: "myapp"}
	applyManifestTo(&req, m, projectDir)

	if req.Runtime == nil || string(*req.Runtime) != "bun" {
		t.Errorf("Runtime = %v, want bun", req.Runtime)
	}
	wantEntry := filepath.Join(projectDir, ".next", "standalone", "server.js")
	if req.Entry == nil || *req.Entry != wantEntry {
		t.Errorf("Entry = %v, want %q", req.Entry, wantEntry)
	}
	if req.Port != 18900 {
		t.Errorf("Port = %d, want 18900", req.Port)
	}
	if req.Env == nil {
		t.Fatal("Env is nil, want NODE_ENV=production")
	}
	if got := strings.Join(*req.Env, ","); got != "NODE_ENV=production" {
		t.Errorf("Env = %q, want NODE_ENV=production", got)
	}
	if req.HealthCheckPath == nil || *req.HealthCheckPath != "/healthz" {
		t.Errorf("HealthCheckPath = %v, want /healthz", req.HealthCheckPath)
	}
}

func TestApplyManifestRespectsCLIOverrides(t *testing.T) {
	projectDir := t.TempDir()
	m := &manifest.Manifest{
		Version:         1,
		Framework:       "nextjs",
		Target:          "creekd",
		Runtime:         "bun",
		Entrypoint:      ".next/standalone/server.js",
		Port:            18900,
		Env:             []string{"NODE_ENV=production"},
		HealthCheckPath: "/healthz",
	}
	// User passed --port 9999 --runtime node --entry /tmp/other.js.
	nodeRuntime := apitypes.Runtime("node")
	cliEnv := []string{"CLI=1"}
	req := apitypes.SpawnRequest{
		Id:              "myapp",
		Runtime:         &nodeRuntime,
		Entry:           ptr("/tmp/other.js"),
		Port:            9999,
		Env:             &cliEnv,
		HealthCheckPath: ptr("/ready"),
	}
	applyManifestTo(&req, m, projectDir)

	if req.Runtime == nil || string(*req.Runtime) != "node" {
		t.Errorf("Runtime = %v, want node (CLI override)", req.Runtime)
	}
	if req.Entry == nil || *req.Entry != "/tmp/other.js" {
		t.Errorf("Entry = %v, want /tmp/other.js (CLI override)", req.Entry)
	}
	if req.Port != 9999 {
		t.Errorf("Port = %d, want 9999 (CLI override)", req.Port)
	}
	if req.Env == nil {
		t.Fatal("Env is nil, want CLI=1")
	}
	if got := strings.Join(*req.Env, ","); got != "CLI=1" {
		t.Errorf("Env = %q, want CLI=1 (CLI override)", got)
	}
	if req.HealthCheckPath == nil || *req.HealthCheckPath != "/ready" {
		t.Errorf("HealthCheckPath = %v, want /ready (CLI override)", req.HealthCheckPath)
	}
}
