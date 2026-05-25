package backup

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

func TestManifest_SignAndVerify_RoundTrips(t *testing.T) {
	key := mustHostKey(t)
	m := Manifest{
		CreekdVersion:      "0.0.1",
		SchemaVersion:      2,
		BackupTimestamp:    "2026-05-24T12:00:00Z",
		AuditLogTipHash:    "sha256:deadbeef",
		FleetCAFingerprint: key.Fingerprint,
		ContentHash:        "sha256:cafef00d",
	}
	if err := SignManifest(&m, key); err != nil {
		t.Fatalf("SignManifest: %v", err)
	}
	if !strings.HasPrefix(m.Signature, "ed25519:") {
		t.Errorf("signature = %q, want ed25519: prefix", m.Signature)
	}
	if !strings.HasPrefix(m.SignedBy, "host-key-fingerprint:sha256:") {
		t.Errorf("signedBy = %q, want host-key-fingerprint:sha256: prefix", m.SignedBy)
	}
	if err := VerifyManifest(&m, key.Pub); err != nil {
		t.Errorf("VerifyManifest on freshly-signed manifest: %v", err)
	}
}

// TestManifest_VerifyDetectsBodyTamper covers the security-critical
// path: any mutation of a signed field must invalidate the
// signature. We test ContentHash here because that's the field a
// restore attacker would most want to swap (point to a different
// payload).
func TestManifest_VerifyDetectsBodyTamper(t *testing.T) {
	key := mustHostKey(t)
	m := Manifest{ContentHash: "sha256:original"}
	if err := SignManifest(&m, key); err != nil {
		t.Fatal(err)
	}
	m.ContentHash = "sha256:tampered"
	err := VerifyManifest(&m, key.Pub)
	var ve *ManifestVerificationError
	if !errors.As(err, &ve) {
		t.Errorf("VerifyManifest on tampered manifest: err = %v, want ManifestVerificationError", err)
	}
}

// TestManifest_VerifyRejectsWrongKey covers signer-swap attacks:
// an attacker re-signs with their own key and updates SignedBy to
// claim that key — VerifyManifest must reject if the caller's
// pub doesn't match SignedBy.
func TestManifest_VerifyRejectsWrongKey(t *testing.T) {
	keyA := mustHostKey(t)
	keyB := mustHostKey(t)
	m := Manifest{ContentHash: "sha256:abc"}
	if err := SignManifest(&m, keyA); err != nil {
		t.Fatal(err)
	}
	// Verify with B's pub: SignedBy says "fingerprint of A", but
	// we're checking against B → mismatch must surface as
	// ManifestVerificationError.
	err := VerifyManifest(&m, keyB.Pub)
	var ve *ManifestVerificationError
	if !errors.As(err, &ve) {
		t.Errorf("VerifyManifest with foreign pubkey: err = %v, want ManifestVerificationError", err)
	}
}

func TestManifest_VerifyRejectsBrokenSignatureEncoding(t *testing.T) {
	key := mustHostKey(t)
	m := Manifest{ContentHash: "sha256:abc"}
	if err := SignManifest(&m, key); err != nil {
		t.Fatal(err)
	}
	m.Signature = "ed25519:not-base64-!!"
	err := VerifyManifest(&m, key.Pub)
	var ve *ManifestVerificationError
	if !errors.As(err, &ve) {
		t.Errorf("expected ManifestVerificationError on malformed signature, got %v", err)
	}
}

func TestManifest_VerifyRejectsMissingPrefix(t *testing.T) {
	key := mustHostKey(t)
	m := Manifest{ContentHash: "sha256:abc"}
	if err := SignManifest(&m, key); err != nil {
		t.Fatal(err)
	}
	// Strip the "ed25519:" prefix.
	m.Signature = strings.TrimPrefix(m.Signature, sigPrefix)
	err := VerifyManifest(&m, key.Pub)
	var ve *ManifestVerificationError
	if !errors.As(err, &ve) {
		t.Errorf("expected ManifestVerificationError on missing prefix, got %v", err)
	}
}

func TestSignManifest_RejectsNilKey(t *testing.T) {
	m := Manifest{}
	if err := SignManifest(&m, nil); err == nil {
		t.Error("SignManifest with nil key should error")
	}
}

// TestHashContent_Stable covers the contentHash invariant: same
// inputs → same output, distinct inputs → distinct output, and
// order matters (so a swapped state+audit can't satisfy the same
// hash).
func TestHashContent_Stable(t *testing.T) {
	h1 := hashContent([]byte("alpha"), []byte("gamma"), []byte("beta"))
	h2 := hashContent([]byte("alpha"), []byte("gamma"), []byte("beta"))
	if h1 != h2 {
		t.Error("same input must yield same hash")
	}
	h3 := hashContent([]byte("beta"), []byte("gamma"), []byte("alpha")) // swap state/audit
	if h1 == h3 {
		t.Error("swapping state/audit must change hash (concatenation order is load-bearing)")
	}
	h4 := hashContent([]byte("alpha"), []byte("delta"), []byte("beta")) // change WAL
	if h1 == h4 {
		t.Error("changing WAL bytes must change hash")
	}
}

// silence unused import in case build tags ever exclude
// crypto/ed25519+rand here.
var _ = ed25519.GenerateKey
var _ = rand.Reader
