package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solcreek/creekd/internal/adminapi"
)

// writeManifest creates a .creek-creekd/manifest.json inside a tempdir
// project tree and returns its absolute path plus the project root.
func writeManifest(t *testing.T, body string) (manifestPath, projectDir string) {
	t.Helper()
	projectDir = t.TempDir()
	manifestDir := filepath.Join(projectDir, ".creek-creekd")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifestPath = filepath.Join(manifestDir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return manifestPath, projectDir
}

const goodManifest = `{
  "version": 1,
  "framework": "nextjs",
  "target": "creekd",
  "buildId": "test-build",
  "nextVersion": "16.2.3",
  "adapter": {"name": "@solcreek/adapter-creekd", "version": "0.1.0"},
  "hasMiddleware": false,
  "hasPrerender": true,
  "runtime": "bun",
  "entrypoint": ".next/standalone/server.js",
  "port": 18900,
  "serveDirs": [".next/standalone"]
}`

func TestLoadManifestHappy(t *testing.T) {
	mp, projectDir := writeManifest(t, goodManifest)
	m, gotProjectDir, err := loadManifest(mp)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if gotProjectDir != projectDir {
		t.Errorf("projectDir = %q, want %q", gotProjectDir, projectDir)
	}
	if m.Version != 1 {
		t.Errorf("Version = %d, want 1", m.Version)
	}
	if m.Runtime != "bun" {
		t.Errorf("Runtime = %q, want bun", m.Runtime)
	}
	if m.Entrypoint != ".next/standalone/server.js" {
		t.Errorf("Entrypoint = %q", m.Entrypoint)
	}
	if m.Port != 18900 {
		t.Errorf("Port = %d, want 18900", m.Port)
	}
}

func TestLoadManifestRejectsWrongVersion(t *testing.T) {
	body := strings.Replace(goodManifest, `"version": 1`, `"version": 999`, 1)
	mp, _ := writeManifest(t, body)
	_, _, err := loadManifest(mp)
	if err == nil || !strings.Contains(err.Error(), "unsupported manifest version") {
		t.Errorf("want 'unsupported manifest version' error, got %v", err)
	}
}

func TestLoadManifestRejectsWrongTarget(t *testing.T) {
	body := strings.Replace(goodManifest, `"target": "creekd"`, `"target": "cloudflare"`, 1)
	mp, _ := writeManifest(t, body)
	_, _, err := loadManifest(mp)
	if err == nil || !strings.Contains(err.Error(), "not 'creekd'") {
		t.Errorf("want target-mismatch error, got %v", err)
	}
}

func TestLoadManifestRejectsBadRuntime(t *testing.T) {
	body := strings.Replace(goodManifest, `"runtime": "bun"`, `"runtime": "python"`, 1)
	mp, _ := writeManifest(t, body)
	_, _, err := loadManifest(mp)
	if err == nil || !strings.Contains(err.Error(), "not 'bun' or 'node'") {
		t.Errorf("want bad-runtime error, got %v", err)
	}
}

func TestLoadManifestRejectsPortOutOfRange(t *testing.T) {
	body := strings.Replace(goodManifest, `"port": 18900`, `"port": 70000`, 1)
	mp, _ := writeManifest(t, body)
	_, _, err := loadManifest(mp)
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("want port-range error, got %v", err)
	}
}

func TestLoadManifestRejectsMissingEntrypoint(t *testing.T) {
	body := strings.Replace(goodManifest, `"entrypoint": ".next/standalone/server.js"`, `"entrypoint": ""`, 1)
	mp, _ := writeManifest(t, body)
	_, _, err := loadManifest(mp)
	if err == nil || !strings.Contains(err.Error(), "missing entrypoint") {
		t.Errorf("want missing-entrypoint error, got %v", err)
	}
}

func TestApplyManifestSeedsEmptyRequest(t *testing.T) {
	_, projectDir := writeManifest(t, goodManifest)
	m := &CreekdManifest{
		Version:    1,
		Framework:  "nextjs",
		Target:     "creekd",
		Runtime:    "bun",
		Entrypoint: ".next/standalone/server.js",
		Port:       18900,
	}
	req := adminapi.SpawnRequest{ID: "myapp"}
	applyManifestTo(&req, m, projectDir)

	if req.Runtime != "bun" {
		t.Errorf("Runtime = %q, want bun", req.Runtime)
	}
	wantEntry := filepath.Join(projectDir, ".next", "standalone", "server.js")
	if req.Entry != wantEntry {
		t.Errorf("Entry = %q, want %q", req.Entry, wantEntry)
	}
	if req.Port != 18900 {
		t.Errorf("Port = %d, want 18900", req.Port)
	}
}

func TestApplyManifestRespectsCLIOverrides(t *testing.T) {
	_, projectDir := writeManifest(t, goodManifest)
	m := &CreekdManifest{
		Version:    1,
		Framework:  "nextjs",
		Target:     "creekd",
		Runtime:    "bun",
		Entrypoint: ".next/standalone/server.js",
		Port:       18900,
	}
	// User passed --port 9999 --runtime node --entry /tmp/other.js.
	req := adminapi.SpawnRequest{
		ID:      "myapp",
		Runtime: "node",
		Entry:   "/tmp/other.js",
		Port:    9999,
	}
	applyManifestTo(&req, m, projectDir)

	if req.Runtime != "node" {
		t.Errorf("Runtime = %q, want node (CLI override)", req.Runtime)
	}
	if req.Entry != "/tmp/other.js" {
		t.Errorf("Entry = %q, want /tmp/other.js (CLI override)", req.Entry)
	}
	if req.Port != 9999 {
		t.Errorf("Port = %d, want 9999 (CLI override)", req.Port)
	}
}

func TestLoadManifestReturnsHelpfulErrorOnMissingFile(t *testing.T) {
	_, _, err := loadManifest("/tmp/does-not-exist-creekctl-test/manifest.json")
	if err == nil {
		t.Fatal("want error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "--from: read") {
		t.Errorf("want '--from: read' in error, got %v", err)
	}
}

func TestLoadManifestRejectsMalformedJSON(t *testing.T) {
	mp, _ := writeManifest(t, `{ not even close to json`)
	_, _, err := loadManifest(mp)
	if err == nil {
		t.Fatal("want error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("want 'parse' in error, got %v", err)
	}
}
