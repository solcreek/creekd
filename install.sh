#!/usr/bin/env sh
# creekd / creekctl installer.
#
#   curl -fsSL https://raw.githubusercontent.com/solcreek/creekd/main/install.sh | sh
#
# Detects OS + arch, fetches the latest GitHub release tarball,
# verifies its SHA256 against checksums.txt, and drops creekd +
# creekctl into a sensible bin directory.
#
# Env overrides:
#   CREEKD_VERSION    pin a specific tag (default: latest)
#   CREEKD_PREFIX     install dir (default: /usr/local/bin if root,
#                     else $HOME/.local/bin)
#   CREEKD_VERIFY_COSIGN
#                     "1" → verify cosign keyless signature on
#                     checksums.txt against the expected signer
#                     identity (release.yml in this repo). Defaults
#                     to soft-attempt: verify if cosign is on PATH,
#                     skip otherwise with a WARN. Set to "1" to
#                     hard-require — missing cosign aborts the
#                     install with a clear error.
set -eu

REPO="solcreek/creekd"
VERSION="${CREEKD_VERSION:-}"
PREFIX="${CREEKD_PREFIX:-}"
VERIFY_COSIGN="${CREEKD_VERIFY_COSIGN:-}"
# OIDC subject identity that signs every release via
# .github/workflows/release.yml. Pinned here so a fork or attacker
# cannot substitute a different release pipeline and have their sig
# pass verification — the expected identity is fixed at install
# time. Updated only when this script's REPO changes.
COSIGN_IDENTITY_REGEX="^https://github.com/${REPO}/.github/workflows/release.yml@refs/tags/v.*$"
COSIGN_OIDC_ISSUER="https://token.actions.githubusercontent.com"

# --- helpers ------------------------------------------------------

log()  { printf '%s\n' "$*" >&2; }
err()  { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

need() {
    command -v "$1" >/dev/null 2>&1 || err "$1 is required but not installed"
}

need curl
need tar
need uname

# --- detect OS / arch --------------------------------------------

uname_s="$(uname -s)"
case "$uname_s" in
    Linux)  os=linux ;;
    Darwin) os=darwin ;;
    *)      err "unsupported OS: $uname_s (creekd supports linux + darwin)" ;;
esac

uname_m="$(uname -m)"
case "$uname_m" in
    x86_64|amd64)   arch=amd64 ;;
    arm64|aarch64)  arch=arm64 ;;
    *)              err "unsupported arch: $uname_m (need amd64 or arm64)" ;;
esac

# --- resolve version ---------------------------------------------

if [ -z "$VERSION" ]; then
    log "==> resolving latest release tag"
    # The redirect target ends in /tag/v0.x.y. Extract via parameter
    # expansion — no jq / sed deps.
    final_url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
        "https://github.com/${REPO}/releases/latest")"
    VERSION="${final_url##*/}"
    case "$VERSION" in
        v*) : ;;
        *)  err "could not parse latest release tag from $final_url" ;;
    esac
fi
log "==> installing creekd $VERSION ($os/$arch)"

# --- pick install dir --------------------------------------------

if [ -z "$PREFIX" ]; then
    if [ "$(id -u)" -eq 0 ]; then
        PREFIX=/usr/local/bin
    else
        PREFIX="$HOME/.local/bin"
        mkdir -p "$PREFIX"
        case ":$PATH:" in
            *":$PREFIX:"*) : ;;
            *) log "note: $PREFIX is not on PATH — add it to your shell rc" ;;
        esac
    fi
fi
[ -d "$PREFIX" ] || err "install dir $PREFIX does not exist"
[ -w "$PREFIX" ] || err "install dir $PREFIX is not writable"

# --- download tarball + checksums --------------------------------

# The goreleaser archive name_template is:
#   creekd_<version-without-v>_<os>_<arch>.tar.gz
ver_nov="${VERSION#v}"
tar_name="creekd_${ver_nov}_${os}_${arch}.tar.gz"
tar_url="https://github.com/${REPO}/releases/download/${VERSION}/${tar_name}"
sha_url="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

log "==> downloading $tar_name"
curl -fsSL -o "$tmp/$tar_name" "$tar_url" \
    || err "download failed: $tar_url"

log "==> downloading checksums.txt"
curl -fsSL -o "$tmp/checksums.txt" "$sha_url" \
    || err "download failed: $sha_url"

# Cosign signature + Fulcio cert sit alongside checksums.txt in the
# release. Pull them speculatively — verification below only fires
# if both cosign and the assets are present.
sig_url="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt.sig"
crt_url="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt.pem"
curl -fsSL -o "$tmp/checksums.txt.sig" "$sig_url" 2>/dev/null || true
curl -fsSL -o "$tmp/checksums.txt.pem" "$crt_url" 2>/dev/null || true

# --- verify cosign signature -------------------------------------

if command -v cosign >/dev/null 2>&1 \
   && [ -s "$tmp/checksums.txt.sig" ] \
   && [ -s "$tmp/checksums.txt.pem" ]; then
    log "==> verifying cosign signature on checksums.txt"
    if cosign verify-blob \
        --certificate-identity-regexp "$COSIGN_IDENTITY_REGEX" \
        --certificate-oidc-issuer "$COSIGN_OIDC_ISSUER" \
        --certificate "$tmp/checksums.txt.pem" \
        --signature "$tmp/checksums.txt.sig" \
        "$tmp/checksums.txt" >/dev/null 2>&1; then
        log "==> cosign verified ($COSIGN_OIDC_ISSUER)"
    else
        err "cosign verification failed — checksums.txt signature did not match the expected release-pipeline identity"
    fi
elif [ "$VERIFY_COSIGN" = "1" ]; then
    err "CREEKD_VERIFY_COSIGN=1 set but cosign or signature assets unavailable — aborting"
else
    log "note: skipping cosign verification (cosign or sig assets not present)"
    log "      install cosign + re-run with CREEKD_VERIFY_COSIGN=1 to hard-require"
fi

# --- verify checksum ---------------------------------------------

if command -v sha256sum >/dev/null 2>&1; then
    SHA="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
    SHA="shasum -a 256"
else
    err "neither sha256sum nor shasum found — cannot verify"
fi

expected="$(grep " $tar_name\$" "$tmp/checksums.txt" | awk '{print $1}')"
[ -n "$expected" ] || err "no checksum for $tar_name in checksums.txt"
actual="$( $SHA "$tmp/$tar_name" | awk '{print $1}' )"
if [ "$expected" != "$actual" ]; then
    err "checksum mismatch for $tar_name (expected $expected, got $actual)"
fi
log "==> checksum verified"

# --- extract + install -------------------------------------------

tar -xzf "$tmp/$tar_name" -C "$tmp"
install -m 0755 "$tmp/creekd"   "$PREFIX/creekd"
install -m 0755 "$tmp/creekctl" "$PREFIX/creekctl"

log "==> installed:"
log "    $PREFIX/creekd ($(${PREFIX}/creekd --version 2>/dev/null || echo unknown))"
log "    $PREFIX/creekctl ($(${PREFIX}/creekctl version 2>/dev/null || echo unknown))"
log ""
log "next: see https://github.com/${REPO}#quickstart"
