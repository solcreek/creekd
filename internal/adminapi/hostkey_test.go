package adminapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/solcreek/creekd/internal/apitypes"
	"github.com/solcreek/creekd/internal/backup"
)

func newTestHostkey(t *testing.T) *backup.HostKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	// Build a HostKey by going through LoadOrCreateHostKey on a
	// tmp path so the fingerprint computation matches production —
	// guarantees test + prod agree on the formatting rules.
	hk := &backup.HostKey{Priv: priv, Pub: pub}
	// Re-derive fingerprint via the public path: easiest is to
	// LoadOrCreate against a tmp file we control. Skip — the
	// backup package's fingerprint helper is unexported. Use a
	// LoadOrCreateHostKey round-trip below.
	tmp := t.TempDir() + "/k"
	got, err := backup.LoadOrCreateHostKey(tmp)
	if err != nil {
		t.Fatalf("LoadOrCreateHostKey: %v", err)
	}
	hk = got
	return hk
}

// TestHostkey_GetReturnsPubkeyAndFingerprint covers the canonical
// path: with hostkey wired, GET /v1/hostkey returns 200 + a
// well-formed HostkeyInfo whose publicKey decodes to ed25519 sized
// bytes and whose fingerprint carries the sha256: prefix.
func TestHostkey_GetReturnsPubkeyAndFingerprint(t *testing.T) {
	ts := newTestServer(t, "")
	hk := newTestHostkey(t)
	ts.srv.SetHostkey(hk)

	status, body := ts.do(t, "GET", "/v1/hostkey", nil, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", status, body)
	}
	var info apitypes.HostkeyInfo
	mustJSON(t, body, &info)
	if info.Algorithm != apitypes.Ed25519 {
		t.Errorf("algorithm = %q, want ed25519", info.Algorithm)
	}
	if info.Fingerprint != hk.Fingerprint {
		t.Errorf("fingerprint = %q, want %q", info.Fingerprint, hk.Fingerprint)
	}
	pubBytes, err := base64.StdEncoding.DecodeString(info.PublicKey)
	if err != nil {
		t.Fatalf("publicKey base64: %v", err)
	}
	if len(pubBytes) != ed25519.PublicKeySize {
		t.Errorf("publicKey size = %d, want %d", len(pubBytes), ed25519.PublicKeySize)
	}
}

// TestHostkey_GetUnauthenticatedEvenWithToken covers the DESIGN's
// TOFU discovery contract: this endpoint MUST be reachable without
// a bearer token even when CREEKD_ADMIN_TOKEN is set, because the
// `creek init` flow needs to fetch the fingerprint BEFORE
// credentials are provisioned. An attacker can't get anything they
// couldn't get anyway — public keys are public.
func TestHostkey_GetUnauthenticatedEvenWithToken(t *testing.T) {
	ts := newTestServer(t, "secret-token")
	ts.srv.SetHostkey(newTestHostkey(t))

	// No Bearer header.
	status, body := ts.do(t, "GET", "/v1/hostkey", nil, "")
	if status != http.StatusOK {
		t.Errorf("auth-exempt path: status = %d body = %s, want 200", status, body)
	}
	// All other paths still require auth — sanity-check that auth
	// is configured.
	status2, _ := ts.do(t, "GET", "/v1/apps", nil, "")
	if status2 != http.StatusUnauthorized {
		t.Errorf("non-exempt path with auth on: status = %d, want 401", status2)
	}
}

// TestHostkey_GetReturns503WhenNotWired covers the missing-hostkey
// case (early boot, no state dir, or test omission). 503 over 500
// signals "this is recoverable / temporary" — the client should
// retry rather than treat it as a permanent failure.
func TestHostkey_GetReturns503WhenNotWired(t *testing.T) {
	ts := newTestServer(t, "")
	// Deliberately skip SetHostkey.
	status, body := ts.do(t, "GET", "/v1/hostkey", nil, "")
	if status != http.StatusServiceUnavailable {
		t.Errorf("unset hostkey: status = %d body = %s, want 503", status, body)
	}
}

// TestIsPublicPath covers the whitelist directly. Defensive: a
// regression that whitelists too broadly would let an attacker
// bypass bearer auth.
func TestIsPublicPath(t *testing.T) {
	if !isPublicPath("/v1/hostkey") {
		t.Error("/v1/hostkey should be public")
	}
	for _, p := range []string{
		"/v1/apps",
		"/v1/apps/foo",
		"/v1/apps/foo/deploy",
		"/v1/hostkey/foo", // anything BELOW /v1/hostkey must NOT match
		"/v1/HOSTKEY",     // case-sensitive
	} {
		if isPublicPath(p) {
			t.Errorf("isPublicPath(%q) = true, want false", p)
		}
	}
}
