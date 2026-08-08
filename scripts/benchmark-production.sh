#!/bin/sh
set -eu

# Production-shaped local benchmark: a built Torana process, a separate
# controlled upstream process, and an external concurrent client. This is not
# a capacity claim; it measures one machine with explicit upstream latency and
# stream cadence. Override ports when they are already in use.

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
duration=${TORANA_BENCH_DURATION:-10s}
warmup=${TORANA_BENCH_WARMUP:-2s}
proxy_port=${TORANA_BENCH_PORT:-18080}
upstream_port=${TORANA_BENCH_UPSTREAM_PORT:-18081}
output=${1:-"$repo_dir/benchmark-production.jsonl"}
clock_ticks=$(getconf CLK_TCK)
stage=$(mktemp -d)
upstream_pid=
torana_pid=

cleanup() {
	if [ -n "$torana_pid" ]; then kill "$torana_pid" 2>/dev/null || true; fi
	if [ -n "$upstream_pid" ]; then kill "$upstream_pid" 2>/dev/null || true; fi
	wait "$torana_pid" 2>/dev/null || true
	wait "$upstream_pid" 2>/dev/null || true
	rm -rf "$stage"
}
trap cleanup EXIT INT TERM

cd "$repo_dir"
CGO_ENABLED=0 go build -trimpath -o "$stage/torana" ./cmd/torana
go build -trimpath -o "$stage/loadbench" ./scripts/loadbench

cat >"$stage/config.json" <<EOF
{"port":$proxy_port,"providers":{"bench":{"url":"http://127.0.0.1:$upstream_port","format":"openai"}},"plugins":{"order":[]}}
EOF

"$stage/loadbench" upstream -listen "127.0.0.1:$upstream_port" \
	-first-byte 100ms -event-delay 10ms -events 100 -response-bytes 4096 \
	>"$stage/upstream.log" 2>&1 &
upstream_pid=$!

TORANA_CONFIG="$stage/config.json" TORANA_DATA_DIR="$stage/data" \
	TORANA_BIND=127.0.0.1 "$stage/torana" >"$stage/torana.log" 2>&1 &
torana_pid=$!

ready=0
for _ in $(seq 1 100); do
	if curl -fsS "http://127.0.0.1:$proxy_port/health" >/dev/null 2>&1; then
		ready=1
		break
	fi
	sleep 0.1
done
if [ "$ready" -ne 1 ]; then
	echo "Torana did not become ready" >&2
	tail -n 50 "$stage/torana.log" >&2
	exit 1
fi

: >"$output"
"$stage/loadbench" metadata -revision "$(git rev-parse HEAD)" >>"$output"
run() {
	"$stage/loadbench" load -duration "$duration" -warmup "$warmup" \
		-rss-pid "$torana_pid" -clock-ticks "$clock_ticks" "$@" >>"$output"
}

for concurrency in 1 8 32; do
	run -name "direct/nonstream/c=$concurrency" \
		-target "http://127.0.0.1:$upstream_port/v1/chat/completions" \
		-concurrency "$concurrency" -payload-bytes 4096
	run -name "torana/nonstream/c=$concurrency" \
		-target "http://127.0.0.1:$proxy_port/provider/bench/v1/chat/completions" \
		-concurrency "$concurrency" -payload-bytes 4096
done

for concurrency in 1 8; do
	run -name "direct/stream/c=$concurrency" \
		-target "http://127.0.0.1:$upstream_port/v1/chat/completions" \
		-concurrency "$concurrency" -payload-bytes 4096 -stream -min-sse-events 100
	run -name "torana/stream/c=$concurrency" \
		-target "http://127.0.0.1:$proxy_port/provider/bench/v1/chat/completions" \
		-concurrency "$concurrency" -payload-bytes 4096 -stream -min-sse-events 100
done

echo "wrote $output"
