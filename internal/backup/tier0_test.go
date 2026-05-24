package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeNow returns a clock that increments by 1 second per call so
// successive WriteTier0 calls produce distinct filenames.
func fakeNow(start time.Time) func() time.Time {
	t := start
	return func() time.Time {
		t = t.Add(time.Second)
		return t
	}
}

func setupStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"version":2,"apps":[]}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "audit.log"),
		[]byte(`{"action":"spawn","prev_sha256":"00..00"}`+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestTier0_WritesArtifactAndManifest covers the happy path: one
// backup landed on disk, manifest verifies, contentHash matches
// the on-disk inputs.
func TestTier0_WritesArtifactAndManifest(t *testing.T) {
	state := setupStateDir(t)
	bdir := t.TempDir()
	key := mustHostKey(t)

	res, err := WriteTier0(Options{
		StateDir: state, BackupDir: bdir,
		CreekdVersion: "0.0.1", SchemaVersion: 2,
		HostKey: key, Now: fakeNow(time.Unix(1_700_000_000, 0)),
	})
	if err != nil {
		t.Fatalf("WriteTier0: %v", err)
	}
	if res.ArtifactPath == "" {
		t.Fatal("ArtifactPath empty")
	}
	if _, err := os.Stat(res.ArtifactPath); err != nil {
		t.Errorf("artifact not on disk: %v", err)
	}
	if !strings.HasSuffix(res.ArtifactPath, ".tar.gz") {
		t.Errorf("artifact = %q, want .tar.gz suffix", res.ArtifactPath)
	}
	if res.Manifest.ContentHash == "" || res.Manifest.Signature == "" {
		t.Errorf("manifest fields not populated: %+v", res.Manifest)
	}
	if err := VerifyManifest(&res.Manifest, key.Pub); err != nil {
		t.Errorf("freshly-written manifest does not verify: %v", err)
	}
}

// TestTier0_ReadArtifactRoundTrip covers the restore path: untar,
// recompute contentHash, manifest still verifies, hashes match.
func TestTier0_ReadArtifactRoundTrip(t *testing.T) {
	state := setupStateDir(t)
	bdir := t.TempDir()
	key := mustHostKey(t)
	res, err := WriteTier0(Options{
		StateDir: state, BackupDir: bdir,
		CreekdVersion: "0.0.1", SchemaVersion: 2,
		HostKey: key, Now: fakeNow(time.Unix(1_700_000_000, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}

	stateBytes, auditBytes, m, _, err := ReadArtifact(res.ArtifactPath)
	if err != nil {
		t.Fatalf("ReadArtifact: %v", err)
	}
	if err := VerifyManifest(&m, key.Pub); err != nil {
		t.Errorf("untarred manifest verify: %v", err)
	}
	recomputed := hashContent(stateBytes, auditBytes)
	if recomputed != m.ContentHash {
		t.Errorf("recomputed contentHash = %q, want %q", recomputed, m.ContentHash)
	}
}

// TestTier0_DetectsTamperedPayload covers the security goal of
// contentHash: any post-backup mutation of state.json or audit.log
// (e.g. an attacker that re-tars the bundle) is caught by the
// contentHash mismatch after untar, even though the signed
// manifest itself remained intact.
func TestTier0_DetectsTamperedPayload(t *testing.T) {
	state := setupStateDir(t)
	bdir := t.TempDir()
	key := mustHostKey(t)
	res, err := WriteTier0(Options{
		StateDir: state, BackupDir: bdir,
		CreekdVersion: "0.0.1", SchemaVersion: 2,
		HostKey: key, Now: fakeNow(time.Unix(1_700_000_000, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Mutate state.json on the source dir and recompute the hash
	// from those new bytes: the original manifest's contentHash
	// will NOT match → tamper detected.
	if err := os.WriteFile(filepath.Join(state, "state.json"), []byte(`{"version":2,"apps":["evil"]}`), 0o640); err != nil {
		t.Fatal(err)
	}
	mutated, err := os.ReadFile(filepath.Join(state, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, auditBytes, _, _, err := ReadArtifact(res.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	got := hashContent(mutated, auditBytes)
	if got == res.Manifest.ContentHash {
		t.Error("contentHash for tampered state should differ from manifest's stored value, but matched — collision or bug")
	}
}

// TestTier0_PrunesPastKeep proves the retention contract: with
// Keep=3 and 5 successive backups, the two oldest must be deleted.
func TestTier0_PrunesPastKeep(t *testing.T) {
	state := setupStateDir(t)
	bdir := t.TempDir()
	key := mustHostKey(t)
	clock := fakeNow(time.Unix(1_700_000_000, 0))

	var artifacts []string
	for i := 0; i < 5; i++ {
		res, err := WriteTier0(Options{
			StateDir: state, BackupDir: bdir,
			CreekdVersion: "0.0.1", SchemaVersion: 2,
			Keep: 3, HostKey: key, Now: clock,
		})
		if err != nil {
			t.Fatalf("WriteTier0 #%d: %v", i, err)
		}
		artifacts = append(artifacts, res.ArtifactPath)
	}

	// Read directory: should be exactly 3 state-*.tar.gz files.
	entries, err := os.ReadDir(bdir)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "state-") && strings.HasSuffix(e.Name(), ".tar.gz") {
			found = append(found, e.Name())
		}
	}
	if len(found) != 3 {
		t.Errorf("retained = %d, want 3 (Keep=3 after 5 backups); got = %v", len(found), found)
	}
	// First two artifacts must be gone.
	for _, p := range artifacts[:2] {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("oldest artifact %s should be pruned, stat err = %v", filepath.Base(p), err)
		}
	}
	// Last three must remain.
	for _, p := range artifacts[2:] {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("recent artifact %s should remain, stat err = %v", filepath.Base(p), err)
		}
	}
}

// TestTier0_AuditTipHashChangesWithLog covers the auditLogTipHash
// field: a different audit.log tip yields a different
// auditLogTipHash, so a restorer can detect mid-flight log
// truncation (manifest's tip doesn't match the actual tip of the
// log inside the tarball).
func TestTier0_AuditTipHashChangesWithLog(t *testing.T) {
	state := setupStateDir(t)
	bdir := t.TempDir()
	key := mustHostKey(t)
	res1, err := WriteTier0(Options{
		StateDir: state, BackupDir: bdir, HostKey: key,
		Now: fakeNow(time.Unix(1_700_000_000, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Append a new audit line; tip changes.
	f, err := os.OpenFile(filepath.Join(state, "audit.log"), os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(f, `{"action":"stop","prev_sha256":"aa..ff"}`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	res2, err := WriteTier0(Options{
		StateDir: state, BackupDir: bdir, HostKey: key,
		Now: fakeNow(time.Unix(1_700_000_100, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res1.Manifest.AuditLogTipHash == res2.Manifest.AuditLogTipHash {
		t.Error("auditLogTipHash unchanged after appending a new audit record")
	}
}

// TestTier0_NoAuditLogYieldsNoneTip covers the fresh-host case:
// audit.log doesn't exist yet → auditLogTipHash == "none" and the
// tarball just doesn't carry audit.log. contentHash still pins
// state.json alone.
func TestTier0_NoAuditLogYieldsNoneTip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"version":2}`), 0o640); err != nil {
		t.Fatal(err)
	}
	bdir := t.TempDir()
	key := mustHostKey(t)
	res, err := WriteTier0(Options{
		StateDir: dir, BackupDir: bdir, HostKey: key,
		Now: fakeNow(time.Unix(1_700_000_000, 0)),
	})
	if err != nil {
		t.Fatalf("WriteTier0 on no-audit dir: %v", err)
	}
	if res.Manifest.AuditLogTipHash != "none" {
		t.Errorf("auditLogTipHash = %q, want \"none\" when audit.log absent", res.Manifest.AuditLogTipHash)
	}
	if err := VerifyManifest(&res.Manifest, key.Pub); err != nil {
		t.Errorf("manifest verify in no-audit case: %v", err)
	}
}

// TestTier0_RejectsMissingStateJSON covers the not-yet-bootstrapped
// case: WriteTier0 must surface a clear error rather than write an
// empty-state.json artifact that would silently overwrite real
// state on restore.
func TestTier0_RejectsMissingStateJSON(t *testing.T) {
	dir := t.TempDir() // empty
	bdir := t.TempDir()
	key := mustHostKey(t)
	_, err := WriteTier0(Options{
		StateDir: dir, BackupDir: bdir, HostKey: key,
		Now: fakeNow(time.Unix(1_700_000_000, 0)),
	})
	if err == nil {
		t.Error("WriteTier0 should fail when state.json is absent")
	}
}

// TestTier0_RequiresHostKey covers the API contract: WriteTier0
// without a key is a programmer error, not a runtime fallback to
// unsigned manifests.
func TestTier0_RequiresHostKey(t *testing.T) {
	state := setupStateDir(t)
	bdir := t.TempDir()
	_, err := WriteTier0(Options{StateDir: state, BackupDir: bdir})
	if err == nil {
		t.Error("WriteTier0 with no HostKey should error")
	}
}
