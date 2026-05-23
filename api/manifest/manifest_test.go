package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// goodManifest is the same fixture the cross-language corpus uses,
// loaded from testdata/valid/nextjs-full.json so updates to the
// fixture flow through unit tests too. Tests that need a mutated
// variant strings.Replace specific tokens in this base.
var goodManifest = func() string {
	body, err := corpusFS.ReadFile("testdata/valid/nextjs-full.json")
	if err != nil {
		panic("corpus_test embed missing nextjs-full.json: " + err.Error())
	}
	return string(body)
}()

func TestLoadHappy(t *testing.T) {
	mp, projectDir := writeManifest(t, goodManifest)
	m, gotProjectDir, err := Load(mp)
	if err != nil {
		t.Fatalf("Load: %v", err)
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
	if m.Port != 3000 {
		t.Errorf("Port = %d, want 3000", m.Port)
	}
}

func TestLoadRejectsWrongVersion(t *testing.T) {
	body := strings.Replace(goodManifest, `"version": 1`, `"version": 999`, 1)
	mp, _ := writeManifest(t, body)
	_, _, err := Load(mp)
	if err == nil || !strings.Contains(err.Error(), "unsupported manifest version") {
		t.Errorf("want 'unsupported manifest version' error, got %v", err)
	}
}

func TestLoadRejectsWrongTarget(t *testing.T) {
	body := strings.Replace(goodManifest, `"target": "creekd"`, `"target": "cloudflare"`, 1)
	mp, _ := writeManifest(t, body)
	_, _, err := Load(mp)
	if err == nil || !strings.Contains(err.Error(), "not 'creekd'") {
		t.Errorf("want target-mismatch error, got %v", err)
	}
}

func TestLoadRejectsBadRuntime(t *testing.T) {
	body := strings.Replace(goodManifest, `"runtime": "bun"`, `"runtime": "python"`, 1)
	mp, _ := writeManifest(t, body)
	_, _, err := Load(mp)
	if err == nil || !strings.Contains(err.Error(), "not 'bun', 'node', or 'deno'") {
		t.Errorf("want bad-runtime error, got %v", err)
	}
}

func TestLoadAllowsNonNextFrameworkMetadata(t *testing.T) {
	body := strings.Replace(goodManifest, `"framework": "nextjs"`, `"framework": "astro"`, 1)
	body = strings.Replace(body, `"runtime": "bun"`, `"runtime": "deno"`, 1)
	body = strings.Replace(body, `"entrypoint": ".next/standalone/server.js"`, `"entrypoint": "server.ts"`, 1)
	mp, _ := writeManifest(t, body)
	m, _, err := Load(mp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Framework != "astro" {
		t.Errorf("Framework = %q, want astro", m.Framework)
	}
	if m.Runtime != "deno" {
		t.Errorf("Runtime = %q, want deno", m.Runtime)
	}
}

func TestLoadRejectsPortOutOfRange(t *testing.T) {
	body := strings.Replace(goodManifest, `"port": 3000`, `"port": 70000`, 1)
	mp, _ := writeManifest(t, body)
	_, _, err := Load(mp)
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("want port-range error, got %v", err)
	}
}

func TestLoadRejectsMissingEntrypoint(t *testing.T) {
	body := strings.Replace(goodManifest, `"entrypoint": ".next/standalone/server.js"`, `"entrypoint": ""`, 1)
	mp, _ := writeManifest(t, body)
	_, _, err := Load(mp)
	if err == nil || !strings.Contains(err.Error(), "missing entrypoint") {
		t.Errorf("want missing-entrypoint error, got %v", err)
	}
}

// Absolute entrypoint in the manifest is rejected — the adapter
// always writes relative paths; an absolute one means corruption or
// tampering and would resolve outside the project root once joined.
func TestLoadRejectsAbsoluteEntrypoint(t *testing.T) {
	body := strings.Replace(goodManifest,
		`"entrypoint": ".next/standalone/server.js"`,
		`"entrypoint": "/etc/passwd"`, 1)
	mp, _ := writeManifest(t, body)
	_, _, err := Load(mp)
	if err == nil {
		t.Fatal("want error for absolute entrypoint, got nil")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error %q should mention 'absolute'", err)
	}
}

// Parent-dir traversal (".." segments after Clean) is rejected for
// the same reason. Covers both ../ and the rarer ..\ separator that
// could arrive from a Windows-authored manifest.
func TestLoadRejectsTraversalEntrypoint(t *testing.T) {
	for _, ep := range []string{
		"../escape.js",
		"./../escape.js",
		".next/../../escape.js",
		"..",
	} {
		body := strings.Replace(goodManifest,
			`"entrypoint": ".next/standalone/server.js"`,
			`"entrypoint": "`+ep+`"`, 1)
		mp, _ := writeManifest(t, body)
		_, _, err := Load(mp)
		if err == nil {
			t.Errorf("entrypoint %q: want error, got nil", ep)
			continue
		}
		if !strings.Contains(err.Error(), "escape") {
			t.Errorf("entrypoint %q: error %q should mention 'escape'", ep, err)
		}
	}
}

func TestLoadReturnsHelpfulErrorOnMissingFile(t *testing.T) {
	_, _, err := Load("/tmp/does-not-exist-creekctl-test/manifest.json")
	if err == nil {
		t.Fatal("want error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "--from: read") {
		t.Errorf("want '--from: read' in error, got %v", err)
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	mp, _ := writeManifest(t, `{ not even close to json`)
	_, _, err := Load(mp)
	if err == nil {
		t.Fatal("want error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("want 'parse' in error, got %v", err)
	}
}

// TestLoadRejectsUnknownTopLevelField confirms strict decoding —
// typos in canonical field names (entryPont vs entrypoint) fail at
// load time with a descriptive error rather than silently producing
// a manifest with Entrypoint="" that explodes later.
func TestLoadRejectsUnknownTopLevelField(t *testing.T) {
	body := strings.Replace(goodManifest,
		`"entrypoint": ".next/standalone/server.js"`,
		`"entryPont": ".next/standalone/server.js"`, 1)
	mp, _ := writeManifest(t, body)
	_, _, err := Load(mp)
	if err == nil {
		t.Fatal("want error for unknown field 'entryPont', got nil")
	}
	if !strings.Contains(err.Error(), "entryPont") {
		t.Errorf("error %q should mention the misspelled field 'entryPont'", err)
	}
}

// TestLoadParsesExtensionFields confirms the surface fields beyond
// the minimum (adapter, hasPrerender, serveDirs, etc.) are actually
// captured into the struct — previously these were silently dropped
// under lenient parsing, which is now visible.
func TestLoadParsesExtensionFields(t *testing.T) {
	mp, _ := writeManifest(t, goodManifest)
	m, _, err := Load(mp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Adapter == nil {
		t.Fatal("Adapter is nil, want parsed metadata")
	}
	if m.Adapter.Name != "@solcreek/adapter-creekd" {
		t.Errorf("Adapter.Name = %q", m.Adapter.Name)
	}
	if m.Adapter.Version != "0.1.0" {
		t.Errorf("Adapter.Version = %q", m.Adapter.Version)
	}
	if !m.HasPrerender {
		t.Error("HasPrerender = false, want true")
	}
	if m.HasMiddleware {
		t.Error("HasMiddleware = true, want false")
	}
	if len(m.ServeDirs) != 1 || m.ServeDirs[0] != ".next/standalone" {
		t.Errorf("ServeDirs = %v", m.ServeDirs)
	}
	if m.NextVersion != "16.2.3" {
		t.Errorf("NextVersion = %q", m.NextVersion)
	}
	if m.BuildID != "test-build-id" {
		t.Errorf("BuildID = %q", m.BuildID)
	}
}
