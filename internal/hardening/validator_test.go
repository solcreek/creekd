package hardening

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// minimalHardenedUnit returns the canonical unit body with every
// required directive present + correct. Tests start from this and
// mutate to test each failure mode independently.
func minimalHardenedUnit() string {
	var b strings.Builder
	b.WriteString("[Service]\n")
	for _, r := range RequiredDirectives() {
		b.WriteString(r.Key)
		b.WriteString("=")
		b.WriteString(r.Want)
		b.WriteString("\n")
	}
	return b.String()
}

// TestValidate_AcceptsCanonicalUnit covers the happy path: the
// canonical hardening set yields zero drift.
func TestValidate_AcceptsCanonicalUnit(t *testing.T) {
	got := Validate(minimalHardenedUnit())
	if len(got) != 0 {
		t.Errorf("canonical unit produced %d drift entries: %v", len(got), got)
	}
}

// TestValidate_ReportsMissingDirective covers the deletion case:
// dropping any directive surfaces a "missing" Drift for that key.
func TestValidate_ReportsMissingDirective(t *testing.T) {
	body := minimalHardenedUnit()
	// Remove the NoNewPrivileges line specifically.
	mutated := strings.Replace(body, "NoNewPrivileges=true\n", "", 1)
	if mutated == body {
		t.Fatal("test setup: failed to strip NoNewPrivileges")
	}
	drift := Validate(mutated)
	if !containsDriftFor(drift, "NoNewPrivileges") {
		t.Errorf("missing NoNewPrivileges not reported: %v", drift)
	}
	if d := findDrift(drift, "NoNewPrivileges"); d.Reason != "missing" || d.Got != "" {
		t.Errorf("NoNewPrivileges drift = %+v, want Reason=missing Got=empty", d)
	}
}

// TestValidate_ReportsWeakenedDirective covers the "directive
// present but value wrong" case. ProtectSystem=basic is a real
// systemd value that's weaker than strict — exactly what we want
// to flag.
func TestValidate_ReportsWeakenedDirective(t *testing.T) {
	body := strings.Replace(minimalHardenedUnit(),
		"ProtectSystem=strict",
		"ProtectSystem=basic", 1)
	drift := Validate(body)
	d := findDrift(drift, "ProtectSystem")
	if d.Reason != "weakened" {
		t.Errorf("ProtectSystem weakened: reason = %q, want weakened", d.Reason)
	}
	if d.Got != "basic" || d.Want != "strict" {
		t.Errorf("ProtectSystem drift = %+v, want Got=basic Want=strict", d)
	}
}

// TestValidate_IgnoresDirectivesOutsideServiceSection covers the
// section-scope rule: a NoNewPrivileges line under [Unit] is not
// honoured by systemd, so the validator must NOT count it as
// present. Bug shield against a misplaced override.
func TestValidate_IgnoresDirectivesOutsideServiceSection(t *testing.T) {
	body := "[Unit]\nNoNewPrivileges=true\n[Service]\n" +
		strings.Replace(minimalHardenedUnit(), "[Service]\n", "", 1)
	// Drop NoNewPrivileges from the Service section so the only
	// place it appears is under [Unit].
	body = strings.Replace(body, "NoNewPrivileges=true\n", "", -1)
	body = "[Unit]\nNoNewPrivileges=true\n[Service]\n" + minimalHardenedUnit()[len("[Service]\n"):]
	body = strings.Replace(body, "[Service]\nNoNewPrivileges=true\n", "[Service]\n", 1)

	drift := Validate(body)
	if !containsDriftFor(drift, "NoNewPrivileges") {
		t.Error("NoNewPrivileges under [Unit] should not satisfy the requirement; want drift, got none")
	}
}

// TestValidate_SystemCallFilterOrderInsensitive proves the
// non-trivial match rule: the same deny tokens in a different
// order MUST NOT flag drift — systemd treats them as equivalent.
func TestValidate_SystemCallFilterOrderInsensitive(t *testing.T) {
	body := strings.Replace(minimalHardenedUnit(),
		"SystemCallFilter=@system-service ~@privileged ~@resources",
		"SystemCallFilter=~@resources @system-service ~@privileged", 1)
	drift := Validate(body)
	if d := findDrift(drift, "SystemCallFilter"); d.Reason != "" {
		t.Errorf("SystemCallFilter reordered triggered drift: %+v", d)
	}
}

// TestValidate_IgnoresCommentsAndBlankLines is a parse-robustness
// check: a unit file with comments + blank lines must validate
// just as cleanly.
func TestValidate_IgnoresCommentsAndBlankLines(t *testing.T) {
	body := "# top-level comment\n\n[Service]\n# inline comment\n; alt-comment\n\n" +
		strings.TrimPrefix(minimalHardenedUnit(), "[Service]\n")
	if drift := Validate(body); len(drift) != 0 {
		t.Errorf("commented unit produced drift: %v", drift)
	}
}

// TestValidate_EmptyUnitReportsAllMissing covers the bottom of the
// failure spectrum: an empty unit body must flag every required
// directive as missing.
func TestValidate_EmptyUnitReportsAllMissing(t *testing.T) {
	drift := Validate("")
	if len(drift) != len(RequiredDirectives()) {
		t.Errorf("empty unit drift count = %d, want %d (all required)",
			len(drift), len(RequiredDirectives()))
	}
}

// TestShippedUnitValidatesClean is the regression fence: the
// init/creekd.service file we publish to the repo MUST itself
// satisfy every directive. A future PR that weakens the shipped
// unit will fail this test before reaching review.
func TestShippedUnitValidatesClean(t *testing.T) {
	// Resolve repo-relative path. This test file lives at
	// internal/hardening/, the unit at init/creekd.service.
	_, here, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(here), "..", "..")
	data, err := os.ReadFile(filepath.Join(repo, "init", "creekd.service"))
	if err != nil {
		t.Fatalf("read shipped unit: %v", err)
	}
	if drift := Validate(string(data)); len(drift) != 0 {
		t.Errorf("shipped init/creekd.service has drift:\n  %s",
			strings.Join(driftStrings(drift), "\n  "))
	}
}

// --- helpers ----------------------------------------------------

func containsDriftFor(drift []Drift, key string) bool {
	for _, d := range drift {
		if d.Key == key {
			return true
		}
	}
	return false
}

func findDrift(drift []Drift, key string) Drift {
	for _, d := range drift {
		if d.Key == key {
			return d
		}
	}
	return Drift{}
}

func driftStrings(drift []Drift) []string {
	out := make([]string, len(drift))
	for i, d := range drift {
		out[i] = d.String()
	}
	return out
}
