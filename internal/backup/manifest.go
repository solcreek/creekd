package backup

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// Manifest is the signed payload accompanying a Tier 0 backup.
//
// All hash fields are hex with a "sha256:" prefix. The signature
// is "ed25519:" + base64(64 raw signature bytes). The signature
// covers the JSON encoding of the manifest with Signature left
// empty — see signableBytes for the exact rule.
//
// ContentHash is a rolled-up sha256 over (state || wal || audit)
// — convenient for a single-shot "did anything change" check, but
// can't tell you WHICH file is corrupted on failure. Files carries
// per-archive-member digests so verify-on-restore can identify the
// specific corrupted file (state.json vs state.json.wal vs
// audit.log vs an extras/ entry).
type Manifest struct {
	CreekdVersion      string            `json:"creekdVersion"`
	SchemaVersion      int               `json:"schemaVersion"`
	BackupTimestamp    string            `json:"backupTimestamp"`
	AuditLogTipHash    string            `json:"auditLogTipHash"`
	FleetCAFingerprint string            `json:"fleetCAFingerprint"`
	ContentHash        string            `json:"contentHash"`
	Files              map[string]string `json:"files,omitempty"`
	SignedBy           string            `json:"signedBy"`
	Signature          string            `json:"signature"`
}

// hashOne returns "sha256:" + hex(sha256(data)). Used to populate
// Manifest.Files entries.
func hashOne(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// ManifestVerificationError signals that a parsed manifest failed
// either the signature check or the signed-by/key mismatch check.
// Restore distinguishes this from on-disk content corruption
// (which surfaces as a content-hash mismatch when the untarred
// state.json + audit.log don't hash to ContentHash).
type ManifestVerificationError struct {
	Reason string
}

func (e *ManifestVerificationError) Error() string {
	return "backup: manifest verification failed: " + e.Reason
}

const sigPrefix = "ed25519:"

// SignManifest fills in m.SignedBy and m.Signature, overwriting
// whatever those fields held.
func SignManifest(m *Manifest, key *HostKey) error {
	if key == nil || len(key.Priv) != ed25519.PrivateKeySize {
		return errors.New("backup: SignManifest requires a non-nil hostkey")
	}
	m.SignedBy = "host-key-fingerprint:" + key.Fingerprint
	payload, err := signableBytes(m)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(key.Priv, payload)
	m.Signature = sigPrefix + base64.StdEncoding.EncodeToString(sig)
	return nil
}

// VerifyManifest checks m.Signature against pub. The caller is
// responsible for proving that pub is the right key (typically by
// recomputing its fingerprint and comparing to m.SignedBy).
func VerifyManifest(m *Manifest, pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return &ManifestVerificationError{Reason: "public key wrong size"}
	}
	wantFP := "host-key-fingerprint:" + fingerprintPub(pub)
	if m.SignedBy != wantFP {
		return &ManifestVerificationError{Reason: fmt.Sprintf("signedBy = %q, want %q", m.SignedBy, wantFP)}
	}
	if len(m.Signature) <= len(sigPrefix) || m.Signature[:len(sigPrefix)] != sigPrefix {
		return &ManifestVerificationError{Reason: "signature missing ed25519: prefix"}
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature[len(sigPrefix):])
	if err != nil {
		return &ManifestVerificationError{Reason: "signature base64: " + err.Error()}
	}
	payload, err := signableBytes(m)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, payload, sig) {
		return &ManifestVerificationError{Reason: "ed25519 verify failed"}
	}
	return nil
}

// signableBytes returns the bytes signed by the manifest: a JSON
// encoding of m with Signature blanked out. SignedBy is
// included — once we've committed to a signer, swapping signers
// must invalidate the signature.
func signableBytes(m *Manifest) ([]byte, error) {
	cp := *m
	cp.Signature = ""
	return json.Marshal(&cp)
}

// hashContent returns "sha256:" + hex(sha256(stateJSON || walJSON || auditLog)).
// Any input may be empty; concatenation order is fixed.
func hashContent(stateJSON, walJSON, auditLog []byte) string {
	h := sha256.New()
	h.Write(stateJSON)
	h.Write(walJSON)
	h.Write(auditLog)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
