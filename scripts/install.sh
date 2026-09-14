#!/bin/sh
# Installs an official Torana Edge release. Website activation waits for the
# first published Edge tag; see docs/RELEASE_INSTALLERS.md.
set -eu

fail() { printf 'torana installer: %s\n' "$*" >&2; exit 1; }
usage() {
    cat <<'USAGE'
Usage: sh install.sh [--version VERSION] [--install-dir DIRECTORY]

Installs the latest published release by default. VERSION accepts v1.2.3 or
1.2.3, including prereleases. DIRECTORY must be absolute; the default is
$HOME/.local/bin. TORANA_VERSION and TORANA_INSTALL_DIR set the same defaults.
No sudo, shell profile changes, service startup, or plugin installation.
USAGE
}

version=${TORANA_VERSION:-}
install_dir=${TORANA_INSTALL_DIR:-}
while [ "$#" -gt 0 ]; do
    case "$1" in
        --version|--install-dir)
            option=$1
            [ "$#" -ge 2 ] && [ -n "$2" ] || fail "$option requires a value"
            case "$option" in
                --version) version=$2 ;;
                --install-dir) install_dir=$2 ;;
            esac
            shift 2
            ;;
        -h|--help) usage; exit 0 ;;
        *) fail "unknown argument: $1 (see --help)" ;;
    esac
done

if [ -z "$install_dir" ]; then
    [ -n "${HOME:-}" ] || fail 'HOME is unset; use --install-dir'
    install_dir=$HOME/.local/bin
fi
case "$install_dir" in
    /*) ;;
    *) fail 'install directory must be an absolute path' ;;
esac

for dependency in curl uname mktemp tar awk grep mkdir cp chmod mv rm; do
    command -v "$dependency" >/dev/null 2>&1 || fail "required command is missing: $dependency"
done
if command -v sha256sum >/dev/null 2>&1; then
    hash_command=sha256sum
elif command -v shasum >/dev/null 2>&1; then
    hash_command=shasum
else
    fail 'SHA-256 verification requires sha256sum or shasum'
fi

case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *) fail 'supported operating systems are macOS and Linux; use install.ps1 on Windows' ;;
esac
case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) fail 'supported architectures are x86_64/amd64 and arm64/aarch64' ;;
esac

release_url=https://github.com/torana-edge/torana-edge/releases
curl_https() {
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --fail --silent --show-error --location --retry 2 \
        --connect-timeout 15 --max-time 300 "$@"
}
if [ -z "$version" ]; then
    if ! resolved=$(curl_https --output /dev/null --write-out '%{url_effective}' "$release_url/latest"); then
        fail 'no published release could be resolved; check GitHub releases or build from source (before the first Edge release, no binaries exist)'
    fi
    case "$resolved" in
        "$release_url/tag/"*) version=${resolved##*/} ;;
        *) fail 'latest release did not resolve to an official release tag' ;;
    esac
fi
version=${version#v}
printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?(\+[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$' \
    || fail 'version must be a release version such as v1.2.3 or v1.2.3-rc.1'

archive=torana_${version}_${os}_${arch}.tar.gz
tmp_dir=
stage_dir=
cleanup() {
    [ -z "$stage_dir" ] || rm -rf "$stage_dir"
    [ -z "$tmp_dir" ] || rm -rf "$tmp_dir"
    :
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/torana-install.XXXXXXXX")
printf 'Downloading Torana %s for %s/%s...\n' "$version" "$os" "$arch"
for asset in "$archive" checksums.txt; do
    curl_https --output "$tmp_dir/$asset" "$release_url/download/v$version/$asset" \
        || fail "could not download $asset for v$version; the release or asset may not be published yet"
done

# Match the entire filename, demand exactly one valid digest, and never accept
# an empty, malformed, missing, or duplicate checksum entry.
if ! expected=$(awk -v file="$archive" '
    $2 == file || $2 == "*" file {
        count++
        if (NF != 2 || length($1) != 64 || $1 ~ /[^0-9a-fA-F]/) bad = 1
        digest = tolower($1)
    }
    END { if (count != 1 || bad) exit 1; print digest }
' "$tmp_dir/checksums.txt"); then
    fail "checksums.txt must contain exactly one valid SHA-256 entry for $archive"
fi
if [ "$hash_command" = sha256sum ]; then
    hash_output=$(sha256sum "$tmp_dir/$archive") || fail 'SHA-256 calculation failed; nothing was installed'
else
    hash_output=$(shasum -a 256 "$tmp_dir/$archive") || fail 'SHA-256 calculation failed; nothing was installed'
fi
actual=$(printf '%s\n' "$hash_output" | awk '{print tolower($1)}')
[ "$expected" = "$actual" ] || fail 'SHA-256 checksum mismatch; nothing was installed'

# Stream only the root binary into a file we create. Archive paths and metadata
# never determine filesystem writes (including symlinks and ../ entries).
tar -tzf "$tmp_dir/$archive" >"$tmp_dir/members" || fail 'invalid release archive'
awk '$0 == "torana" { count++ } END { exit count != 1 }' "$tmp_dir/members" \
    || fail 'release archive must contain exactly one root torana binary'
tar -xOzf "$tmp_dir/$archive" torana >"$tmp_dir/torana" || fail 'could not read the torana binary'
[ -s "$tmp_dir/torana" ] || fail 'release archive contains an empty binary or link'

destination=$install_dir/torana
if [ -L "$destination" ] || { [ -e "$destination" ] && [ ! -f "$destination" ]; }; then
    fail "refusing to replace a symlink or non-regular file: $destination"
fi
mkdir -p "$install_dir" || fail "cannot create install directory: $install_dir"
stage_dir=$(mktemp -d "$install_dir/.torana-install.XXXXXXXX")
cp "$tmp_dir/torana" "$stage_dir/torana"
chmod 755 "$stage_dir/torana"
mv -f "$stage_dir/torana" "$destination" || fail "cannot replace $destination; check directory permissions"
printf 'Installed Torana %s to %s\n' "$version" "$destination"
case ":${PATH:-}:" in
    *:"$install_dir":*) printf 'Run: torana version\n' ;;
    *) printf 'Add this directory to your shell PATH, then run torana version:\n  %s\n' "$install_dir" ;;
esac
