package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Tier0Keep is the default retention: 168 hourly artifacts = one
// week.
const Tier0Keep = 168

// Options controls a single Tier 0 backup run. StateDir + BackupDir
// + HostKey are mandatory; ACMEDir + SystemdUnitPath are optional
// (skipped silently if missing — the unit may not be installed in
// dev mode).
type Options struct {
	StateDir        string
	BackupDir       string
	ACMEDir         string
	SystemdUnitPath string
	CreekdVersion   string
	SchemaVersion   int
	Keep            int
	HostKey         *HostKey
	// Now is injected so tests can assert deterministic timestamps;
	// production callers leave it nil for time.Now().
	Now func() time.Time
}

// Result describes a finished backup run.
type Result struct {
	ArtifactPath string
	Manifest     Manifest
	Pruned       []string
}

// WriteTier0 produces one backup tar.gz under opts.BackupDir.
// Steps: read state.json + audit.log → compute contentHash →
// build + sign manifest → tar+gzip everything → fsync → prune.
func WriteTier0(opts Options) (*Result, error) {
	if err := validateOptions(&opts); err != nil {
		return nil, err
	}

	stateBytes, err := os.ReadFile(filepath.Join(opts.StateDir, "state.json"))
	if err != nil {
		return nil, fmt.Errorf("backup: read state.json: %w", err)
	}
	auditBytes, auditTipHash, err := readAuditLog(filepath.Join(opts.StateDir, "audit.log"))
	if err != nil {
		return nil, err
	}

	now := opts.Now().UTC()
	m := Manifest{
		CreekdVersion:      opts.CreekdVersion,
		SchemaVersion:      opts.SchemaVersion,
		BackupTimestamp:    now.Format(time.RFC3339),
		AuditLogTipHash:    auditTipHash,
		FleetCAFingerprint: opts.HostKey.Fingerprint, // Stage 0: hostkey IS the trust anchor
		ContentHash:        hashContent(stateBytes, auditBytes),
	}
	if err := SignManifest(&m, opts.HostKey); err != nil {
		return nil, err
	}
	manifestJSON, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("backup: marshal manifest: %w", err)
	}

	artifactName := fmt.Sprintf("state-%d.tar.gz", now.Unix())
	artifactPath := filepath.Join(opts.BackupDir, artifactName)
	if err := writeArtifact(artifactPath, &opts, stateBytes, auditBytes, manifestJSON); err != nil {
		return nil, err
	}

	pruned, err := pruneOldest(opts.BackupDir, opts.Keep)
	if err != nil {
		return nil, fmt.Errorf("backup: prune: %w", err)
	}
	return &Result{ArtifactPath: artifactPath, Manifest: m, Pruned: pruned}, nil
}

func validateOptions(opts *Options) error {
	if opts.StateDir == "" {
		return errors.New("backup: StateDir required")
	}
	if opts.BackupDir == "" {
		return errors.New("backup: BackupDir required")
	}
	if opts.HostKey == nil {
		return errors.New("backup: HostKey required")
	}
	if opts.Keep == 0 {
		opts.Keep = Tier0Keep
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := os.MkdirAll(opts.BackupDir, 0o750); err != nil {
		return fmt.Errorf("backup: mkdir BackupDir: %w", err)
	}
	return nil
}

// readAuditLog returns the file's full contents plus
// sha256(last_line) — the audit chain's tip — so the manifest can
// pin chain continuity. A missing file is permitted (fresh host,
// no admin actions yet) and yields zero bytes + "none".
func readAuditLog(path string) ([]byte, string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "none", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("backup: read audit.log: %w", err)
	}
	tip := auditTipHash(raw)
	return raw, tip, nil
}

func auditTipHash(raw []byte) string {
	trimmed := bytes.TrimRight(raw, "\n")
	if len(trimmed) == 0 {
		return "none"
	}
	idx := bytes.LastIndexByte(trimmed, '\n')
	lastLine := trimmed[idx+1:]
	sum := sha256.Sum256(lastLine)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeArtifact(path string, opts *Options, stateBytes, auditBytes, manifestJSON []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("backup: open artifact tmp: %w", err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	if err := writeTarFile(tw, "state.json", stateBytes, opts.Now()); err != nil {
		return closeTmp(tw, gz, f, tmp, err)
	}
	if len(auditBytes) > 0 {
		if err := writeTarFile(tw, "audit.log", auditBytes, opts.Now()); err != nil {
			return closeTmp(tw, gz, f, tmp, err)
		}
	}
	if opts.ACMEDir != "" {
		if err := writeTarDir(tw, "acme/", opts.ACMEDir, opts.Now()); err != nil {
			return closeTmp(tw, gz, f, tmp, err)
		}
	}
	if opts.SystemdUnitPath != "" {
		if err := writeTarMaybe(tw, "creekd.service", opts.SystemdUnitPath, opts.Now()); err != nil {
			return closeTmp(tw, gz, f, tmp, err)
		}
	}
	if err := writeTarFile(tw, "MANIFEST.json", manifestJSON, opts.Now()); err != nil {
		return closeTmp(tw, gz, f, tmp, err)
	}

	if err := tw.Close(); err != nil {
		return closeTmp(nil, gz, f, tmp, fmt.Errorf("backup: tar close: %w", err))
	}
	if err := gz.Close(); err != nil {
		return closeTmp(nil, nil, f, tmp, fmt.Errorf("backup: gz close: %w", err))
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("backup: fsync artifact: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("backup: close artifact: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("backup: rename artifact: %w", err)
	}
	if dirFd, derr := os.Open(filepath.Dir(path)); derr == nil {
		_ = dirFd.Sync()
		_ = dirFd.Close()
	}
	return nil
}

func closeTmp(tw *tar.Writer, gz *gzip.Writer, f *os.File, tmp string, cause error) error {
	if tw != nil {
		_ = tw.Close()
	}
	if gz != nil {
		_ = gz.Close()
	}
	_ = f.Close()
	_ = os.Remove(tmp)
	return cause
}

func writeTarFile(tw *tar.Writer, name string, data []byte, mtime time.Time) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    0o640,
		Size:    int64(len(data)),
		ModTime: mtime,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("backup: tar header %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("backup: tar body %s: %w", name, err)
	}
	return nil
}

func writeTarMaybe(tw *tar.Writer, name, srcPath string, mtime time.Time) error {
	data, err := os.ReadFile(srcPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("backup: read %s: %w", srcPath, err)
	}
	return writeTarFile(tw, name, data, mtime)
}

func writeTarDir(tw *tar.Writer, tarPrefix, srcDir string, mtime time.Time) error {
	entries, err := os.ReadDir(srcDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("backup: readdir %s: %w", srcDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(srcDir, e.Name()))
		if err != nil {
			return fmt.Errorf("backup: read %s/%s: %w", srcDir, e.Name(), err)
		}
		if err := writeTarFile(tw, tarPrefix+e.Name(), data, mtime); err != nil {
			return err
		}
	}
	return nil
}

// pruneOldest deletes the oldest state-*.tar.gz files in dir so
// that no more than keep remain. Returns the paths it removed.
func pruneOldest(dir string, keep int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var artifacts []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "state-") && strings.HasSuffix(e.Name(), ".tar.gz") {
			artifacts = append(artifacts, e.Name())
		}
	}
	if len(artifacts) <= keep {
		return nil, nil
	}
	sort.Strings(artifacts) // unix-second filename → lex-sort == chrono-sort
	overflow := len(artifacts) - keep
	pruned := make([]string, 0, overflow)
	for _, name := range artifacts[:overflow] {
		p := filepath.Join(dir, name)
		if err := os.Remove(p); err != nil {
			return pruned, fmt.Errorf("backup: remove %s: %w", p, err)
		}
		pruned = append(pruned, p)
	}
	return pruned, nil
}

// ReadArtifact reads a Tier 0 tarball back into its component
// bytes plus the parsed manifest. It is the inverse of writeArtifact
// for the parts that matter to restore + tests: state.json,
// audit.log (may be empty), MANIFEST.json. acme/ + creekd.service
// are returned in `extras` keyed by archive name.
func ReadArtifact(path string) (stateJSON, auditLog []byte, m Manifest, extras map[string][]byte, err error) {
	f, oerr := os.Open(path)
	if oerr != nil {
		err = oerr
		return
	}
	defer f.Close()
	gz, gerr := gzip.NewReader(f)
	if gerr != nil {
		err = gerr
		return
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	extras = map[string][]byte{}
	for {
		hdr, terr := tr.Next()
		if errors.Is(terr, io.EOF) {
			break
		}
		if terr != nil {
			err = terr
			return
		}
		buf := bytes.Buffer{}
		if _, cerr := io.Copy(&buf, tr); cerr != nil {
			err = cerr
			return
		}
		switch hdr.Name {
		case "state.json":
			stateJSON = buf.Bytes()
		case "audit.log":
			auditLog = buf.Bytes()
		case "MANIFEST.json":
			if jerr := json.Unmarshal(buf.Bytes(), &m); jerr != nil {
				err = fmt.Errorf("backup: parse manifest from artifact: %w", jerr)
				return
			}
		default:
			extras[hdr.Name] = buf.Bytes()
		}
	}
	return
}
