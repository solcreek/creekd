package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// withCosignTimeoutForTest shortens cosignVerifyTimeout for the
// duration of a test and returns a restore function suitable for
// `defer`. Saves a 30s wall-clock penalty on timeout-path tests.
func withCosignTimeoutForTest(d time.Duration) func() {
	prev := cosignVerifyTimeout
	cosignVerifyTimeout = d
	return func() { cosignVerifyTimeout = prev }
}

// writeFakeCosign writes a shell script at path that exits with
// the given code after echoing whatever message is passed via
// args. Lets tests simulate cosign's accept/reject behaviour
// without installing real cosign.
func writeFakeCosign(t *testing.T, path string, exitCode int, message string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake cosign script needs POSIX shell")
	}
	script := fmt.Sprintf(`#!/bin/sh
echo %q
exit %d
`, message, exitCode)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cosign: %v", err)
	}
}

// stageArtifacts writes a synthetic release set into dir: a
// tarball (named tarballName) of `body`, a checksums.txt mapping
// tarballName → sha256(body), and empty sig + pem files.
// Returns the paths in the order Verify expects them.
func stageArtifacts(t *testing.T, dir, tarballName, body string) (tarballPath, sigPath, certPath, checksumsPath string) {
	t.Helper()
	tarballPath = filepath.Join(dir, tarballName)
	if err := os.WriteFile(tarballPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	checksumsPath = filepath.Join(dir, "checksums.txt")
	line := hex.EncodeToString(sum[:]) + "  " + tarballName + "\n"
	if err := os.WriteFile(checksumsPath, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	sigPath = filepath.Join(dir, "checksums.txt.sig")
	if err := os.WriteFile(sigPath, []byte("not-a-real-sig"), 0o644); err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "checksums.txt.pem")
	if err := os.WriteFile(certPath, []byte("not-a-real-cert"), 0o644); err != nil {
		t.Fatal(err)
	}
	return
}

// TestVerify_HappyPath covers the canonical accept: fake cosign
// exits 0 AND the tarball's SHA256 matches checksums.txt. Verify
// returns nil — installation may proceed.
func TestVerify_HappyPath(t *testing.T) {
	dir := t.TempDir()
	tar, sig, cert, sums := stageArtifacts(t, dir, "creekd_x.tar.gz", "release-bytes")
	cosign := filepath.Join(dir, "cosign")
	writeFakeCosign(t, cosign, 0, "Verified OK")

	v := New()
	v.CosignPath = cosign
	if err := v.Verify(context.Background(), tar, "creekd_x.tar.gz", sig, cert, sums); err != nil {
		t.Errorf("Verify on valid artifacts: %v", err)
	}
}

// TestVerify_RejectsCosignFailure covers the security-critical
// signature-rejection path. Fake cosign exits non-zero → Verify
// returns ErrSignatureInvalid (wrapped).
func TestVerify_RejectsCosignFailure(t *testing.T) {
	dir := t.TempDir()
	tar, sig, cert, sums := stageArtifacts(t, dir, "creekd_x.tar.gz", "release-bytes")
	cosign := filepath.Join(dir, "cosign")
	writeFakeCosign(t, cosign, 1, "Error: signature does not match expected identity")

	v := New()
	v.CosignPath = cosign
	err := v.Verify(context.Background(), tar, "creekd_x.tar.gz", sig, cert, sums)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Errorf("err = %v, want errors.Is(ErrSignatureInvalid)", err)
	}
}

// TestVerify_RejectsTamperedTarball covers the second
// verification layer: cosign accepts the (untampered) checksums
// file, but the tarball on disk has been swapped — SHA256
// mismatch surfaces ErrSignatureInvalid.
func TestVerify_RejectsTamperedTarball(t *testing.T) {
	dir := t.TempDir()
	tar, sig, cert, sums := stageArtifacts(t, dir, "creekd_x.tar.gz", "original-bytes")
	cosign := filepath.Join(dir, "cosign")
	writeFakeCosign(t, cosign, 0, "Verified OK")

	// Tamper: overwrite the tarball after checksums was computed.
	if err := os.WriteFile(tar, []byte("attacker-substituted-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	v := New()
	v.CosignPath = cosign
	err := v.Verify(context.Background(), tar, "creekd_x.tar.gz", sig, cert, sums)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Errorf("tampered tarball: err = %v, want errors.Is(ErrSignatureInvalid)", err)
	}
}

// TestVerify_MissingChecksumsEntry covers the setup-error path:
// the tarball name isn't in checksums.txt at all. This is a
// configuration mistake, NOT a signature rejection — Verify
// returns a plain error (NOT ErrSignatureInvalid) so the caller
// can distinguish "wrong file" from "untrusted bytes".
func TestVerify_MissingChecksumsEntry(t *testing.T) {
	dir := t.TempDir()
	tar, sig, cert, sums := stageArtifacts(t, dir, "creekd_x.tar.gz", "bytes")
	cosign := filepath.Join(dir, "cosign")
	writeFakeCosign(t, cosign, 0, "OK")

	v := New()
	v.CosignPath = cosign
	err := v.Verify(context.Background(), tar, "creekd_NOT_LISTED.tar.gz", sig, cert, sums)
	if err == nil {
		t.Fatal("missing checksums entry should error")
	}
	if errors.Is(err, ErrSignatureInvalid) {
		t.Errorf("missing entry should NOT surface as ErrSignatureInvalid (setup error, not security): %v", err)
	}
}

// TestVerify_CosignNotInstalled covers the dependency-missing
// path: cosign isn't on PATH at all. This is again NOT a
// security failure — the caller may want to fall back to
// checksum-only mode (matching install.sh's soft-attempt
// semantics) rather than treating it as untrusted.
func TestVerify_CosignNotInstalled(t *testing.T) {
	dir := t.TempDir()
	tar, sig, cert, sums := stageArtifacts(t, dir, "creekd_x.tar.gz", "bytes")

	v := New()
	v.CosignPath = filepath.Join(dir, "definitely-not-here")
	err := v.Verify(context.Background(), tar, "creekd_x.tar.gz", sig, cert, sums)
	if err == nil {
		t.Fatal("missing cosign should error")
	}
	if errors.Is(err, ErrSignatureInvalid) {
		t.Errorf("missing cosign should NOT surface as ErrSignatureInvalid: %v", err)
	}
}

// TestVerify_CosignNotExecutable covers the EACCES path: cosign
// exists on the configured path but is chmod -x. Like the
// not-installed case, this is a setup problem (the operator's
// install is broken), NOT a signature rejection — surfacing it as
// ErrSignatureInvalid would point the operator at the wrong fix.
func TestVerify_CosignNotExecutable(t *testing.T) {
	dir := t.TempDir()
	tar, sig, cert, sums := stageArtifacts(t, dir, "creekd_x.tar.gz", "bytes")
	cosign := filepath.Join(dir, "cosign")
	// Mode 0644: file exists, not executable.
	if err := os.WriteFile(cosign, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	v := New()
	v.CosignPath = cosign
	err := v.Verify(context.Background(), tar, "creekd_x.tar.gz", sig, cert, sums)
	if err == nil {
		t.Fatal("non-executable cosign should error")
	}
	if errors.Is(err, ErrSignatureInvalid) {
		t.Errorf("non-executable cosign should NOT surface as ErrSignatureInvalid (setup error, not security): %v", err)
	}
}

// TestVerify_CosignTimeoutNotSignatureRejection covers the
// deadline path: a hung cosign (e.g. Rekor network black hole)
// must surface as "unavailable", NOT as ErrSignatureInvalid. The
// trap here is that exec.CommandContext kills the process via
// SIGKILL on timeout, which produces an *exec.ExitError — easy to
// mistake for a non-zero verdict if the classifier only checks
// the type. The implementation guards by inspecting ctx.Err()
// before falling through to the ExitError branch.
func TestVerify_CosignTimeoutNotSignatureRejection(t *testing.T) {
	dir := t.TempDir()
	tar, sig, cert, sums := stageArtifacts(t, dir, "creekd_x.tar.gz", "bytes")

	// Fake cosign that sleeps far longer than the test override.
	cosign := filepath.Join(dir, "cosign")
	if err := os.WriteFile(cosign, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer withCosignTimeoutForTest(200 * time.Millisecond)()

	v := New()
	v.CosignPath = cosign
	err := v.Verify(context.Background(), tar, "creekd_x.tar.gz", sig, cert, sums)
	if err == nil {
		t.Fatal("hung cosign should error")
	}
	if errors.Is(err, ErrSignatureInvalid) {
		t.Errorf("timeout should NOT surface as ErrSignatureInvalid (setup/availability error): %v", err)
	}
}

// TestVerify_CtxCancelInterruptsCosign covers the signal-handling
// contract: an already-cancelled caller ctx must abort verifyCosign
// without waiting for the internal cosignVerifyTimeout. This is
// what lets `creekctl self-upgrade` honor Ctrl-C / SIGINT during a
// hung Rekor lookup instead of riding out the full 30s.
func TestVerify_CtxCancelInterruptsCosign(t *testing.T) {
	dir := t.TempDir()
	tar, sig, cert, sums := stageArtifacts(t, dir, "creekd_x.tar.gz", "bytes")
	cosign := filepath.Join(dir, "cosign")
	if err := os.WriteFile(cosign, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	v := New()
	v.CosignPath = cosign

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled — should fail fast, not wait 30s

	start := time.Now()
	err := v.Verify(ctx, tar, "creekd_x.tar.gz", sig, cert, sums)
	if err == nil {
		t.Fatal("pre-cancelled ctx should error")
	}
	if errors.Is(err, ErrSignatureInvalid) {
		t.Errorf("cancellation should NOT surface as ErrSignatureInvalid: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("ctx cancel took %s — should be near-instant, not waiting for cosignVerifyTimeout", elapsed)
	}
}

// TestVerify_PassesPipelineIdentityToCosign covers the security
// invariant that the cosign invocation actually pins the expected
// identity regex — a regression that omitted --certificate-identity-regexp
// would silently accept any signed checksums file.
func TestVerify_PassesPipelineIdentityToCosign(t *testing.T) {
	dir := t.TempDir()
	tar, sig, cert, sums := stageArtifacts(t, dir, "creekd_x.tar.gz", "bytes")

	// Fake cosign that echoes its own argv so the test can grep
	// for the expected flags.
	cosign := filepath.Join(dir, "cosign")
	echoScript := `#!/bin/sh
echo "$@" > "$0.argv"
exit 0
`
	if err := os.WriteFile(cosign, []byte(echoScript), 0o755); err != nil {
		t.Fatal(err)
	}

	v := New()
	v.CosignPath = cosign
	if err := v.Verify(context.Background(), tar, "creekd_x.tar.gz", sig, cert, sums); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(cosign + ".argv")
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{
		"--certificate-identity-regexp",
		DefaultIdentityRegex,
		"--certificate-oidc-issuer",
		DefaultOIDCIssuer,
		"verify-blob",
	} {
		if !contains(string(argv), must) {
			t.Errorf("cosign argv missing %q: %s", must, argv)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) &&
		(haystack == needle ||
			indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
