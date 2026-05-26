package upgrade

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// cosignVerifyTimeout caps how long a single `cosign verify-blob`
// call may run. cosign contacts Rekor + Fulcio, so a network black
// hole would otherwise let self-upgrade hang indefinitely. var,
// not const, so the timeout test can shorten it without a 30s
// wall-clock penalty.
var cosignVerifyTimeout = 30 * time.Second

// ErrSignatureInvalid is returned by Verify when EITHER the cosign
// signature on checksums.txt fails to verify against the expected
// pipeline identity OR the tarball's SHA256 does not match the
// entry in checksums.txt. The two failure modes are deliberately
// collapsed into one sentinel — both mean "do not install this
// binary"; distinguishing them would invite a partial-trust UX
// that misleads operators.
var ErrSignatureInvalid = errors.New("upgrade: signature invalid")

// DefaultIdentityRegex pins the OIDC subject of the signing
// pipeline to this repo's release.yml on a v* tag. Updated only
// when the canonical repo location changes; baked in so a fork
// cannot swap an attacker-controlled identity past verification.
const DefaultIdentityRegex = `^https://github\.com/solcreek/creekd/\.github/workflows/release\.yml@refs/tags/v.*$`

// DefaultOIDCIssuer is GitHub Actions' Fulcio OIDC issuer. Pinned
// so signatures from any other issuer (a forked Actions
// runtime, a self-hosted runner with a different OIDC config,
// etc.) cannot satisfy verification.
const DefaultOIDCIssuer = "https://token.actions.githubusercontent.com"

// Verifier holds the verification configuration. Construct with
// New and override fields for tests (notably CosignPath to point
// at a fake binary).
type Verifier struct {
	// CosignPath is the cosign binary used for verify-blob.
	// New() defaults to "cosign", which is resolved through PATH
	// at exec time. Set to a non-empty absolute path to bypass
	// PATH lookup (commonly used in tests).
	CosignPath string
	// IdentityRegex pins the expected signing-pipeline identity.
	// Defaults to DefaultIdentityRegex.
	IdentityRegex string
	// OIDCIssuer pins the expected OIDC issuer. Defaults to
	// DefaultOIDCIssuer.
	OIDCIssuer string
}

// New returns a Verifier populated with the production defaults.
// Override any field after construction (typically only useful in
// tests).
func New() *Verifier {
	return &Verifier{
		CosignPath:    "cosign",
		IdentityRegex: DefaultIdentityRegex,
		OIDCIssuer:    DefaultOIDCIssuer,
	}
}

// Verify runs the two-layer check on the given release artifacts.
// All paths must exist and be readable. The tarballName is the
// filename as it appears in checksums.txt (e.g.
// "creekd_0.0.5_linux_amd64.tar.gz"); used for entry lookup.
//
// ctx is propagated to the cosign subprocess; cancelling it
// (typically via signal.NotifyContext) aborts a hung verify in
// addition to the internal cosignVerifyTimeout floor.
//
// Returns ErrSignatureInvalid (wrapped) on either failure. Other
// errors (file read, cosign exec failure, missing checksums entry)
// are returned verbatim so callers can distinguish "verification
// said no" from "could not even attempt verification".
func (v *Verifier) Verify(ctx context.Context, tarballPath, tarballName, sigPath, certPath, checksumsPath string) error {
	if err := v.verifyCosign(ctx, sigPath, certPath, checksumsPath); err != nil {
		return err
	}
	return v.verifyChecksum(tarballPath, tarballName, checksumsPath)
}

func (v *Verifier) verifyCosign(parentCtx context.Context, sigPath, certPath, checksumsPath string) error {
	bin := v.CosignPath
	if bin == "" {
		bin = "cosign"
	}
	identity := v.IdentityRegex
	if identity == "" {
		identity = DefaultIdentityRegex
	}
	issuer := v.OIDCIssuer
	if issuer == "" {
		issuer = DefaultOIDCIssuer
	}

	// Bound the cosign call: it queries Rekor + Fulcio over the
	// network, so a connectivity black hole could otherwise wedge
	// self-upgrade forever. Derive from the caller's ctx so a
	// SIGINT also short-circuits the timer.
	ctx, cancel := context.WithTimeout(parentCtx, cosignVerifyTimeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, bin, "verify-blob",
		"--certificate-identity-regexp", identity,
		"--certificate-oidc-issuer", issuer,
		"--certificate", certPath,
		"--signature", sigPath,
		checksumsPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	// Only a normal non-zero exit means cosign rejected the
	// signature. Timeout-SIGKILL also surfaces as *exec.ExitError,
	// so ctx.Err() and Exited() screen those out.
	if ctx.Err() != nil {
		// context.DeadlineExceeded → bounded timeout fired (slow
		// Rekor / Fulcio). context.Canceled → operator SIGINT
		// propagated from the parent ctx. Both are availability,
		// not security; the message distinguishes them so the
		// operator knows whether to retry or fix the install.
		// Report the actual elapsed time — the parent ctx may have
		// fired with a deadline shorter than cosignVerifyTimeout,
		// in which case the literal const would overstate the wait.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("upgrade: cosign unavailable (%s): timed out after %s", bin, time.Since(start).Round(time.Millisecond))
		}
		return fmt.Errorf("upgrade: cosign unavailable (%s): %w", bin, ctx.Err())
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.Exited() {
		return fmt.Errorf("%w: cosign verify-blob: %s", ErrSignatureInvalid, strings.TrimSpace(string(out)))
	}
	return fmt.Errorf("upgrade: cosign unavailable (%s): %w", bin, err)
}

// verifyChecksum looks up tarballName in checksums.txt and
// compares the recorded SHA256 against the actual file.
func (v *Verifier) verifyChecksum(tarballPath, tarballName, checksumsPath string) error {
	want, err := lookupChecksum(checksumsPath, tarballName)
	if err != nil {
		return err
	}
	got, err := sha256File(tarballPath)
	if err != nil {
		return fmt.Errorf("upgrade: hash %s: %w", tarballPath, err)
	}
	if want != got {
		return fmt.Errorf("%w: %s sha256 mismatch (want %s, got %s)",
			ErrSignatureInvalid, tarballName, want, got)
	}
	return nil
}

// lookupChecksum returns the hex SHA256 recorded for name in a
// goreleaser-style checksums.txt: each line is
// `<sha256>  <filename>`. Returns an error (NOT ErrSignatureInvalid)
// when the entry is absent — that's a "wrong file or wrong
// checksums.txt" setup error, not a verification rejection.
func lookupChecksum(checksumsPath, name string) (string, error) {
	f, err := os.Open(checksumsPath)
	if err != nil {
		return "", fmt.Errorf("upgrade: open %s: %w", checksumsPath, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		// Trim leading/trailing whitespace; format is hex + 2-space
		// + filename per coreutils convention.
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if filepath.Base(fields[1]) == name {
			return fields[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("upgrade: scan %s: %w", checksumsPath, err)
	}
	return "", fmt.Errorf("upgrade: no entry for %q in %s", name, checksumsPath)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
