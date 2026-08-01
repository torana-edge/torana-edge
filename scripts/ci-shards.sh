#!/bin/sh
# ci-shards.sh — the SINGLE source of truth for the CI race-shard partition.
#
# The workflow's test jobs and its exhaustiveness check BOTH consume this
# helper, so the selection logic cannot drift between the matrix and the
# proof.
#
# usage:
#   ci-shards.sh <wasm|plugin|proxy|remainder>  — that shard's package list
#   ci-shards.sh fixtures <shard>               — the wasm targets the shard must build
#   ci-shards.sh check                          — verify the partition against `GOWORK=off go list ./...`
#   ci-shards.sh check-synthetic                — same, reading the package list from stdin (tests)
set -eu

REPO_PREFIX='github.com/torana-edge/torana-edge'
SHARD_MOD='^github.com/torana-edge/torana-edge/internal/(wasm|plugin|proxy)(/|$)'
WASM_MOD='^github.com/torana-edge/torana-edge/internal/wasm(/|$)'
PLUGIN_MOD='^github.com/torana-edge/torana-edge/internal/plugin(/|$)'
PROXY_MOD='^github.com/torana-edge/torana-edge/internal/proxy(/|$)'

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
self=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/ci-shards.sh

case "${1:-}" in
wasm | plugin | proxy | remainder)
	mod=$SHARD_MOD
	[ "$1" = wasm ] && mod=$WASM_MOD
	[ "$1" = plugin ] && mod=$PLUGIN_MOD
	[ "$1" = proxy ] && mod=$PROXY_MOD
	if [ "$1" = remainder ]; then
		(cd "$root" && GOWORK=off go list ./...) | grep -vE "$mod"
	else
		(cd "$root" && GOWORK=off go list ./...) | grep -E "$mod"
	fi
	;;
fixtures)
	[ $# -eq 2 ] || { echo "usage: ci-shards.sh fixtures <shard>" >&2; exit 2; }
	shard=$2
	if [ "$shard" = remainder ]; then
		# Union of the remainder packages' fixture sets (usually empty).
		packages=$("$self" remainder)
	else
		packages=$("$self" "$shard")
	fi
	for p in $packages; do
		rel=${p#"$REPO_PREFIX"}
		rel=${rel#/}
		[ -n "$rel" ] || continue # the module root itself
		(cd "$root" && go run ./scripts/fixtures-for-pkg.go "$rel")
	done | sort -u | sed 's#^#examples/plugins/#' | sed 's#$#/plugin.wasm#'
	;;
check | check-synthetic)
	if [ "$1" = check ]; then
		input=$(cd "$root" && GOWORK=off go list ./... | sort)
	else
		input=$(cat | sort)
	fi
	# 1. The input itself must have no duplicate lines.
	dupes=$(printf '%s\n' "$input" | uniq -d)
	[ -z "$dupes" ] || { echo "duplicate packages in the inventory:" >&2; printf '%s\n' "$dupes" >&2; exit 1; }
	# 2. The four shard lists, computed from the SAME input.
	wasm=$(printf '%s\n' "$input" | grep -E "$WASM_MOD" || true)
	plugin=$(printf '%s\n' "$input" | grep -E "$PLUGIN_MOD" || true)
	proxy=$(printf '%s\n' "$input" | grep -E "$PROXY_MOD" || true)
	remainder=$(printf '%s\n' "$input" | grep -vE "$SHARD_MOD" || true)
	all_lists=$(printf '%s\n%s\n%s\n%s\n' "$wasm" "$plugin" "$proxy" "$remainder")
	# 3. No package may appear in more than one shard (overlap corrupts the
	#    partition into duplicates).
	overlaps=$(printf '%s\n' "$all_lists" | sort | uniq -d)
	[ -z "$overlaps" ] || { echo "packages matched by more than one shard:" >&2; printf '%s\n' "$overlaps" >&2; exit 1; }
	# 4. The sorted union must equal the inventory exactly (omissions), and
	#    the total cardinality must match (dedup must not hide a duplicate).
	union=$(printf '%s\n' "$all_lists" | sort -u)
	[ "$union" = "$input" ] || {
		echo "shard union does not equal the package inventory:" >&2
		comm -3 <(printf '%s\n' "$input") <(printf '%s\n' "$union") >&2
		exit 1
	}
	total=$(printf '%s\n' "$all_lists" | grep -c . || true)
	expected=$(printf '%s\n' "$input" | grep -c . || true)
	[ "$total" = "$expected" ] || {
		echo "shard cardinality $total != inventory $expected (a duplicate was hidden by dedup)" >&2
		exit 1
	}
	echo "shard partition verified: $expected packages, no duplicates, no omissions"
	;;
*)
	echo "usage: ci-shards.sh <wasm|plugin|proxy|remainder|fixtures <shard>|check|check-synthetic>" >&2
	exit 2
	;;
esac
