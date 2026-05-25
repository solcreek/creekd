package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeReleaseFixture stages a goreleaser-shaped release tree on
// disk + returns an httptest server that serves it under the
// /vX/<artifact> path convention runSelfUpgrade expects.
//
// fakeBinaryContent is what the dummy creekd + creekctl files
// contain — assertions can later read the swapped binary and
// confirm this exact string landed.
func fakeReleaseFixture(t *testing.T, version, fakeBinaryContent string) (releaseBase string, latestRedirect string, tarName string) {
	t.Helper()
	osName := runtime.GOOS
	arch := runtime.GOARCH
	verNoV := strings.TrimPrefix(version, "v")
	tarName = fmt.Sprintf("creekd_%s_%s_%s.tar.gz", verNoV, osName, arch)

	dir := t.TempDir()
	tarPath := filepath.Join(dir, tarName)
	makeFakeTarball(t, tarPath, fakeBinaryContent)

	sum := sha256OfFile(t, tarPath)
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"),
		[]byte(sum+"  "+tarName+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt.sig"),
		[]byte("not-a-real-sig"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt.pem"),
		[]byte("not-a-real-cert"), 0o644); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	// /releases/download/<tag>/<file>
	mux.HandleFunc("/releases/download/"+version+"/", func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		http.ServeFile(w, r, filepath.Join(dir, name))
	})
	// /releases/latest → redirect to /releases/tag/<version>.
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/releases/tag/"+version)
		w.WriteHeader(http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/releases/download", srv.URL + "/releases/latest", tarName
}

// makeFakeTarball writes a tar.gz at path containing two regular
// files (creekd + creekctl) both with the given content. Mimics
// goreleaser's tarball shape closely enough for extractTarGz to
// pick them up.
func makeFakeTarball(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"creekd", "creekctl"} {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	_ = f.Close()
}

func sha256OfFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeFakeCosignAlwaysAccept writes a shell script at path that
// always succeeds — substitutes for real cosign in tests.
func writeFakeCosignAlwaysAccept(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake cosign script needs POSIX shell")
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeFakeCosignAlwaysReject writes a shell script at path that
// always fails — simulates a tampered signature.
func writeFakeCosignAlwaysReject(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake cosign script needs POSIX shell")
	}
	if err := os.WriteFile(path,
		[]byte("#!/bin/sh\necho 'Error: identity mismatch'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestRunSelfUpgrade_HappyPath covers the canonical end-to-end:
// fake release served over httptest + fake cosign accept → both
// dst binaries get replaced with the tarball's content.
func TestRunSelfUpgrade_HappyPath(t *testing.T) {
	const ver = "v0.0.99"
	const fakeBody = "FRESH-RELEASE-BINARY-BYTES"
	releaseBase, _, _ := fakeReleaseFixture(t, ver, fakeBody)

	// Install dst directory with stale bytes.
	binDir := t.TempDir()
	dst1 := filepath.Join(binDir, "creekd")
	dst2 := filepath.Join(binDir, "creekctl")
	for _, p := range []string{dst1, dst2} {
		if err := os.WriteFile(p, []byte("OLD"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cosign := filepath.Join(t.TempDir(), "cosign")
	writeFakeCosignAlwaysAccept(t, cosign)
	t.Setenv("CREEKCTL_COSIGN_PATH", cosign)

	out, err := runSub(t, "self-upgrade", []string{
		"--to", ver,
		"--release-base", releaseBase,
		"--creekd", dst1,
		"--creekctl", dst2,
	})
	if err != nil {
		t.Fatalf("self-upgrade: %v\noutput: %s", err, out)
	}

	for _, p := range []string{dst1, dst2} {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != fakeBody {
			t.Errorf("post-upgrade %s = %q, want %q", filepath.Base(p), got, fakeBody)
		}
	}
}

// TestRunSelfUpgrade_RejectsBadSignature covers the security-
// critical path: when cosign rejects, the dst binaries MUST NOT
// be touched, AND the error message MUST carry the
// upgrade_signature_invalid code so admin scripts can branch on it.
func TestRunSelfUpgrade_RejectsBadSignature(t *testing.T) {
	const ver = "v0.0.99"
	releaseBase, _, _ := fakeReleaseFixture(t, ver, "MALICIOUS")

	binDir := t.TempDir()
	dst1 := filepath.Join(binDir, "creekd")
	dst2 := filepath.Join(binDir, "creekctl")
	for _, p := range []string{dst1, dst2} {
		if err := os.WriteFile(p, []byte("ORIGINAL"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cosign := filepath.Join(t.TempDir(), "cosign")
	writeFakeCosignAlwaysReject(t, cosign)
	t.Setenv("CREEKCTL_COSIGN_PATH", cosign)

	_, err := runSub(t, "self-upgrade", []string{
		"--to", ver,
		"--release-base", releaseBase,
		"--creekd", dst1,
		"--creekctl", dst2,
	})
	if err == nil {
		t.Fatal("self-upgrade with rejecting cosign should error")
	}
	if !strings.Contains(err.Error(), "upgrade_signature_invalid") {
		t.Errorf("err = %v, want to contain upgrade_signature_invalid", err)
	}
	for _, p := range []string{dst1, dst2} {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "ORIGINAL" {
			t.Errorf("rejected upgrade clobbered %s: got %q, want ORIGINAL", filepath.Base(p), got)
		}
	}
}

// TestRunSelfUpgrade_ResolvesLatestRedirect covers the --to=""
// (default) path: with no version pinned, the subcommand follows
// the /releases/latest redirect to discover the tag.
func TestRunSelfUpgrade_ResolvesLatestRedirect(t *testing.T) {
	const ver = "v0.0.77"
	releaseBase, latestURL, _ := fakeReleaseFixture(t, ver, "BODY")

	binDir := t.TempDir()
	dst1 := filepath.Join(binDir, "creekd")
	dst2 := filepath.Join(binDir, "creekctl")
	for _, p := range []string{dst1, dst2} {
		if err := os.WriteFile(p, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cosign := filepath.Join(t.TempDir(), "cosign")
	writeFakeCosignAlwaysAccept(t, cosign)
	t.Setenv("CREEKCTL_COSIGN_PATH", cosign)

	out, err := runSub(t, "self-upgrade", []string{
		"--release-base", releaseBase,
		"--latest-url", latestURL,
		"--creekd", dst1,
		"--creekctl", dst2,
	})
	if err != nil {
		t.Fatalf("self-upgrade with default --to: %v\n%s", err, out)
	}
	if !strings.Contains(out, ver) {
		t.Errorf("output missing resolved tag %q: %s", ver, out)
	}
}

// TestRunSelfUpgrade_DefaultsCreekdToSibling covers the path
// resolution default: when --creekd is omitted, the subcommand
// derives it from the creekctl path's directory.
func TestRunSelfUpgrade_DefaultsCreekdToSibling(t *testing.T) {
	const ver = "v0.0.55"
	const body = "SIBLING-DERIVED"
	releaseBase, _, _ := fakeReleaseFixture(t, ver, body)

	binDir := t.TempDir()
	ctl := filepath.Join(binDir, "creekctl")
	siblingDaemon := filepath.Join(binDir, "creekd")
	for _, p := range []string{ctl, siblingDaemon} {
		if err := os.WriteFile(p, []byte("OLD"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cosign := filepath.Join(t.TempDir(), "cosign")
	writeFakeCosignAlwaysAccept(t, cosign)
	t.Setenv("CREEKCTL_COSIGN_PATH", cosign)

	_, err := runSub(t, "self-upgrade", []string{
		"--to", ver,
		"--release-base", releaseBase,
		"--creekctl", ctl,
		// NO --creekd — should derive from ctl's directory.
	})
	if err != nil {
		t.Fatalf("self-upgrade: %v", err)
	}
	got, err := os.ReadFile(siblingDaemon)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("sibling creekd = %q, want %q (derived path swap)", got, body)
	}
}
