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
#                     to soft-attempt: verify if cosign is on PATH
#                     AND the signature assets exist on the release.
#                     A 404 on the .sig/.pem is treated as legit (an
#                     older release predates cosign signing); any
#                     other fetch failure logs WARN and skips. Set
#                     to "1" to hard-require — missing cosign or
#                     unfetchable sig assets abort with a clear
#                     error.
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
#
# Dots in literal positions are escaped so a path like
# "release-yml" (or any single char in place of the dots) can't
# squeak past the matcher. Tag suffix is tightened to semver-ish so
# branch refs can never satisfy the identity.
COSIGN_IDENTITY_REGEX="^https://github\.com/${REPO}/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+.*\$"
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
# release. Fetch each one and capture the HTTP status so we can tell
# "this release predates cosign signing" (404 → silent skip is fine)
# from "transient network failure or proxy interference" (other →
# loud WARN so the operator doesn't silently lose verification).
sig_url="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt.sig"
crt_url="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt.pem"

# fetch_asset URL OUT_PATH -> echoes "ok", "absent", or "fetch_failed:<code>"
fetch_asset() {
    _url="$1"; _out="$2"
    _code="$(curl -fsSL -o "$_out" -w '%{http_code}' "$_url" 2>/dev/null || true)"
    if [ -s "$_out" ]; then
        echo ok
    elif [ "$_code" = "404" ]; then
        echo absent
    else
        echo "fetch_failed:${_code:-network}"
    fi
}

sig_status="$(fetch_asset "$sig_url" "$tmp/checksums.txt.sig")"
crt_status="$(fetch_asset "$crt_url" "$tmp/checksums.txt.pem")"

# --- verify cosign signature -------------------------------------

cosign_present=0
command -v cosign >/dev/null 2>&1 && cosign_present=1

# Flatten the "transient fetch failure" predicate to avoid POSIX sh
# precedence subtleties when mixing && and || in one if-clause.
sig_unreachable=0
crt_unreachable=0
[ "$sig_status" != ok ] && [ "$sig_status" != absent ] && sig_unreachable=1
[ "$crt_status" != ok ] && [ "$crt_status" != absent ] && crt_unreachable=1

if [ "$cosign_present" = 1 ] && [ "$sig_status" = ok ] && [ "$crt_status" = ok ]; then
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
    err "CREEKD_VERIFY_COSIGN=1 set but verification could not run (cosign=${cosign_present} sig=${sig_status} cert=${crt_status})"
elif [ "$cosign_present" = 1 ] && { [ "$sig_unreachable" = 1 ] || [ "$crt_unreachable" = 1 ]; }; then
    log "WARN: cosign is installed but signature assets could not be fetched (sig=${sig_status} cert=${crt_status})"
    log "      verification SKIPPED — re-run with CREEKD_VERIFY_COSIGN=1 to abort instead"
else
    log "note: skipping cosign verification (cosign=${cosign_present} sig=${sig_status} cert=${crt_status})"
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
