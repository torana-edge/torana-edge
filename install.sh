#!/usr/bin/env bash
# Torana Edge one-line install script
# Usage: curl -fsSL https://raw.githubusercontent.com/torana-edge/torana-edge/main/install.sh | bash
set -euo pipefail

VERSION="${INSTALL_VERSION:-latest}"
REPO="torana-edge/torana-edge"
BINARY="torana"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

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
    URL="https://github.com/${REPO}/releases/latest/download/${BINARY}-${OS}-${ARCH}.tar.gz"
else
    URL="https://github.com/${REPO}/releases/download/${VERSION}/${BINARY}-${OS}-${ARCH}.tar.gz"
fi

echo "Installing Torana Edge $VERSION for $OS/$ARCH..."
curl -fsSL "$URL" -o "/tmp/${BINARY}.tar.gz"
tar xzf "/tmp/${BINARY}.tar.gz" -C /tmp
sudo mv "/tmp/${BINARY}" "$INSTALL_DIR/${BINARY}"
rm -f "/tmp/${BINARY}.tar.gz"

echo "Torana Edge installed to $INSTALL_DIR/${BINARY}"
echo "Run: torana"
