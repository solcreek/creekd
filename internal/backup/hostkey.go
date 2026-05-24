package backup

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// HostKey is a persisted ed25519 keypair used to sign Tier 0
// backup manifests. The on-disk encoding is the raw 64-byte
// ed25519 private key (which embeds the public key in its second
// half). Fingerprint is sha256(public key) in hex, prefixed with
// "sha256:".
type HostKey struct {
	Priv        ed25519.PrivateKey
	Pub         ed25519.PublicKey
	Fingerprint string
}

// hostKeyFileMode is 0600 — only the creekd user may read the
// private key.
const hostKeyFileMode fs.FileMode = 0600

// LoadOrCreateHostKey reads an ed25519 private key from path, or
// generates and persists a new one if path does not exist. The
// parent directory must already exist (creekd's data-dir is
// created by NewStore + the systemd unit, not here).
//
// This is the minimal scaffold needed for Tier 0 backup signing.
// The TOFU pinning ceremony, fingerprint display, and rotation
// machinery land separately in #21.
func LoadOrCreateHostKey(path string) (*HostKey, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		return parseHostKey(raw)
	case errors.Is(err, fs.ErrNotExist):
		return generateAndPersistHostKey(path)
	default:
		return nil, fmt.Errorf("backup: read hostkey %s: %w", path, err)
	}
}

func parseHostKey(raw []byte) (*HostKey, error) {
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("backup: hostkey size = %d, want %d", len(raw), ed25519.PrivateKeySize)
	}
	priv := ed25519.PrivateKey(raw)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("backup: hostkey does not expose ed25519 public")
	}
	return &HostKey{Priv: priv, Pub: pub, Fingerprint: fingerprintPub(pub)}, nil
}

func generateAndPersistHostKey(path string) (*HostKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("backup: generate hostkey: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, priv, hostKeyFileMode); err != nil {
		return nil, fmt.Errorf("backup: write hostkey tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("backup: rename hostkey: %w", err)
	}
	if dirFd, derr := os.Open(filepath.Dir(path)); derr == nil {
		_ = dirFd.Sync()
		_ = dirFd.Close()
	}
	return &HostKey{Priv: priv, Pub: pub, Fingerprint: fingerprintPub(pub)}, nil
}

func fingerprintPub(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "sha256:" + hex.EncodeToString(sum[:])
}
