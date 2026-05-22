"""Shared fixtures for the OpenAI SDK compatibility suite.

The tests assume a running gateway. We do **not** orchestrate it from
Python: that's the job of `make compat-python` locally and the
.github/workflows/compat.yml job in CI. This keeps the Python suite a
pure consumer — easy to point at any environment (staging, prod
shadow, locally-built binary) by changing two env vars.

Required env vars:
    XBEACON_BASE_URL   gateway base URL, no trailing slash (e.g. http://127.0.0.1:18080)
    XBEACON_API_KEY    bearer token configured in the gateway's auth table

The conftest fails fast at session start if either is missing, rather
than producing twelve identical connection-refused errors.
"""
from __future__ import annotations

import os

import httpx
import pytest
from openai import OpenAI


def _require(name: str) -> str:
    val = os.environ.get(name, "").strip()
    if not val:
        pytest.exit(
            f"compat suite requires env var {name}; "
            f"set it to the gateway URL/key before running pytest.",
            returncode=2,
        )
    return val


@pytest.fixture(scope="session")
def base_url() -> str:
    return _require("XBEACON_BASE_URL").rstrip("/")


@pytest.fixture(scope="session")
def api_key() -> str:
    return _require("XBEACON_API_KEY")


@pytest.fixture(scope="session")
def client(base_url: str, api_key: str) -> OpenAI:
    """OpenAI SDK client pointed at the gateway.

    base_url is `<gateway>/v1` because the SDK appends `/chat/...`
    relative to it. The gateway mounts /v1/* as documented.
    """
    return OpenAI(
        base_url=f"{base_url}/v1",
        api_key=api_key,
        # Disable retries: we want SDK error mapping to be visible per-test.
        max_retries=0,
        timeout=10.0,
    )


@pytest.fixture(scope="session")
def raw_http(base_url: str, api_key: str) -> httpx.Client:
    """Pre-authenticated httpx client for the few tests that need to
    bypass the SDK (intentional 4xx triggers, raw header inspection)."""
    return httpx.Client(
        base_url=base_url,
        headers={"Authorization": f"Bearer {api_key}"},
        timeout=10.0,
    )


# Model used by the mock upstream. The CI gateway config (configs/
# providers.compat.yaml) routes ALL traffic to mockupstream, so the
# model name only matters insofar as it must be in the registry.
DEFAULT_MODEL = "gpt-4o-mini"
