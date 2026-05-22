"""OpenAI Python SDK compatibility regression suite for X-Beacon.

Each test exercises one SDK surface and asserts the gateway's response
flows through SDK parsing without raising. Failures here = a real user
who installed `openai` from PyPI and pointed it at the gateway just broke.

Tests grouped by SDK feature:

  * chat.completions.create     non-stream / stream
  * tool calling                tools + tool_choice
  * structured output           response_format = json_object
  * logprobs
  * error class mapping         AuthenticationError / BadRequestError
  * models.list                 catalog discovery

All tests assume an empty messages list won't fail (the mock upstream
doesn't simulate). The fixtures are session-scoped, so the SDK keeps a
connection pool alive across the whole run.
"""
from __future__ import annotations

import json

import httpx
import openai
import pytest

from conftest import DEFAULT_MODEL


# ---------- chat.completions.create — non-streaming ----------


def test_chat_completion_basic(client):
    """The bread-and-butter call. If this breaks, nothing else matters."""
    resp = client.chat.completions.create(
        model=DEFAULT_MODEL,
        messages=[{"role": "user", "content": "hi"}],
    )
    assert resp.id, "id must be present"
    assert resp.object == "chat.completion"
    assert resp.model
    assert len(resp.choices) == 1
    choice = resp.choices[0]
    assert choice.index == 0
    assert choice.message.role == "assistant"
    assert choice.message.content
    assert choice.finish_reason == "stop"
    assert resp.usage is not None
    assert resp.usage.total_tokens > 0


def test_chat_completion_streaming(client):
    """Stream must yield chunks the SDK iterator parses cleanly."""
    stream = client.chat.completions.create(
        model=DEFAULT_MODEL,
        messages=[{"role": "user", "content": "stream please"}],
        stream=True,
    )
    chunks = list(stream)
    assert chunks, "stream produced no chunks; [DONE] terminator may be missing"
    # First chunk usually carries the role delta; the SDK parses
    # this into `delta.role == "assistant"`.
    roles = [c.choices[0].delta.role for c in chunks if c.choices[0].delta.role]
    assert "assistant" in roles, "first chunk's delta should set role=assistant"
    # Some chunk in the middle must carry content.
    contents = [
        c.choices[0].delta.content
        for c in chunks
        if c.choices[0].delta.content
    ]
    assert contents, "no content delta arrived in any chunk"
    # Last chunk before [DONE] carries finish_reason.
    finish_reasons = [
        c.choices[0].finish_reason for c in chunks if c.choices[0].finish_reason
    ]
    assert "stop" in finish_reasons, "stream did not surface finish_reason"


# ---------- tool calling ----------


def test_chat_completion_with_tools(client):
    """Tool-call wire shape: the most-broken feature across LLM gateways.

    What we assert:
      - SDK parses tool_calls without raising
      - arguments is a *string* (so client can json.loads it)
      - arguments parses back to the dict we'd expect
    """
    tools = [
        {
            "type": "function",
            "function": {
                "name": "search",
                "description": "search the web",
                "parameters": {
                    "type": "object",
                    "properties": {"q": {"type": "string"}},
                    "required": ["q"],
                },
            },
        }
    ]
    resp = client.chat.completions.create(
        model=DEFAULT_MODEL,
        messages=[{"role": "user", "content": "find something"}],
        tools=tools,
    )
    msg = resp.choices[0].message
    assert resp.choices[0].finish_reason == "tool_calls"
    assert msg.tool_calls, "tool_calls must be present when finish_reason=tool_calls"
    tc = msg.tool_calls[0]
    assert tc.type == "function"
    assert tc.function.name
    # The whole point: arguments survives as a JSON string.
    assert isinstance(tc.function.arguments, str)
    parsed = json.loads(tc.function.arguments)
    assert isinstance(parsed, dict), "arguments string must be a JSON object"


def test_chat_completion_tool_choice_forced(client):
    """tool_choice="required" must round-trip through the gateway.

    The gateway must NOT eat the field; if it does, the upstream sees
    `auto` and may decline to call any tool.
    """
    tools = [
        {
            "type": "function",
            "function": {
                "name": "search",
                "parameters": {"type": "object", "properties": {"q": {"type": "string"}}},
            },
        }
    ]
    resp = client.chat.completions.create(
        model=DEFAULT_MODEL,
        messages=[{"role": "user", "content": "x"}],
        tools=tools,
        tool_choice="required",
    )
    # Behavior assertion: when the mock sees `tools`, it returns a
    # tool_call; verifies the gateway forwarded the `tools` block.
    assert resp.choices[0].message.tool_calls


# ---------- response_format / structured output ----------


def test_chat_completion_json_mode(client):
    """response_format={"type":"json_object"} must reach the upstream."""
    resp = client.chat.completions.create(
        model=DEFAULT_MODEL,
        messages=[{"role": "user", "content": "give me json"}],
        response_format={"type": "json_object"},
    )
    content = resp.choices[0].message.content
    assert content, "json_object response must have content"
    # Content must itself be parseable JSON — that's the contract.
    parsed = json.loads(content)
    assert isinstance(parsed, dict)


# ---------- logprobs ----------


def test_chat_completion_logprobs(client):
    """logprobs=True must produce a populated logprobs field on choices."""
    resp = client.chat.completions.create(
        model=DEFAULT_MODEL,
        messages=[{"role": "user", "content": "anything"}],
        logprobs=True,
    )
    lp = resp.choices[0].logprobs
    assert lp is not None, "logprobs requested but not surfaced"
    assert lp.content, "logprobs.content must be a non-empty list"
    entry = lp.content[0]
    assert entry.token
    assert isinstance(entry.logprob, (int, float))


# ---------- models.list ----------


def test_models_list(client):
    """SDK's models.list() must parse the gateway's /v1/models output."""
    page = client.models.list()
    items = list(page)
    assert items, "/v1/models returned an empty catalog"
    for m in items:
        assert m.id
        assert m.object == "model"
        # owned_by is part of the OpenAI shape; SDK doesn't fail without
        # it but real clients (LangChain, autogen) do.
        assert m.owned_by


def test_models_list_extensions_ignored_by_sdk(client):
    """Day 1-2 added pricing/capabilities/etc. The SDK must tolerate
    these new fields without raising (forward-compat protection)."""
    page = client.models.list()
    list(page)  # if SDK strict-parsed, this would have raised already


# ---------- error mapping ----------


def test_auth_error_raises_authentication_error(base_url: str):
    """Bad API key → openai.AuthenticationError, not a generic APIError."""
    bad_client = openai.OpenAI(
        base_url=f"{base_url}/v1",
        api_key="sk-this-is-not-valid",
        max_retries=0,
        timeout=5.0,
    )
    with pytest.raises(openai.AuthenticationError):
        bad_client.chat.completions.create(
            model=DEFAULT_MODEL,
            messages=[{"role": "user", "content": "x"}],
        )


def test_missing_auth_raises_authentication_error(base_url: str):
    """No Authorization header → still AuthenticationError class."""
    # The SDK requires *some* api_key string. Empty is rejected
    # client-side; we use a deliberately invalid token to ensure the
    # 401 comes from the gateway, not the SDK.
    bad_client = openai.OpenAI(
        base_url=f"{base_url}/v1",
        api_key="x",
        max_retries=0,
        timeout=5.0,
    )
    with pytest.raises(openai.AuthenticationError):
        bad_client.chat.completions.create(
            model=DEFAULT_MODEL,
            messages=[{"role": "user", "content": "x"}],
        )


def test_bad_request_raises_bad_request_error(raw_http: httpx.Client):
    """Malformed JSON body should map to a BadRequestError shape.

    We have to bypass the SDK here — the SDK won't *send* malformed JSON.
    We send via httpx and just confirm the gateway emits a 4xx with a
    well-formed error envelope; the SDK's BadRequestError class maps
    `status_code in (400, 422)` against that shape.
    """
    resp = raw_http.post("/v1/chat/completions", content=b"{this is not json")
    assert 400 <= resp.status_code < 500
    body = resp.json()
    assert "error" in body
    err = body["error"]
    assert err.get("type"), "error.type required for SDK error class mapping"
    assert err.get("message")


def test_unknown_model_routes_or_errors_cleanly(client):
    """A model not registered in the gateway must produce a clear error,
    not a 500. The default_provider in configs/providers.compat.yaml is
    set, so this will pass through and the mock will reply ok; if it's
    unset, a 4xx with a sensible message is also acceptable.
    """
    try:
        resp = client.chat.completions.create(
            model="totally-fake-model-xyz",
            messages=[{"role": "user", "content": "x"}],
        )
        # If default_provider absorbs it, we get a normal response.
        assert resp.choices
    except openai.APIError as e:
        # Otherwise: the gateway must give a clean, mapped error.
        assert e.status_code < 500, f"unknown model triggered 5xx: {e}"
