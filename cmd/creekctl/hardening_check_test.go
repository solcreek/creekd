package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solcreek/creekd/internal/hardening"
)

// TestRunHardeningCheck_AcceptsCanonicalUnit covers the happy
// path: pointing the subcommand at a unit that contains every
// required directive yields a clean exit + a one-line summary.
func TestRunHardeningCheck_AcceptsCanonicalUnit(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "creekd.service")
	if err := os.WriteFile(tmp, []byte(canonicalUnit()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runSub(t, "hardening-check", []string{tmp})
	if err != nil {
		t.Errorf("clean unit: err = %v, want nil", err)
	}
	if !strings.Contains(out, "hardening clean") {
		t.Errorf("output missing summary: %q", out)
	}
}

// TestRunHardeningCheck_ReportsDriftAndExitsError covers the
// failure path: missing directives MUST be itemised AND the
// subcommand MUST return a non-nil error so CI / scripts can branch
// on the exit code.
func TestRunHardeningCheck_ReportsDriftAndExitsError(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "creekd.service")
	if err := os.WriteFile(tmp, []byte("[Service]\nNoNewPrivileges=true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runSub(t, "hardening-check", []string{tmp})
	if err == nil {
		t.Fatal("drifted unit: err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "systemd_hardening_drift") {
		t.Errorf("err = %v, want to mention systemd_hardening_drift", err)
	}
	// Each missing directive shown in the human output.
	for _, key := range []string{"ProtectSystem", "MemoryDenyWriteExecute"} {
		if !strings.Contains(out, key) {
			t.Errorf("output missing %s: %q", key, out)
		}
	}
}

// TestRunHardeningCheck_JSONOutputEmitsDriftArray covers the
// agent-facing JSON path: --json yields a parseable array of
// Drift records. The subcommand still returns a non-nil error so
// scripts piping JSON to jq can detect "drift present" via exit
// code without needing to count array entries.
func TestRunHardeningCheck_JSONOutputEmitsDriftArray(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "creekd.service")
	if err := os.WriteFile(tmp, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runSub(t, "hardening-check", []string{"--json", tmp})
	if err == nil {
		t.Fatal("missing all directives: err = nil, want non-nil")
	}
	trimmed := strings.TrimSpace(out)
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		t.Errorf("--json output is not a JSON array: %q", out)
	}
	// Sanity: count drift entries roughly matches the required set.
	// Drift fields are exported → encoding/json keeps them PascalCase.
	if strings.Count(out, `"Key"`) != len(hardening.RequiredDirectives()) {
		t.Errorf("--json drift count != required count: %q", out)
	}
}

// TestRunHardeningCheck_MissingFileReturnsError covers the
// path-not-found case: the subcommand surfaces a clear filesystem
// error rather than silently treating the absent unit as clean.
func TestRunHardeningCheck_MissingFileReturnsError(t *testing.T) {
	_, err := runSub(t, "hardening-check", []string{filepath.Join(t.TempDir(), "no-such-file")})
	if err == nil {
		t.Error("missing file: err = nil, want non-nil")
	}
}

// canonicalUnit synthesises a unit body that contains every
// directive in the canonical hardening set with the expected
// value. Mirrors the helper used in internal/hardening tests; kept
// duplicated here to avoid a cross-package test-helper export.
func canonicalUnit() string {
	var b strings.Builder
	b.WriteString("[Service]\n")
	for _, r := range hardening.RequiredDirectives() {
		b.WriteString(r.Key)
		b.WriteString("=")
		b.WriteString(r.Want)
		b.WriteString("\n")
	}
	return b.String()
}
