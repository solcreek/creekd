// Package backup implements Tier 0 (local, always-on) backups of the
// creekd state set: state.json + audit.log + acme/ + the systemd
// unit. Each backup is a tar.gz containing the source files plus a
// signed MANIFEST.json. The manifest carries an ed25519 signature
// over the manifest's own contents and a contentHash =
// sha256(state.json || audit.log) over the bytes-as-backed-up, so
// restore can prove both that the tarball was produced by this
// host (signature) and that the bytes inside survived transport
// (contentHash). See product-planning/DESIGN-self-host-state.md
// §"Tier 0 — local (always on)".
package backup
