// Package backup implements Tier 0 (local, always-on) backups of the
// creekd state set: state.json + state.json.wal + audit.log + acme/
// + the systemd unit. Each backup is a tar.gz containing the source
// files plus a signed MANIFEST.json. The manifest carries an ed25519
// signature over its own contents, a rolled-up ContentHash =
// sha256(state || wal || audit) for a quick "did anything change"
// check, and a Files map of per-archive-member sha256 digests so
// forensic restore can identify which specific member was corrupted.
// Restore can therefore prove both that the tarball was produced by
// this host (signature) and that the bytes inside survived transport
// (ContentHash + Files).
package backup
