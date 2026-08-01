#!/bin/sh
# fixtures-for-pkg.sh — the fixture set one package's tests require.
#
# usage: fixtures-for-pkg.sh <pkg-path>     (accepted: internal/<name> or ./internal/<name>)
#
# Prints the bare fixture names (e.g. test-observer) whose built wasm that
# package's tests reference literally. The inventory test in test/fixturebuild
# pins this mapping against the actual references, so a newly referenced guest
# cannot be forgotten.
#
# internal/wasm is special-cased: its all-fixture ABI inventory legitimately
# requires every v2 fixture.
set -eu

pkg=$1
case "$pkg" in
./*) pkg=${pkg#./} ;;
esac

case "$pkg" in
internal/wasm)
	sed -n 's/^TESTDATA_DIRS := //p' Makefile | tr ' ' '\n' | grep '^examples/plugins/' | sed 's#examples/plugins/##' | sort -u
	;;
internal/*)
	# Literal reference forms in tests: full paths (examples/plugins/<name>)
	# and the fixturesDir+"/<name>" helper form. The inventory test in
	# test/fixturebuild pins this mapping against the same matcher, so a newly
	# referenced guest cannot be forgotten.
	{
		grep -rhoE 'examples/plugins/[a-z0-9-]+' "$pkg" --include='*_test.go' 2>/dev/null | sed 's#examples/plugins/##'
		grep -rhoE 'fixturesDir\+"/[a-z0-9-]+' "$pkg" --include='*_test.go' 2>/dev/null | sed 's#fixturesDir+"/##'
	} | sort -u
	;;
*)
	echo "fixtures-for-pkg.sh: PKG must be ./internal/... (got $pkg)" >&2
	exit 1
	;;
esac
