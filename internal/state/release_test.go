package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/solcreek/creekd/internal/supervisor"
)

// helper: a store seeded with one app named id, ready for
// CreateRelease testing.
func storeWithApp(t *testing.T, id string) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.AddApp(supervisor.Config{ID: id, Command: "sleep", Args: []string{"1"}, Port: 9000}); err != nil {
		t.Fatalf("AddApp: %v", err)
	}
	return s
}

// TestCreateRelease_AssignsMonotonicSeq covers the per-app
// monotonic releaseSeq contract: first release seq=1, second
// seq=2, etc. Never reused even after rollback.
func TestCreateRelease_AssignsMonotonicSeq(t *testing.T) {
	s := storeWithApp(t, "a")
	r1, err := s.CreateRelease("a", ReleaseInput{Spec: ReleaseSpec{Image: "img1"}})
	if err != nil {
		t.Fatalf("first CreateRelease: %v", err)
	}
	if r1.Spec.ReleaseSeq != 1 {
		t.Errorf("first release seq = %d, want 1", r1.Spec.ReleaseSeq)
	}
	r2, err := s.CreateRelease("a", ReleaseInput{Spec: ReleaseSpec{Image: "img2"}})
	if err != nil {
		t.Fatalf("second CreateRelease: %v", err)
	}
	if r2.Spec.ReleaseSeq != 2 {
		t.Errorf("second release seq = %d, want 2", r2.Spec.ReleaseSeq)
	}
}

// TestCreateRelease_FillsAppUID covers the contract that the
// new Release's AppUID is sourced from the persisted app metadata,
// not from caller-supplied input. Prevents callers from spoofing
// AppUID values into the ledger.
func TestCreateRelease_FillsAppUID(t *testing.T) {
	s := storeWithApp(t, "a")
	meta, _ := s.Meta("a")
	rel, err := s.CreateRelease("a", ReleaseInput{Spec: ReleaseSpec{
		AppUID: "spoofed-by-caller",
		Image:  "img1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rel.Spec.AppUID != meta.UID {
		t.Errorf("rel.Spec.AppUID = %q, want %q (the persisted UID, not the caller-supplied value)", rel.Spec.AppUID, meta.UID)
	}
}

// TestCreateRelease_NewlyCreatedIsActive proves the new release
// lands with Phase=Active. The earlier-Active flip is covered
// separately.
func TestCreateRelease_NewlyCreatedIsActive(t *testing.T) {
	s := storeWithApp(t, "a")
	rel, err := s.CreateRelease("a", ReleaseInput{Spec: ReleaseSpec{Image: "img"}})
	if err != nil {
		t.Fatal(err)
	}
	if rel.Phase != ReleasePhaseActive {
		t.Errorf("new release phase = %q, want Active", rel.Phase)
	}
}

// TestCreateRelease_FlipsPriorActiveToSuperseded covers the
// at-most-one-Active invariant under the default (deploy)
// semantic: a second CreateRelease sets the prior Active to
// Superseded.
func TestCreateRelease_FlipsPriorActiveToSuperseded(t *testing.T) {
	s := storeWithApp(t, "a")
	r1, _ := s.CreateRelease("a", ReleaseInput{Spec: ReleaseSpec{Image: "img1"}})
	_, err := s.CreateRelease("a", ReleaseInput{Spec: ReleaseSpec{Image: "img2"}})
	if err != nil {
		t.Fatal(err)
	}

	all := s.Releases("a")
	if len(all) != 2 {
		t.Fatalf("Releases len = %d, want 2", len(all))
	}
	if all[0].Spec.ReleaseSeq != r1.Spec.ReleaseSeq {
		t.Errorf("ordering: all[0].seq = %d, want %d", all[0].Spec.ReleaseSeq, r1.Spec.ReleaseSeq)
	}
	if all[0].Phase != ReleasePhaseSuperseded {
		t.Errorf("prior release phase = %q, want Superseded (default flip)", all[0].Phase)
	}
	if all[1].Phase != ReleasePhaseActive {
		t.Errorf("new release phase = %q, want Active", all[1].Phase)
	}
}

// TestCreateRelease_RollbackVariantFlipsPriorToRolledBack proves
// PriorActivePhase = RolledBack overrides the default. This is
// the path #8c's rollback handler will use.
func TestCreateRelease_RollbackVariantFlipsPriorToRolledBack(t *testing.T) {
	s := storeWithApp(t, "a")
	_, _ = s.CreateRelease("a", ReleaseInput{Spec: ReleaseSpec{Image: "img1"}})
	_, err := s.CreateRelease("a", ReleaseInput{
		Spec:             ReleaseSpec{Image: "img1", RolledBackFrom: 1, OriginalArtifactRelease: 1},
		PriorActivePhase: ReleasePhaseRolledBack,
	})
	if err != nil {
		t.Fatal(err)
	}
	all := s.Releases("a")
	if all[0].Phase != ReleasePhaseRolledBack {
		t.Errorf("prior phase after rollback flip = %q, want RolledBack", all[0].Phase)
	}
	if all[1].Spec.RolledBackFrom != 1 {
		t.Errorf("new release rolledBackFrom = %d, want 1", all[1].Spec.RolledBackFrom)
	}
}

// TestCreateRelease_RejectsUnknownApp covers the sentinel-error
// contract: callers can errors.Is(err, ErrAppNotFound).
func TestCreateRelease_RejectsUnknownApp(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateRelease("ghost", ReleaseInput{})
	if !errors.Is(err, ErrAppNotFound) {
		t.Errorf("CreateRelease on unknown app: err = %v, want errors.Is(ErrAppNotFound)", err)
	}
}

// TestCreateRelease_RejectsInvalidPriorPhase locks down the API:
// PriorActivePhase MUST be Superseded, RolledBack, or "" (default).
// "Active" is meaningless (you can't flip prior Active to Active).
// "RolledBack" with new release also valid for rollback.
func TestCreateRelease_RejectsInvalidPriorPhase(t *testing.T) {
	s := storeWithApp(t, "a")
	_, err := s.CreateRelease("a", ReleaseInput{
		Spec:             ReleaseSpec{Image: "img"},
		PriorActivePhase: ReleasePhaseActive, // invalid: can't flip to Active
	})
	if err == nil {
		t.Error("CreateRelease with PriorActivePhase=Active should error")
	}
	_, err = s.CreateRelease("a", ReleaseInput{
		Spec:             ReleaseSpec{Image: "img"},
		PriorActivePhase: "Nonsense",
	})
	if err == nil {
		t.Error("CreateRelease with invalid PriorActivePhase should error")
	}
}

// TestCreateRelease_PersistsAcrossRestart covers durability: a
// fresh Store reads back the full release ledger.
func TestCreateRelease_PersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s1, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.AddApp(supervisor.Config{ID: "a", Command: "sleep", Args: []string{"1"}, Port: 9000}); err != nil {
		t.Fatal(err)
	}
	_, _ = s1.CreateRelease("a", ReleaseInput{Spec: ReleaseSpec{Image: "img1", GitSha: "abc"}})
	_, _ = s1.CreateRelease("a", ReleaseInput{Spec: ReleaseSpec{Image: "img2", GitSha: "def"}})

	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := s2.Releases("a")
	if len(got) != 2 {
		t.Fatalf("reload: %d releases, want 2", len(got))
	}
	if got[1].Spec.GitSha != "def" {
		t.Errorf("reload release[1].GitSha = %q, want %q", got[1].Spec.GitSha, "def")
	}
	active, ok := s2.ActiveRelease("a")
	if !ok || active.Spec.ReleaseSeq != 2 {
		t.Errorf("post-reload ActiveRelease seq = %d (ok=%v), want 2 true", active.Spec.ReleaseSeq, ok)
	}
}

// TestCreateRelease_BumpsAppResourceVersion covers the wire
// contract: creating a release IS a status mutation (Active flip
// on prior, new ledger entry), so the app's ResourceVersion bumps
// — even though spec didn't move. Generation does NOT bump
// (spec-only counter).
func TestCreateRelease_BumpsAppResourceVersion(t *testing.T) {
	s := storeWithApp(t, "a")
	rvBefore, _ := s.Meta("a")
	_, err := s.CreateRelease("a", ReleaseInput{Spec: ReleaseSpec{Image: "img"}})
	if err != nil {
		t.Fatal(err)
	}
	rvAfter, _ := s.Meta("a")
	if rvAfter.ResourceVersion != rvBefore.ResourceVersion+1 {
		t.Errorf("rv after CreateRelease = %d, want %d", rvAfter.ResourceVersion, rvBefore.ResourceVersion+1)
	}
	if rvAfter.Generation != rvBefore.Generation {
		t.Errorf("generation moved: %d → %d (spec untouched, must stay)", rvBefore.Generation, rvAfter.Generation)
	}
}

// TestRemoveApp_DropsReleaseLedger proves StopApp drops the entry's
// ledger too — a re-creation with the same name doesn't inherit
// historical releases from a prior incarnation. (UID is also
// regenerated per existing AppMetadata semantics.)
func TestRemoveApp_DropsReleaseLedger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddApp(supervisor.Config{ID: "a", Command: "sleep", Args: []string{"1"}, Port: 9000}); err != nil {
		t.Fatal(err)
	}
	_, _ = s.CreateRelease("a", ReleaseInput{Spec: ReleaseSpec{Image: "img"}})
	if err := s.RemoveApp("a"); err != nil {
		t.Fatal(err)
	}
	// Re-create with same name.
	if err := s.AddApp(supervisor.Config{ID: "a", Command: "sleep", Args: []string{"1"}, Port: 9000}); err != nil {
		t.Fatal(err)
	}
	if got := s.Releases("a"); len(got) != 0 {
		t.Errorf("re-created app inherited %d releases from prior incarnation, want 0", len(got))
	}
	// First release on the recreated app must still be seq=1 (counter
	// reset on app deletion).
	rel, err := s.CreateRelease("a", ReleaseInput{Spec: ReleaseSpec{Image: "fresh"}})
	if err != nil {
		t.Fatal(err)
	}
	if rel.Spec.ReleaseSeq != 1 {
		t.Errorf("first release on re-created app seq = %d, want 1 (counter must reset)", rel.Spec.ReleaseSeq)
	}
}

// TestActiveRelease_ReturnsFalseWhenNoneExists covers the empty-
// ledger case: an app with no deploys has no Active release.
func TestActiveRelease_ReturnsFalseWhenNoneExists(t *testing.T) {
	s := storeWithApp(t, "a")
	if _, ok := s.ActiveRelease("a"); ok {
		t.Error("ActiveRelease on app with no releases should return ok=false")
	}
}

// TestFindRelease_ReturnsByExactSeq covers the rollback handler's
// lookup primitive: --to=N must resolve to the release with that
// seq (or fail).
func TestFindRelease_ReturnsByExactSeq(t *testing.T) {
	s := storeWithApp(t, "a")
	_, _ = s.CreateRelease("a", ReleaseInput{Spec: ReleaseSpec{Image: "v1"}})
	_, _ = s.CreateRelease("a", ReleaseInput{Spec: ReleaseSpec{Image: "v2"}})

	r, ok := s.FindRelease("a", 1)
	if !ok || r.Spec.Image != "v1" {
		t.Errorf("FindRelease(a, 1) = %+v ok=%v, want v1", r, ok)
	}
	if _, ok := s.FindRelease("a", 99); ok {
		t.Error("FindRelease(a, 99) returned ok=true for nonexistent seq")
	}
}

// TestV2ToV3Migration_TransparentlyAddsEmptyReleases covers the
// forward-compat contract: a v2 state.json (no releases array) on
// disk MUST load cleanly, present as empty release ledger to
// callers, AND be rewritten as v3 on disk so subsequent boots take
// the steady-state path.
func TestV2ToV3Migration_TransparentlyAddsEmptyReleases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	// Hand-craft a v2 state.json (no releases field).
	v2 := []byte(`{
  "version": 2,
  "apps": [
    {
      "config": {"ID":"legacy-app","Command":"sleep","Args":["1"],"Port":9000},
      "metadata": {
        "uid": "018f5c8d-0000-7000-8000-000000000001",
        "generation": 1,
        "resource_version": 1,
        "creation_timestamp": "2026-05-01T00:00:00Z"
      }
    }
  ]
}`)
	if err := os.WriteFile(path, v2, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore on v2 file: %v", err)
	}
	if got := s.Releases("legacy-app"); len(got) != 0 {
		t.Errorf("legacy app has %d releases, want 0 (v2 had no ledger)", len(got))
	}

	// File on disk should now be v3.
	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(rewritten, &head); err != nil {
		t.Fatal(err)
	}
	if head.Version != FormatVersion {
		t.Errorf("post-migration version on disk = %d, want %d", head.Version, FormatVersion)
	}

	// CreateRelease on the migrated app must work and seq starts at 1.
	rel, err := s.CreateRelease("legacy-app", ReleaseInput{Spec: ReleaseSpec{Image: "img1"}})
	if err != nil {
		t.Fatalf("CreateRelease on migrated app: %v", err)
	}
	if rel.Spec.ReleaseSeq != 1 {
		t.Errorf("first release seq after migration = %d, want 1", rel.Spec.ReleaseSeq)
	}
}

// TestReleases_ReturnsDeepCopy proves the Releases getter is safe
// against caller mutation: appending to the returned slice doesn't
// leak back into the store.
func TestReleases_ReturnsDeepCopy(t *testing.T) {
	s := storeWithApp(t, "a")
	_, _ = s.CreateRelease("a", ReleaseInput{Spec: ReleaseSpec{Image: "v1"}})

	leak := s.Releases("a")
	leak = append(leak, Release{UID: "evil"})
	_ = leak

	got := s.Releases("a")
	if len(got) != 1 {
		t.Errorf("internal state corrupted by caller mutation: %d releases, want 1", len(got))
	}
}
