#!/usr/bin/env bash
# Torana Edge one-line install script
# Usage: curl -fsSL https://raw.githubusercontent.com/torana-edge/torana-edge/main/install.sh | bash
set -euo pipefail

VERSION="${INSTALL_VERSION:-latest}"
REPO="torana-edge/torana-edge"
BINARY="torana"
INSTALL_DIR="${INSTALL_DIR:-${HOME}/.local/bin}"

need_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "Error: '$1' is required but not installed." >&2
        exit 1
    fi
}

need_cmd curl
need_cmd tar

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH="amd64"
case "$(uname -m)" in
    arm64|aarch64) ARCH="arm64" ;;
esac

if [ "$VERSION" = "latest" ]; then
    RELEASE_PATH="latest/download"
    RELEASE_TAG="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest" | awk -F/ '{print $NF}')"
else
    RELEASE_PATH="download/${VERSION}"
    RELEASE_TAG="$VERSION"
fi
ARCHIVE_VERSION="${RELEASE_TAG#v}"
URL="https://github.com/${REPO}/releases/${RELEASE_PATH}/${BINARY}_${ARCHIVE_VERSION}_${OS}_${ARCH}.tar.gz"

echo "Installing Torana Edge $VERSION for $OS/$ARCH..."
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
curl -fsSL "$URL" -o "$TMP_DIR/${BINARY}.tar.gz"
tar xzf "$TMP_DIR/${BINARY}.tar.gz" -C "$TMP_DIR"
mkdir -p "$INSTALL_DIR"
install -m 0755 "$TMP_DIR/${BINARY}" "$INSTALL_DIR/${BINARY}"

echo "Torana Edge installed to $INSTALL_DIR/${BINARY}"
case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *) echo "Add $INSTALL_DIR to PATH before running Torana." ;;
esac
echo "Run: torana"
