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
set -eu

REPO="solcreek/creekd"
VERSION="${CREEKD_VERSION:-}"
PREFIX="${CREEKD_PREFIX:-}"

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
