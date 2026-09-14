#!/usr/bin/env python3
"""Validate bridge fixtures with pinned official provider SDK models.

This script performs no provider or network calls. Install the adjacent pinned
requirements, export fixtures with the Go tests, then pass their directory:

  python -m pip install -r scripts/bridge-sdk-validation-requirements.txt
  TORANA_BRIDGE_SDK_FIXTURE_DIR=/tmp/bridge-sdk-fixtures \
    go test ./internal/bridge -run 'TestExportSDKValidation.*Fixtures' -count=1
  python scripts/validate-bridge-sdk.py --fixtures /tmp/bridge-sdk-fixtures
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Iterable

import anthropic
import httpx
import openai
from anthropic.types import Message, RawMessageStreamEvent
from openai.types.chat import ChatCompletion, ChatCompletionChunk
from openai.types.responses import Response, ResponseStreamEvent
from pydantic import TypeAdapter


def sse_data(path: Path) -> Iterable[str]:
    chunks: list[str] = []
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line:
            if chunks:
                yield "\n".join(chunks)
                chunks = []
            continue
        if line.startswith("data:"):
            chunks.append(line[5:].lstrip())
    if chunks:
        yield "\n".join(chunks)


def validate_sse(
    path: Path,
    adapter: TypeAdapter | None = None,
    *,
    allow_anthropic_ping: bool = False,
) -> int:
    count = 0
    for data in sse_data(path):
        if data == "[DONE]":
            continue
        # Anthropic documents ping as a transport event, but its pinned public
        # RawMessageStreamEvent response union intentionally omits that arm.
        if allow_anthropic_ping and json.loads(data) == {"type": "ping"}:
            count += 1
            continue
        if adapter is None:
            ChatCompletionChunk.model_validate_json(data, strict=True)
        else:
            adapter.validate_json(data, strict=True)
        count += 1
    if count == 0:
        raise ValueError(f"{path} contained no JSON SSE events")
    return count


def fixture_transport(path: Path) -> httpx.MockTransport:
    payload = path.read_bytes()

    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            headers={"content-type": "text/event-stream"},
            content=payload,
        )

    return httpx.MockTransport(handler)


def validate_stream_accumulators(fixtures: Path) -> dict[str, int]:
    responses_client = openai.OpenAI(
        api_key="sdk-fixture",
        base_url="https://fixture.invalid/v1",
        http_client=httpx.Client(
            transport=fixture_transport(fixtures / "openai-responses-tool.sse")
        ),
        _strict_response_validation=True,
    )
    try:
        with responses_client.responses.stream(
            model="sdk-model", input="fixture"
        ) as stream:
            response = stream.get_final_response()
    finally:
        responses_client.close()
    calls = [item for item in response.output if item.type == "function_call"]
    if (
        [call.call_id for call in calls]
        != ["call_sdk_alpha", "call_sdk_beta"]
        or [json.loads(call.arguments)["n"] for call in calls]
        != [9007199254740993, 9007199254740995]
        or len({call.id for call in calls}) != 2
        or any(call.id == call.call_id for call in calls)
    ):
        raise ValueError(
            "OpenAI Responses accumulator lost tool identities or arguments"
        )

    anthropic_client = anthropic.Anthropic(
        api_key="sdk-fixture",
        base_url="https://fixture.invalid",
        http_client=httpx.Client(
            transport=fixture_transport(fixtures / "anthropic.sse")
        ),
        _strict_response_validation=True,
    )
    try:
        with anthropic_client.messages.stream(
            model="sdk-model",
            max_tokens=16,
            messages=[{"role": "user", "content": "fixture"}],
        ) as stream:
            message = stream.get_final_message()
    finally:
        anthropic_client.close()
    if (
        message.usage.input_tokens != 7
        or message.usage.output_tokens != 2
        or message.usage.cache_read_input_tokens != 3
    ):
        raise ValueError(f"Anthropic accumulator lost terminal usage: {message.usage}")

    return {
        "openai_response_tool_calls": len(calls),
        "anthropic_final_input_tokens": message.usage.input_tokens,
        "anthropic_final_output_tokens": message.usage.output_tokens,
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--fixtures", type=Path, required=True)
    args = parser.parse_args()
    fixtures = args.fixtures

    ChatCompletion.model_validate_json(
        (fixtures / "openai-chat.json").read_text(), strict=True
    )
    Response.model_validate_json(
        (fixtures / "openai-responses.json").read_text(), strict=True
    )
    Message.model_validate_json(
        (fixtures / "anthropic.json").read_text(), strict=True
    )

    event_counts: dict[str, int] = {}
    stream_specs = (
        ("openai-chat*.sse", None, False),
        ("openai-responses*.sse", TypeAdapter(ResponseStreamEvent), False),
        ("anthropic*.sse", TypeAdapter(RawMessageStreamEvent), True),
    )
    for required in (
        "openai-chat.sse",
        "openai-responses.sse",
        "anthropic.sse",
        "openai-chat-tool.sse",
        "openai-responses-tool.sse",
        "anthropic-tool.sse",
    ):
        if not (fixtures / required).exists():
            raise FileNotFoundError(f"required stream fixture is missing: {required}")
    for pattern, adapter, allow_ping in stream_specs:
        for path in sorted(fixtures.glob(pattern)):
            event_counts[path.name] = validate_sse(
                path, adapter, allow_anthropic_ping=allow_ping
            )
    accumulators = validate_stream_accumulators(fixtures)

    print(
        json.dumps(
            {
                "openai": openai.__version__,
                "anthropic": anthropic.__version__,
                "json_fixtures": 3,
                "stream_events": event_counts,
                "stream_accumulators": accumulators,
            },
            sort_keys=True,
        )
    )


if __name__ == "__main__":
    main()
