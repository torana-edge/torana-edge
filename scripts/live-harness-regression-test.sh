#!/bin/sh
set -eu

repo_dir=$(unset CDPATH; cd -- "$(dirname "$0")/.." && pwd)
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/torana-live-runner-test.XXXXXX")
trap 'rm -rf "$test_dir"' EXIT HUP INT TERM
mkdir -p "$test_dir/bin"

cat >"$test_dir/bin/torana" <<'EOF'
#!/bin/sh
set -eu
command_name=${1:-}
case "$command_name" in
  start)
    mkdir -p "$TORANA_DATA_DIR"
    if [ "${FAKE_START_SIGNAL:-0}" = 1 ]; then
      kill -TERM "$PPID"
      sleep 0.1
    fi
    [ "${FAKE_START_FAIL:-0}" = 0 ] || exit 1
    ;;
  status)
    if [ -f "$TORANA_DATA_DIR/stopped" ]; then printf '{"status":"stopped"}\n'; else printf '{"status":"running"}\n'; fi
    ;;
  stop)
    : >"$FAKE_STOP_CALLED"
    [ "${FAKE_STOP_FAIL:-0}" = 0 ] || exit 1
    : >"$TORANA_DATA_DIR/stopped"
    : >"$FAKE_MARKER"
    printf '{"status":"stopped"}\n'
    ;;
  feed)
    printf '[%s]\n' '{"status":200,"tokens_in":1,"tokens_out":1},{"status":200,"tokens_in":1,"tokens_out":1},{"status":200,"tokens_in":1,"tokens_out":1},{"status":200,"tokens_in":1,"tokens_out":1},{"status":200,"tokens_in":1,"tokens_out":1},{"status":200,"tokens_in":1,"tokens_out":1},{"status":200,"tokens_in":1,"tokens_out":1}'
    ;;
  *) exit 2 ;;
esac
EOF

cat >"$test_dir/bin/curl" <<'EOF'
#!/bin/sh
set -eu
output=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) output=$2; shift 2 ;;
    --write-out) shift 2 ;;
    --max-time|-H|--data-binary) shift 2 ;;
    --silent|--show-error|--fail) shift ;;
    http*) url=$1; shift ;;
    *) shift ;;
  esac
done
if [ -z "$output" ]; then
  [ ! -f "$FAKE_MARKER" ]
  exit $?
fi
case "$url" in
  */v1/chat/completions)
    if [ "${FAKE_UNRELATED:-0}" = 1 ]; then body='{"choices":[{"finish_reason":"stop","message":{"content":"142"}}]}'
    else body='{"choices":[{"finish_reason":"stop","message":{"content":"42"}}]}' ; fi
    ;;
  */v1/responses)
    if [ "${FAKE_INCOMPLETE:-0}" = 1 ]; then body='{"object":"response","status":"incomplete","output":[]}'
    else body='{"object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"42"}]}]}' ; fi
    ;;
  */v1/messages) body='{"type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"42"}]}' ;;
  *v1internal:generateContent) body='{"response":{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"42"}]}}]}}' ;;
  *generateContent) body='{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"42"}]}}]}' ;;
  *) body='{"error":{"message":"unexpected fixture route"}}' ;;
esac
printf '%s' "$body" >"$output"
printf '200'
EOF
chmod +x "$test_dir/bin/torana" "$test_dir/bin/curl"

run_case() {
  case_dir=$test_dir/$1
  mkdir -p "$case_dir"
  shift
  set +e
  env PATH="$test_dir/bin:$PATH" TORANA_LIVE_CASE=deepseek-chat-bridges \
    TORANA_LIVE_DEEPSEEK_TOKEN=fixture TORANA_LIVE_BIN="$test_dir/bin/torana" \
    TORANA_LIVE_PORT=18082 TORANA_LIVE_TIMEOUT_SECONDS=2 FAKE_MARKER="$case_dir/stopped" \
    FAKE_STOP_CALLED="$case_dir/stop-called" "$@" \
    "$repo_dir/scripts/live-harness-regression.sh" >"$case_dir/out" 2>"$case_dir/err"
  case_status=$?
  set -e
  return "$case_status"
}

if ! run_case success env; then
  sed -n '1,80p' "$test_dir/success/err" >&2
  exit 1
fi
if run_case incomplete env FAKE_INCOMPLETE=1; then
  echo "incomplete response was accepted" >&2
  exit 1
fi
if run_case unrelated env FAKE_UNRELATED=1; then
  echo "unrelated text containing 42 was accepted" >&2
  exit 1
fi
if run_case partial_start env FAKE_START_FAIL=1; then
  echo "partial start failure was accepted" >&2
  exit 1
fi
[ -f "$test_dir/partial_start/stop-called" ]
if run_case signal_start env FAKE_START_SIGNAL=1; then
  echo "signal during start was accepted" >&2
  exit 1
fi
[ -f "$test_dir/signal_start/stop-called" ]
if run_case stop_failure env FAKE_STOP_FAIL=1; then
  echo "failed shutdown was accepted" >&2
  exit 1
fi
grep -q 'isolated state retained at:' "$test_dir/stop_failure/err"
retained=$(sed -n 's/^Torana shutdown could not be confirmed; isolated state retained at: //p' "$test_dir/stop_failure/err")
[ -n "$retained" ] && [ -d "$retained" ]
rm -rf "$retained"

if TORANA_LIVE_CASE=deepseek-chat-bridges TORANA_LIVE_DEEPSEEK_TOKEN=fixture \
  TORANA_LIVE_BIN="$test_dir/bin/torana" TORANA_LIVE_TIMEOUT_SECONDS=0 \
  "$repo_dir/scripts/live-harness-regression.sh" >/dev/null 2>&1; then
  echo "zero timeout was accepted" >&2
  exit 1
fi
if TORANA_LIVE_CASE=deepseek-chat-bridges TORANA_LIVE_DEEPSEEK_TOKEN=fixture \
  TORANA_LIVE_BIN="$test_dir/bin/torana" TORANA_LIVE_TIMEOUT_SECONDS=999999999999999999999999 \
  "$repo_dir/scripts/live-harness-regression.sh" >/dev/null 2>&1; then
  echo "oversized timeout integer was accepted" >&2
  exit 1
fi
if TORANA_LIVE_CASE=deepseek-chat-bridges TORANA_LIVE_DEEPSEEK_TOKEN=fixture \
  TORANA_LIVE_BIN="$test_dir/bin/torana" TORANA_LIVE_PORT=70000 \
  "$repo_dir/scripts/live-harness-regression.sh" >/dev/null 2>&1; then
  echo "invalid port was accepted" >&2
  exit 1
fi

echo "PASS live-harness-regression executable fixtures"
