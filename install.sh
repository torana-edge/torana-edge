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
ARCHIVE="${BINARY}_${ARCHIVE_VERSION}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/${RELEASE_PATH}/${ARCHIVE}"
CHECKSUM_URL="https://github.com/${REPO}/releases/${RELEASE_PATH}/checksums.txt"

echo "Installing Torana Edge $VERSION for $OS/$ARCH..."
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
curl -fsSL "$URL" -o "$TMP_DIR/$ARCHIVE"
curl -fsSL "$CHECKSUM_URL" -o "$TMP_DIR/checksums.txt"
EXPECTED="$(awk -v archive="$ARCHIVE" '$2 == archive || $2 == "*" archive {print $1; exit}' "$TMP_DIR/checksums.txt")"
if [ -z "$EXPECTED" ]; then
    echo "Error: release checksum for $ARCHIVE was not published." >&2
    exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL="$(sha256sum "$TMP_DIR/$ARCHIVE" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
    ACTUAL="$(shasum -a 256 "$TMP_DIR/$ARCHIVE" | awk '{print $1}')"
else
    echo "Error: sha256sum or shasum is required." >&2
    exit 1
fi
if [ "$ACTUAL" != "$EXPECTED" ]; then
    echo "Error: checksum verification failed for $ARCHIVE." >&2
    exit 1
fi
if tar -tzf "$TMP_DIR/$ARCHIVE" | awk '
    /^\// { bad=1 }
    /(^|\/)\.\.(\/|$)/ { bad=1 }
    END { exit bad ? 0 : 1 }
'; then
    echo "Error: archive contains an unsafe path." >&2
    exit 1
fi
tar xzf "$TMP_DIR/$ARCHIVE" -C "$TMP_DIR"
mkdir -p "$INSTALL_DIR"
install -m 0755 "$TMP_DIR/${BINARY}" "$INSTALL_DIR/${BINARY}"

echo "Torana Edge installed to $INSTALL_DIR/${BINARY}"
case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *) echo "Add $INSTALL_DIR to PATH before running Torana." ;;
esac

cat <<'NEXT'

Torana is a proxy — on its own it forwards traffic and nothing else. The
interesting behaviour lives in plugins, which are NOT installed with the
gateway. Pick the ones you want:

  torana plugin install --official          # the six maintained plugins
  torana plugin install github.com/you/your-plugins/plugins/foo

Plugins are compiled from source on your machine, never downloaded prebuilt,
and none of them run until you review and approve their capabilities in the
control plane. A Go toolchain is required to build them.

  torana serve                              # then open http://127.0.0.1:8080/_torana/

NEXT
