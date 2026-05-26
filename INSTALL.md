# Installing creekd

## Quick install (Linux / macOS)

```sh
curl -fsSL https://raw.githubusercontent.com/solcreek/creekd/main/install.sh | sh
```

This downloads the latest tagged release, verifies its SHA256
against the published `checksums.txt`, and drops `creekd` +
`creekctl` into `/usr/local/bin` (root) or `~/.local/bin` (user).

For paranoid installs, also verify the cosign keyless signature:

```sh
# Requires cosign on PATH (https://docs.sigstore.dev/cosign/installation).
# The env var has to ride with `sh`, not `curl` — otherwise it stays
# in curl's environment and install.sh never sees it.
curl -fsSL https://raw.githubusercontent.com/solcreek/creekd/main/install.sh \
  | CREEKD_VERIFY_COSIGN=1 sh
```

The installer verifies the cosign signature on `checksums.txt`
was produced by THIS repo's `release.yml` on a `v*` tag (identity
pinned). A fork or hijacked pipeline cannot satisfy verification.

## Environment overrides

| Variable               | Default                                    | Purpose                                                                                  |
|------------------------|--------------------------------------------|------------------------------------------------------------------------------------------|
| `CREEKD_VERSION`       | latest release tag                         | Pin a specific release (e.g. `v0.0.5`).                                                  |
| `CREEKD_PREFIX`        | `/usr/local/bin` if root, else `~/.local/bin` | Install directory.                                                                       |
| `CREEKD_VERIFY_COSIGN` | unset (soft-attempt)                       | `1` → hard-require cosign verification. Missing cosign aborts the install.               |

## Manual install (offline / air-gapped)

1. Download the release tarball + `checksums.txt` (+ optionally
   `.sig` and `.pem`) from the [GitHub Releases page][releases]
   on a machine with internet, then transfer to the target host.
2. Verify SHA256 (Linux uses `sha256sum`; macOS ships `shasum`):
   ```sh
   # Linux
   sha256sum --check checksums.txt --ignore-missing
   # macOS
   shasum -a 256 -c checksums.txt --ignore-missing
   ```
3. (Optional) verify cosign:
   ```sh
   cosign verify-blob \
     --certificate-identity-regexp '^https://github.com/solcreek/creekd/\.github/workflows/release\.yml@refs/tags/v.*$' \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com \
     --certificate checksums.txt.pem \
     --signature checksums.txt.sig \
     checksums.txt
   ```
4. Extract and copy `creekd` + `creekctl` to a directory on
   `PATH`.

[releases]: https://github.com/solcreek/creekd/releases

## Post-install (systemd, Linux)

The installer drops binaries only — it does NOT configure
systemd. To run `creekd` as a system service:

1. Create the `creekd` user and state directories:
   ```sh
   sudo useradd --system --no-create-home --shell /usr/sbin/nologin creekd
   sudo mkdir -p /var/lib/creekd /var/log/creekd
   sudo chown -R creekd:creekd /var/lib/creekd /var/log/creekd
   ```
2. Install the hardened systemd unit shipped in the source repo
   at [`init/creekd.service`](init/creekd.service):
   ```sh
   sudo cp init/creekd.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now creekd
   ```
3. Verify the unit's hardening is intact on disk:
   ```sh
   creekctl hardening-check /etc/systemd/system/creekd.service
   ```
   Output: `hardening clean (22 directives validated)` on
   success.

## Verification

```sh
creekd --version
creekctl version
```

## Uninstall

```sh
sudo systemctl disable --now creekd
sudo rm /etc/systemd/system/creekd.service
sudo rm /usr/local/bin/creekd /usr/local/bin/creekctl
# Optional: wipe state (irreversible)
sudo rm -rf /var/lib/creekd /var/log/creekd
sudo userdel creekd
```

## Building from source

Requires Go 1.22+:

```sh
git clone https://github.com/solcreek/creekd
cd creekd
make build
sudo cp creekd creekctl /usr/local/bin/
```

`make build` produces a CGO-free static binary suitable for any
glibc / musl Linux. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for
the development workflow.
