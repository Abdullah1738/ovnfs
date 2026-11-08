#!/bin/sh
# ovnfs installer for Linux and macOS
# Usage: curl -fsSL https://stencil.so/ovnfs.sh | sh
#        curl -fsSL https://stencil.so/ovnfs.sh | sh -s -- --version v0.2.3
#
# Options:
#   --version <tag>  install this release instead of the latest
#
# Downloads the release binary from Stencil's build service ($OVNFS_BUILD_URL,
# default https://build.stencil.so), checks it against the service's SHA-256,
# and installs it as $OVNFS_INSTALL_DIR/ovnfs (default ~/.local/bin/ovnfs).
# `ovnfs update` later replaces it with the latest release.
set -e

INSTALL_DIR="${OVNFS_INSTALL_DIR:-$HOME/.local/bin}"
BUILD_URL="${OVNFS_BUILD_URL:-https://build.stencil.so}"
while [ "${BUILD_URL%/}" != "$BUILD_URL" ]; do
    BUILD_URL="${BUILD_URL%/}"
done

VERSION=""
while [ $# -gt 0 ]; do
    case "$1" in
        --version)
            if [ -z "$2" ]; then
                echo "Missing value for --version"
                exit 1
            fi
            VERSION="$2"
            shift 2
            ;;
        --version=*)
            VERSION="${1#*=}"
            shift
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

case "$(uname -s)" in
    Linux)  PLATFORM="linux" ;;
    Darwin) PLATFORM="macos" ;;
    *)      echo "Unsupported OS: $(uname -s); on Windows, use https://stencil.so/ovnfs.ps1"; exit 1 ;;
esac

# On macOS, hw.optional.arm64 stays correct inside a Rosetta shell, where
# uname -m reports the translated x86_64.
if [ "$PLATFORM" = "macos" ] && [ "$(sysctl -in hw.optional.arm64 2>/dev/null || /usr/sbin/sysctl -in hw.optional.arm64 2>/dev/null)" = "1" ]; then
    ARCH="arm64"
else
    case "$(uname -m)" in
        x86_64|amd64)  ARCH="x86_64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *)             echo "Unsupported architecture: $(uname -m)"; exit 1 ;;
    esac
fi
TARGET="$PLATFORM-$ARCH"

# Hex sha256 of file "$1".
sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d ' ' -f 1
    else
        shasum -a 256 "$1" | cut -d ' ' -f 1
    fi
}
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
    echo "sha256sum or shasum is required to verify the download"
    exit 1
fi

# The string value of field "$1" in the one-line JSON on stdin (last
# occurrence). Escaped quotes inside JSON strings never end in `"<key>"`, so
# string contents cannot fake a key.
json_string() {
    sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p"
}

# The members of the answer's top-level `file` object (the requested target's
# file). `build.files[]` lists every target's file under the `files` key, so
# its sha256s never match here.
json_file_object() {
    sed -n 's/.*"file"[[:space:]]*:[[:space:]]*{\([^}]*\)}.*/\1/p'
}

if [ -n "$VERSION" ]; then
    if ! printf '%s\n' "$VERSION" | grep -Eq '^v?[0-9]+\.[0-9]+\.[0-9]+$'; then
        echo "Not a release tag: $VERSION"
        exit 1
    fi
    echo "Fetching ovnfs v${VERSION#v}..."
    API_URL="$BUILD_URL/api/products/ovnfs/versions/${VERSION#v}/$TARGET"
else
    echo "Fetching the latest release..."
    API_URL="$BUILD_URL/api/products/ovnfs/latest/$TARGET?channel=stable"
fi
if ! ANSWER="$(curl -fsSL --connect-timeout 10 --max-time 60 --retry 3 "$API_URL")"; then
    echo "Failed to find an ovnfs build for $TARGET at $API_URL"
    exit 1
fi
ANSWER="$(printf '%s' "$ANSWER" | tr -d '\r\n')"

# build.version is the answer's only "version".
VERSION="$(printf '%s\n' "$ANSWER" | json_string version)"
EXPECTED="$(printf '%s\n' "$ANSWER" | json_file_object | json_string sha256)"
# Go's JSON encoder writes `&` in the presigned URL as \u0026.
DOWNLOAD_URL="$(printf '%s\n' "$ANSWER" | json_string download | sed 's/\\u0026/\&/g')"
if [ -z "$VERSION" ] || [ -z "$DOWNLOAD_URL" ] || ! printf '%s\n' "$EXPECTED" | grep -Eq '^[0-9a-f]{64}$'; then
    echo "Unexpected answer from $API_URL"
    exit 1
fi

mkdir -p "$INSTALL_DIR"
# Download beside the destination and rename it into place only once it
# checks out, so a bad download never replaces a working ovnfs, nor does a
# half-written one replace the binary a daemon runs.
STAGED="$INSTALL_DIR/.ovnfs.download.$$"
trap 'rm -f "$STAGED"' EXIT
echo "Downloading ovnfs v$VERSION ($TARGET)..."
curl -fsSL --connect-timeout 10 --speed-limit 1024 --speed-time 30 --retry 3 "$DOWNLOAD_URL" -o "$STAGED"
ACTUAL="$(sha256_of "$STAGED")"
if [ "$ACTUAL" != "$EXPECTED" ]; then
    echo "Checksum mismatch for ovnfs v$VERSION: expected $EXPECTED, got $ACTUAL"
    exit 1
fi
chmod 755 "$STAGED"
if ! OUTPUT="$("$STAGED" version 2>&1)"; then
    echo "ovnfs v$VERSION does not run here:"
    echo "$OUTPUT" | sed 's/^/    /'
    exit 1
fi
UPGRADE=""
[ -e "$INSTALL_DIR/ovnfs" ] && UPGRADE=1
mv -f "$STAGED" "$INSTALL_DIR/ovnfs"

echo ""
echo "✓ Installed ovnfs v$VERSION to $INSTALL_DIR/ovnfs"
case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) echo "Add $INSTALL_DIR to your PATH to run ovnfs" ;;
esac
if [ -n "$UPGRADE" ]; then
    echo "A running ovnfs daemon keeps the old version until 'ovnfs restart'"
fi
if ! command -v git >/dev/null 2>&1; then
    echo "ovnfs runs git, which is not on your PATH"
fi
if [ "$PLATFORM" = "linux" ] && ! command -v fusermount3 >/dev/null 2>&1; then
    echo "ovnfs mounts with FUSE on Linux and needs fusermount3 (the fuse3 package)"
fi
