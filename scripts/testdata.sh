#!/bin/sh
# testdata.sh — content-addressed WASM fixture build engine.
#
# usage: testdata.sh <dir> <stamp-file> <output> -- <build-command...>
#
# Builds <output> from <dir> ONLY when its content fingerprint changed or the
# output is missing. The fingerprint covers:
#
#   - every source file under <dir> (path + content; additions, modifications,
#     and deletions all change it),
#   - the repository root go.mod and go.sum (an SDK pin change rebuilds all
#     fixtures),
#   - the build command itself, and
#   - the Go toolchain version.
#
# Plain mtime prerequisites are deliberately NOT the decision: touching a file
# without changing it, or deleting a source, must not leave a stale wasm. The
# Makefile wires each fixture target's prerequisites (directory, *.go, root
# build-identity files) so this script re-runs when anything might have
# changed; the fingerprint decides whether an actual go build happens.
#
# The build command runs with <dir> as its working directory and must produce
# <output>.
set -eu

dir=$1
stamp=$2
out=$3
shift 3
[ "${1:-}" = "--" ] && shift
[ $# -gt 0 ] || { echo "testdata.sh: no build command" >&2; exit 2; }

root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root"

# Portable SHA-256: sha256sum is not present by default on macOS. The tool is
# chosen once; TESTDATA_HASH_TOOL forces a choice for testing the fallback.
if [ -n "${TESTDATA_HASH_TOOL:-}" ]; then
	case "$TESTDATA_HASH_TOOL" in
	sha256sum) hash_cmd() { sha256sum "$@"; } ;;
	shasum) hash_cmd() { shasum -a 256 "$@"; } ;;
	*) echo "testdata.sh: unknown TESTDATA_HASH_TOOL $TESTDATA_HASH_TOOL" >&2; exit 2 ;;
	esac
elif command -v sha256sum >/dev/null 2>&1; then
	hash_cmd() { sha256sum "$@"; }
elif command -v shasum >/dev/null 2>&1; then
	hash_cmd() { shasum -a 256 "$@"; }
else
	echo "testdata.sh: no sha256 tool (need sha256sum or shasum -a 256)" >&2
	exit 2
fi

# Relative source list (excluding the output itself and any stamps).
sources=$(cd "$dir" && find . -type f ! -name "$(basename "$out")" | sort)

fp=$({
	printf 'dir=%s\n' "$dir"
	for f in $sources; do
		printf 'f %s ' "$f"
		hash_cmd "$dir/$f" | cut -d' ' -f1
	done
	printf 'gomod '
	hash_cmd go.mod 2>/dev/null | cut -d' ' -f1 || true
	printf 'gosum '
	hash_cmd go.sum 2>/dev/null | cut -d' ' -f1 || true
	printf 'cmd %s\n' "$*"
	printf 'go '
	go version
} | sha256sum | cut -d' ' -f1)

mkdir -p "$(dirname "$stamp")"
if [ ! -f "$out" ] || [ ! -s "$stamp" ] || [ "$(cat "$stamp" 2>/dev/null || true)" != "$fp" ]; then
	echo "building $out"
	# env(1) so VAR=val prefixes (GOOS=wasip1 GOARCH=wasm …) parse correctly
	# when the command arrives as separate argv words.
	(cd "$dir" && env "$@")
	printf '%s\n' "$fp" > "$stamp"
fi
