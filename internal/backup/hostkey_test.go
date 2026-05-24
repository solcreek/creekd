package backup

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestHostKey_LoadOrCreate_GeneratesNewWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hostkey")

	hk, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateHostKey: %v", err)
	}
	if len(hk.Priv) != ed25519.PrivateKeySize {
		t.Errorf("priv size = %d, want %d", len(hk.Priv), ed25519.PrivateKeySize)
	}
	if len(hk.Pub) != ed25519.PublicKeySize {
		t.Errorf("pub size = %d, want %d", len(hk.Pub), ed25519.PublicKeySize)
	}
	if hk.Fingerprint[:7] != "sha256:" {
		t.Errorf("fingerprint = %q, want sha256: prefix", hk.Fingerprint)
	}

	// File must have landed on disk with mode 0600.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat hostkey: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("hostkey perm = %o, want 0600", perm)
	}
}

// TestHostKey_LoadOrCreate_LoadsExisting proves the same key bytes
// round-trip on a second call — the second call must NOT generate
// a fresh keypair (that would invalidate every prior signed
// manifest).
func TestHostKey_LoadOrCreate_LoadsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hostkey")
	h1, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	h2, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if string(h1.Priv) != string(h2.Priv) {
		t.Error("second load produced different private key (must be stable)")
	}
	if h1.Fingerprint != h2.Fingerprint {
		t.Errorf("fingerprint drift: %q vs %q", h1.Fingerprint, h2.Fingerprint)
	}
}

func TestHostKey_LoadOrCreate_RejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hostkey")
	if err := os.WriteFile(path, []byte("not a real ed25519 key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateHostKey(path); err == nil {
		t.Error("LoadOrCreateHostKey on garbage file should error")
	}
}

// helper used by manifest_test.go: a deterministic keypair for
// signing comparisons.
func mustHostKey(t *testing.T) *HostKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return &HostKey{Priv: priv, Pub: pub, Fingerprint: fingerprintPub(pub)}
}
