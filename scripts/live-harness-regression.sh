#!/bin/sh
set -eu

# Opt-in, bounded live compatibility probe. It is intentionally absent from CI.
# Secrets are accepted only through the environment and are never placed in
# command arguments, generated configuration, or output.

case_name=${TORANA_LIVE_CASE:-}
torana_bin=${TORANA_LIVE_BIN:-./torana}
port=${TORANA_LIVE_PORT:-18082}
timeout_seconds=${TORANA_LIVE_TIMEOUT_SECONDS:-45}
started=false
cleaned=false

if [ "$case_name" != "deepseek-chat-bridges" ]; then
  echo "TORANA_LIVE_CASE must be deepseek-chat-bridges" >&2
  exit 2
fi
if [ -z "${TORANA_LIVE_DEEPSEEK_TOKEN:-}" ]; then
  echo "TORANA_LIVE_DEEPSEEK_TOKEN is required" >&2
  exit 2
fi
if [ ! -x "$torana_bin" ]; then
  echo "TORANA_LIVE_BIN is not executable: $torana_bin" >&2
  exit 2
fi
case "$timeout_seconds" in
  ''|*[!0-9]*) echo "TORANA_LIVE_TIMEOUT_SECONDS must be an integer from 1 to 300" >&2; exit 2 ;;
esac
if [ "$timeout_seconds" -lt 1 ] || [ "$timeout_seconds" -gt 300 ]; then
  echo "TORANA_LIVE_TIMEOUT_SECONDS must be an integer from 1 to 300" >&2
  exit 2
fi
case "$port" in
  ''|*[!0-9]*) echo "TORANA_LIVE_PORT must be an integer from 1 to 65535" >&2; exit 2 ;;
esac
if [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
  echo "TORANA_LIVE_PORT must be an integer from 1 to 65535" >&2
  exit 2
fi
for command_name in curl jq mktemp; do
  command -v "$command_name" >/dev/null 2>&1 || {
    echo "required command is unavailable: $command_name" >&2
    exit 2
  }
done

live_dir=$(mktemp -d "${TMPDIR:-/tmp}/torana-live-regression.XXXXXX")
seed=$live_dir/seed.json
data_dir=$live_dir/state
server_log=$live_dir/torana.log

cleanup() {
  original_status=$?
  trap - EXIT HUP INT TERM
  [ "$cleaned" = false ] || exit "$original_status"
  cleaned=true
  if [ "$started" = true ]; then
    stop_ok=false
    if TORANA_DATA_DIR=$data_dir "$torana_bin" stop --yes >/dev/null 2>&1; then
      attempt=0
      while [ "$attempt" -lt 50 ]; do
        if ! TORANA_DATA_DIR=$data_dir "$torana_bin" status --json 2>/dev/null |
          jq -e '.status == "running"' >/dev/null 2>&1 &&
          ! curl --silent --fail --max-time 1 "http://127.0.0.1:$port/health" >/dev/null 2>&1; then
          stop_ok=true
          break
        fi
        attempt=$((attempt + 1))
        sleep 0.1
      done
    fi
    if [ "$stop_ok" != true ]; then
      echo "Torana shutdown could not be confirmed; isolated state retained at: $live_dir" >&2
      echo "Retry with: TORANA_DATA_DIR=$data_dir $torana_bin stop --yes" >&2
      exit 1
    fi
  fi
  rm -rf "$live_dir"
  exit "$original_status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

cat >"$seed" <<EOF
{
  "managed": true,
  "port": $port,
  "providers": {
    "deepseek-native": {
      "url": "https://api.deepseek.com",
      "format": "openai",
      "auth": {"mode": "credential", "credential": "deepseek-live"}
    },
    "deepseek-native-anthropic": {
      "url": "https://api.deepseek.com/anthropic",
      "format": "anthropic",
      "auth": {"mode": "credential", "credential": "deepseek-live"}
    },
    "deepseek-responses": {
      "url": "https://api.deepseek.com",
      "format": "openai",
      "auth": {"mode": "credential", "credential": "deepseek-live"},
      "bridge": {"client": "openai-responses", "upstream": "openai-chat", "model": "deepseek-chat"}
    },
    "deepseek-anthropic": {
      "url": "https://api.deepseek.com",
      "format": "openai",
      "auth": {"mode": "credential", "credential": "deepseek-live"},
      "bridge": {"client": "anthropic", "upstream": "openai-chat", "model": "deepseek-chat"}
    },
    "deepseek-gemini": {
      "url": "https://api.deepseek.com",
      "format": "openai",
      "auth": {"mode": "credential", "credential": "deepseek-live"},
      "bridge": {"client": "gemini", "upstream": "openai-chat", "model": "deepseek-chat"}
    },
    "deepseek-codeassist": {
      "url": "https://api.deepseek.com",
      "format": "openai",
      "auth": {"mode": "credential", "credential": "deepseek-live"},
      "bridge": {"client": "gemini-codeassist", "upstream": "openai-chat", "model": "deepseek-chat"}
    }
  },
  "credentials": {
    "sources": {"env": {"type": "env"}},
    "entries": {"deepseek-live": {"source": "env", "key": "TORANA_LIVE_DEEPSEEK_TOKEN"}}
  },
  "plugins": {"order": []}
}
EOF

TORANA_CONFIG=$seed TORANA_DATA_DIR=$data_dir TORANA_BIND=127.0.0.1 TORANA_PORT=$port \
  "$torana_bin" start >"$server_log" 2>&1
started=true

attempt=0
while ! TORANA_DATA_DIR=$data_dir "$torana_bin" status --json 2>/dev/null |
  jq -e '.status == "running"' >/dev/null 2>&1 ||
  ! curl --silent --fail --max-time 1 "http://127.0.0.1:$port/health" >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 50 ]; then
    echo "Torana did not become ready" >&2
    sed -n '1,80p' "$server_log" >&2
    exit 1
  fi
  sleep 0.1
done

post_json() {
  route=$1
  body=$2
  output=$3
  status=$(curl --silent --show-error --max-time "$timeout_seconds" \
    -H 'Content-Type: application/json' --data-binary "$body" \
    --output "$output" --write-out '%{http_code}' \
    "http://127.0.0.1:$port$route")
  case "$status" in
    2??) ;;
    *)
      message=$(jq -r '.error.message // "upstream request failed"' "$output" 2>/dev/null || echo "upstream request failed")
      echo "$route returned HTTP $status: $message" >&2
      if [ -f "$live_dir/native.json" ]; then
        jq -c '{top:(keys|sort),choice:(.choices[0]|keys|sort),message:(.choices[0].message|keys|sort),usage:.usage}' \
          "$live_dir/native.json" >&2 2>/dev/null || true
      fi
      tail -n 30 "$data_dir/torana.log" >&2 2>/dev/null || true
      return 1
      ;;
  esac
}

require_json() {
  label=$1
  filter=$2
  file=$3
  if ! jq -e "$filter" "$file" >/dev/null 2>&1; then
    echo "$label did not return completed text matching the fixed probe" >&2
    return 1
  fi
}

post_json /provider/deepseek-native/v1/chat/completions \
  '{"model":"deepseek-flash","max_tokens":8,"reasoning_effort":"none","messages":[{"role":"user","content":"Reply with exactly 42"}]}' \
  "$live_dir/native.json"
require_json "native OpenAI Chat" '.choices[0].finish_reason == "stop" and (.choices[0].message.content | strings | contains("42"))' "$live_dir/native.json"
echo "PASS native OpenAI Chat"

post_json /provider/deepseek-native/v1/responses \
  '{"model":"deepseek-flash","max_output_tokens":8,"reasoning":{"effort":"none"},"input":"Reply with exactly 42"}' \
  "$live_dir/native-responses.json"
require_json "native OpenAI Responses" '.object == "response" and .status == "completed" and any(.output[]?; .type == "message" and any(.content[]?; .type == "output_text" and (.text | strings | contains("42"))))' "$live_dir/native-responses.json"
echo "PASS native OpenAI Responses"

post_json /provider/deepseek-native-anthropic/v1/messages \
  '{"model":"deepseek-flash","max_tokens":8,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"Reply with exactly 42"}]}' \
  "$live_dir/native-anthropic.json"
require_json "native Anthropic" '.type == "message" and .stop_reason == "end_turn" and any(.content[]?; .type == "text" and (.text | strings | contains("42")))' "$live_dir/native-anthropic.json"
echo "PASS native Anthropic"

post_json /provider/deepseek-responses/v1/responses \
  '{"model":"client-model","max_output_tokens":8,"input":"Reply with exactly 42"}' \
  "$live_dir/responses.json"
require_json "translated Responses" '.object == "response" and .status == "completed" and any(.output[]?; .type == "message" and any(.content[]?; .type == "output_text" and (.text | strings | contains("42"))))' "$live_dir/responses.json"
echo "PASS Responses client translated to Chat upstream"

post_json /provider/deepseek-anthropic/v1/messages \
  '{"model":"client-model","max_tokens":8,"messages":[{"role":"user","content":"Reply with exactly 42"}]}' \
  "$live_dir/anthropic.json"
require_json "translated Anthropic" '.type == "message" and .stop_reason == "end_turn" and any(.content[]?; .type == "text" and (.text | strings | contains("42")))' "$live_dir/anthropic.json"
echo "PASS Anthropic client translated to Chat upstream"

post_json /provider/deepseek-gemini/v1beta/models/client-model:generateContent \
  '{"contents":[{"role":"user","parts":[{"text":"Reply with exactly 42"}]}],"generationConfig":{"maxOutputTokens":8}}' \
  "$live_dir/gemini.json"
require_json "translated Gemini" 'any(.candidates[]?; .finishReason == "STOP" and any(.content.parts[]?; (.text | strings | contains("42"))))' "$live_dir/gemini.json"
echo "PASS Gemini client translated to Chat upstream"

post_json /provider/deepseek-codeassist/v1internal:generateContent \
  '{"model":"client-model","request":{"contents":[{"role":"user","parts":[{"text":"Reply with exactly 42"}]}],"generationConfig":{"maxOutputTokens":8}}}' \
  "$live_dir/codeassist.json"
require_json "translated Code Assist" 'any(.response.candidates[]?; .finishReason == "STOP" and any(.content.parts[]?; (.text | strings | contains("42"))))' "$live_dir/codeassist.json"
echo "PASS Code Assist client translated to Chat upstream"

TORANA_DATA_DIR=$data_dir "$torana_bin" feed >"$live_dir/feed.json"
jq -e 'length == 7 and all(.[]; .status == 200 and .tokens_in > 0 and .tokens_out > 0)' \
  "$live_dir/feed.json" >/dev/null

echo "PASS deepseek-chat-bridges: native Chat, Responses, and Anthropic plus four translated client shapes; feed usage present"
