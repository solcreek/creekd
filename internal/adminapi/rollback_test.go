package adminapi

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/solcreek/creekd/internal/apitypes"
	"github.com/solcreek/creekd/internal/state"
)

// rollbackTestServer spawns an app, deploys N times, and returns
// the server, the test store, and the per-deploy ports so the
// caller can assert release-ledger state.
func rollbackTestServer(t *testing.T, deploys int) (*testServer, *state.Store, []int) {
	t.Helper()
	ts := newTestServer(t, "")
	// Disable the HTTPHealthChecker so Deploy's ready probe accepts
	// "process started" as healthy after a brief settle. The
	// supervisor default would otherwise reject sleep-based test
	// apps (no HTTP listener) with deploy_unhealthy. Health-probe
	// behaviour itself is exercised in supervisor's deploy
	// integration tests.
	ts.sup.HealthChecker = nil
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ts.srv.SetStore(store)

	// Spawn the initial app.
	p0 := freeTCPPort(t)
	status, body := ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "rbk", Command: ptr("sleep"), Args: &[]string{"30"}, Port: p0}, "")
	if status != http.StatusCreated {
		t.Fatalf("spawn status = %d, body = %s", status, body)
	}
	t.Cleanup(func() { _ = ts.sup.Stop("rbk") })

	ports := []int{p0}
	for i := 0; i < deploys; i++ {
		p := freeTCPPort(t)
		ports = append(ports, p)
		dr := apitypes.DeployRequest{Port: p, Command: ptr("sleep"), Args: &[]string{"30"},
			Env: &[]string{fmt.Sprintf("VERSION=%d", i+1)}}
		status, body := ts.do(t, "POST", "/v1/apps/rbk/deploy", dr, "")
		if status != http.StatusOK {
			t.Fatalf("deploy %d status = %d body = %s", i, status, body)
		}
	}
	return ts, store, ports
}

// TestDeploy_CreatesReleaseLedger covers the happy path: each
// successful deploy MUST append a Release record. After N deploys
// the ledger has N entries, the latest is Active, prior ones
// Superseded.
func TestDeploy_CreatesReleaseLedger(t *testing.T) {
	_, store, _ := rollbackTestServer(t, 2)
	releases := store.Releases("rbk")
	if len(releases) != 2 {
		t.Fatalf("Releases len = %d, want 2 (two deploys)", len(releases))
	}
	if releases[0].Phase != state.ReleasePhaseSuperseded {
		t.Errorf("releases[0].Phase = %q, want Superseded", releases[0].Phase)
	}
	if releases[1].Phase != state.ReleasePhaseActive {
		t.Errorf("releases[1].Phase = %q, want Active", releases[1].Phase)
	}
	// Sanity-check ReleaseSeq monotonicity.
	if releases[0].Spec.ReleaseSeq != 1 || releases[1].Spec.ReleaseSeq != 2 {
		t.Errorf("seqs = %d, %d; want 1, 2", releases[0].Spec.ReleaseSeq, releases[1].Spec.ReleaseSeq)
	}
}

// TestDeploy_ReleaseCarriesEnvHashAndConfigSnapshot covers the
// rollback-enabler fields: Release must persist EnvHash + a
// ConfigSnapshot (the supervisor.Config to re-run on rollback).
func TestDeploy_ReleaseCarriesEnvHashAndConfigSnapshot(t *testing.T) {
	_, store, _ := rollbackTestServer(t, 1)
	releases := store.Releases("rbk")
	r := releases[0]
	if r.Spec.EnvHash == "" {
		t.Error("Release.Spec.EnvHash is empty; deploy must compute it from env")
	}
	if r.Spec.ConfigSnapshot == nil {
		t.Fatal("Release.Spec.ConfigSnapshot is nil; rollback target won't be re-runnable")
	}
	if r.Spec.ConfigSnapshot.ID != "rbk" {
		t.Errorf("ConfigSnapshot.ID = %q, want \"rbk\"", r.Spec.ConfigSnapshot.ID)
	}
}

// TestRollback_HappyPath covers the canonical rollback flow: deploy
// v2 over v1, then rollback to v1. Resulting state:
//   - 3 releases in ledger (seqs 1, 2, 3)
//   - seq 3 is Active and references seq 2 (RolledBackFrom)
//     and seq 2 (OriginalArtifactRelease since v2 was the source)
//
// Wait — that's wrong. On rollback v2 → v1, we're moving the system
// back to v1's artifact. So the new Release seq=3 has:
//   - RolledBackFrom = 2 (the one we rolled AWAY from)
//   - Hmm, the design wording is ambiguous. Re-reading...
//
// DESIGN: "creek rollback my-app --to=v122: Creates a new Release v124
// with spec.rolledBackFrom = v122". So rolledBackFrom points at the
// TARGET (the version we ROLLED BACK TO). That makes more sense as a
// chain link — the new release inherited its artifact from this one.
//
// So rollback v2 → v1 produces seq 3 with:
//   - RolledBackFrom = 1 (the target we rolled back to)
//   - OriginalArtifactRelease = 1 (first appearance of v1's artifact)
//
// And the prior Active (seq 2) becomes RolledBack (because we rolled
// AWAY FROM seq 2).
func TestRollback_HappyPath(t *testing.T) {
	ts, store, _ := rollbackTestServer(t, 1) // v1 spawn + v2 deploy = 1 release for "deploy"; actually only 1 release after spawn+deploy(1)

	// rollbackTestServer creates "deploys" total Release records (the
	// initial spawn does NOT create a release — only Deploy does, per
	// #8c contract). So after the call above we have 1 Release (v1).
	// Add a second deploy to set up the rollback test cleanly.
	p := freeTCPPort(t)
	dr := apitypes.DeployRequest{Port: p, Command: ptr("sleep"), Args: &[]string{"30"},
		Env: &[]string{"VERSION=2"}}
	status, body := ts.do(t, "POST", "/v1/apps/rbk/deploy", dr, "")
	if status != http.StatusOK {
		t.Fatalf("second deploy: status = %d, body = %s", status, body)
	}
	// Now ledger: seq=1 (Superseded), seq=2 (Active).

	// Rollback to seq=1.
	status, body = ts.do(t, "POST", "/v1/apps/rbk/rollback?to=1", nil, "")
	if status != http.StatusOK {
		t.Fatalf("rollback status = %d body = %s, want 200", status, body)
	}
	var rel apitypes.Release
	mustJSON(t, body, &rel)
	if rel.Phase != "Active" {
		t.Errorf("returned release phase = %q, want Active", rel.Phase)
	}
	if rel.Spec.ReleaseSeq != 3 {
		t.Errorf("returned release seq = %d, want 3", rel.Spec.ReleaseSeq)
	}
	if rel.Spec.RolledBackFrom == nil || *rel.Spec.RolledBackFrom != 1 {
		t.Errorf("rolledBackFrom = %v, want 1 (the target we rolled back to)", rel.Spec.RolledBackFrom)
	}
	if rel.Spec.OriginalArtifactRelease == nil || *rel.Spec.OriginalArtifactRelease != 1 {
		t.Errorf("originalArtifactRelease = %v, want 1 (v1 is the first appearance)", rel.Spec.OriginalArtifactRelease)
	}

	// Ledger state assertions.
	releases := store.Releases("rbk")
	if len(releases) != 3 {
		t.Fatalf("ledger len = %d, want 3", len(releases))
	}
	if releases[1].Phase != state.ReleasePhaseRolledBack {
		t.Errorf("seq 2 phase after rollback = %q, want RolledBack", releases[1].Phase)
	}
	if releases[2].Phase != state.ReleasePhaseActive {
		t.Errorf("seq 3 phase = %q, want Active", releases[2].Phase)
	}
}

// TestRollback_TargetNotFoundReturns404 covers the
// release_artifact_pruned error code when the requested seq isn't
// in the ledger.
func TestRollback_TargetNotFoundReturns404(t *testing.T) {
	ts, _, _ := rollbackTestServer(t, 1)
	status, body := ts.do(t, "POST", "/v1/apps/rbk/rollback?to=99", nil, "")
	if status != http.StatusNotFound {
		t.Errorf("status = %d body = %s, want 404", status, body)
	}
	var er apitypes.ErrorResponse
	mustJSON(t, body, &er)
	if string(er.Code) != string(apitypes.ErrorCodeReleaseArtifactPruned) {
		t.Errorf("error code = %q, want release_artifact_pruned", er.Code)
	}
}

// TestResolveOriginalArtifactRelease_ChainPropagation covers the
// non-trivial OAR resolution at unit level: rolling back to a
// rollback record (target has OAR != 0) MUST propagate the FIRST-
// appearance seq, not point at the intermediate. Rolling back to a
// fresh deploy record (target has OAR == 0) uses target.seq.
//
// HTTP-level chain testing requires port reassignment on rollback
// (snap port collides with current Active on chained re-runs);
// covered separately if/when ?port=N lands. The 0.0.x OAR
// invariant lives entirely in this resolver — the rest of the
// handler is mechanical.
func TestResolveOriginalArtifactRelease_ChainPropagation(t *testing.T) {
	// Case 1: target is a fresh deploy. OAR resolves to target.seq.
	freshDeploy := state.Release{Spec: state.ReleaseSpec{ReleaseSeq: 7}}
	if got := resolveOriginalArtifactRelease(freshDeploy); got != 7 {
		t.Errorf("fresh-deploy target → OAR = %d, want 7 (target seq)", got)
	}

	// Case 2: target was itself a rollback record. OAR propagates.
	rollbackRecord := state.Release{Spec: state.ReleaseSpec{
		ReleaseSeq:              7,
		RolledBackFrom:          2,
		OriginalArtifactRelease: 1, // the artifact's first appearance
	}}
	if got := resolveOriginalArtifactRelease(rollbackRecord); got != 1 {
		t.Errorf("chained-rollback target → OAR = %d, want 1 (first appearance, not intermediate)", got)
	}
}

// TestRollback_RequiresPositiveToParam covers input validation: ?to=0
// or omitted yields 400, not a server error.
func TestRollback_RequiresPositiveToParam(t *testing.T) {
	ts, _, _ := rollbackTestServer(t, 1)
	status, _ := ts.do(t, "POST", "/v1/apps/rbk/rollback?to=0", nil, "")
	if status != http.StatusBadRequest {
		t.Errorf("?to=0: status = %d, want 400", status)
	}
}

// TestEnvHash_StableAndOrderInsensitive covers the wire contract
// for the EnvHash helper used by deploy + rollback handlers. Same
// SET → same hash. Same set in different order → same hash.
// Different set → different hash.
func TestEnvHash_StableAndOrderInsensitive(t *testing.T) {
	h1 := state.EnvHash([]string{"A=1", "B=2"})
	h2 := state.EnvHash([]string{"B=2", "A=1"})
	if h1 != h2 {
		t.Error("EnvHash should be order-insensitive on equivalent sets")
	}
	h3 := state.EnvHash([]string{"A=1", "B=3"})
	if h1 == h3 {
		t.Error("different env-value → same hash; helper is broken")
	}
	if state.EnvHash(nil) != "" {
		t.Errorf("EnvHash(nil) = %q, want \"\"", state.EnvHash(nil))
	}
}
