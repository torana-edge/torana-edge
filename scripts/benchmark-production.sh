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
profile=${TORANA_BENCH_PROFILE:-provider}
case "$profile" in
	provider)
		default_first_byte=100ms
		default_event_delay=10ms
		default_events=100
		default_response_bytes=4096
		default_payload_bytes="4096"
		default_nonstream_concurrency="1 8 32"
		default_stream_concurrency="1 8"
		default_run_stream=1
		;;
	saturation)
		default_first_byte=0ms
		default_event_delay=0ms
		default_events=100
		default_response_bytes=4096
		default_payload_bytes="1024 16384 131072"
		default_nonstream_concurrency="1 8 32 128"
		default_stream_concurrency="1 8"
		default_run_stream=0
		;;
	large)
		# Keep near-limit bodies separate from the ordinary saturation matrix.
		# The largest generated request remains below Torana's 10 MiB limit
		# after JSON framing.
		default_first_byte=0ms
		default_event_delay=0ms
		default_events=100
		default_response_bytes=4096
		default_payload_bytes="1048576 4194304 8388608"
		default_nonstream_concurrency="1 4 8"
		default_stream_concurrency="1"
		default_run_stream=0
		;;
	*)
		echo "unknown TORANA_BENCH_PROFILE: $profile (want provider, saturation, or large)" >&2
		exit 2
		;;
esac
first_byte=${TORANA_BENCH_FIRST_BYTE:-$default_first_byte}
event_delay=${TORANA_BENCH_EVENT_DELAY:-$default_event_delay}
events=${TORANA_BENCH_EVENTS:-$default_events}
response_bytes=${TORANA_BENCH_RESPONSE_BYTES:-$default_response_bytes}
payload_bytes=${TORANA_BENCH_PAYLOAD_BYTES:-$default_payload_bytes}
nonstream_concurrency=${TORANA_BENCH_NONSTREAM_CONCURRENCY:-$default_nonstream_concurrency}
stream_concurrency=${TORANA_BENCH_STREAM_CONCURRENCY:-$default_stream_concurrency}
run_stream=${TORANA_BENCH_RUN_STREAM:-$default_run_stream}
request_shape=${TORANA_BENCH_REQUEST_SHAPE:-plain}
if [ "$run_stream" != 0 ] && [ "$run_stream" != 1 ]; then
	echo "TORANA_BENCH_RUN_STREAM must be 0 or 1" >&2
	exit 2
fi
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
	-first-byte "$first_byte" -event-delay "$event_delay" -events "$events" -response-bytes "$response_bytes" \
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
"$stage/loadbench" metadata -revision "$(git rev-parse HEAD)" \
	-profile "$profile" -upstream-first-byte "$first_byte" \
	-upstream-event-delay "$event_delay" -upstream-events "$events" \
	-upstream-response-bytes "$response_bytes" -payload-bytes "$payload_bytes" \
	-nonstream-concurrency "$nonstream_concurrency" \
	-stream-concurrency "$stream_concurrency" -run-stream "$run_stream" \
	-request-shape "$request_shape" >>"$output"
run() {
	"$stage/loadbench" load -duration "$duration" -warmup "$warmup" \
		-rss-pid "$torana_pid" -clock-ticks "$clock_ticks" \
		-request-shape "$request_shape" "$@" >>"$output"
}

for payload in $payload_bytes; do
	payload_label=
	if [ "$payload_bytes" != 4096 ]; then payload_label="/p=$payload"; fi
	for concurrency in $nonstream_concurrency; do
		run -name "direct/nonstream$payload_label/c=$concurrency" \
			-target "http://127.0.0.1:$upstream_port/v1/chat/completions" \
			-concurrency "$concurrency" -payload-bytes "$payload"
		run -name "torana/nonstream$payload_label/c=$concurrency" \
			-target "http://127.0.0.1:$proxy_port/provider/bench/v1/chat/completions" \
			-concurrency "$concurrency" -payload-bytes "$payload"
	done
done

if [ "$run_stream" = 1 ]; then
	for payload in $payload_bytes; do
		payload_label=
		if [ "$payload_bytes" != 4096 ]; then payload_label="/p=$payload"; fi
		for concurrency in $stream_concurrency; do
			run -name "direct/stream$payload_label/c=$concurrency" \
				-target "http://127.0.0.1:$upstream_port/v1/chat/completions" \
				-concurrency "$concurrency" -payload-bytes "$payload" -stream -min-sse-events "$events"
			run -name "torana/stream$payload_label/c=$concurrency" \
				-target "http://127.0.0.1:$proxy_port/provider/bench/v1/chat/completions" \
				-concurrency "$concurrency" -payload-bytes "$payload" -stream -min-sse-events "$events"
		done
	done
fi

echo "wrote $output"
