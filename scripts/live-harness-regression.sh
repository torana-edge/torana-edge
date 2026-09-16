#!/bin/sh
set -eu

# Opt-in, bounded live compatibility probe. It is intentionally absent from CI.
# Secrets are accepted only through the environment and are never placed in
# command arguments, generated configuration, or output.

case_name=${TORANA_LIVE_CASE:-}
torana_bin=${TORANA_LIVE_BIN:-./torana}
port=${TORANA_LIVE_PORT:-18082}
timeout_seconds=${TORANA_LIVE_TIMEOUT_SECONDS:-45}

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
  TORANA_DATA_DIR=$data_dir "$torana_bin" stop --yes >/dev/null 2>&1 || true
  rm -rf "$live_dir"
}
trap cleanup EXIT HUP INT TERM

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

post_json /provider/deepseek-native/v1/chat/completions \
  '{"model":"deepseek-chat","max_tokens":8,"messages":[{"role":"user","content":"Reply with exactly 42"}]}' \
  "$live_dir/native.json"
jq -e '.choices[0].message.content | strings | contains("42")' "$live_dir/native.json" >/dev/null

post_json /provider/deepseek-responses/v1/responses \
  '{"model":"client-model","max_output_tokens":8,"input":"Reply with exactly 42"}' \
  "$live_dir/responses.json"
jq -e '.object == "response" and (.output | type == "array")' "$live_dir/responses.json" >/dev/null

post_json /provider/deepseek-anthropic/v1/messages \
  '{"model":"client-model","max_tokens":8,"messages":[{"role":"user","content":"Reply with exactly 42"}]}' \
  "$live_dir/anthropic.json"
jq -e '.type == "message" and (.content | type == "array")' "$live_dir/anthropic.json" >/dev/null

post_json /provider/deepseek-gemini/v1beta/models/client-model:generateContent \
  '{"contents":[{"role":"user","parts":[{"text":"Reply with exactly 42"}]}],"generationConfig":{"maxOutputTokens":8}}' \
  "$live_dir/gemini.json"
jq -e '.candidates | type == "array"' "$live_dir/gemini.json" >/dev/null

post_json /provider/deepseek-codeassist/v1internal:generateContent \
  '{"model":"client-model","request":{"contents":[{"role":"user","parts":[{"text":"Reply with exactly 42"}]}],"generationConfig":{"maxOutputTokens":8}}}' \
  "$live_dir/codeassist.json"
jq -e '.response.candidates | type == "array"' "$live_dir/codeassist.json" >/dev/null

TORANA_DATA_DIR=$data_dir "$torana_bin" feed >"$live_dir/feed.json"
jq -e 'length == 5 and all(.[]; .status == 200 and .tokens_in > 0 and .tokens_out > 0)' \
  "$live_dir/feed.json" >/dev/null

echo "PASS deepseek-chat-bridges: native OpenAI Chat plus Responses, Anthropic, Gemini, and Code Assist clients; feed usage present"
