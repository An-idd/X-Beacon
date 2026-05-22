# X-Beacon · OpenAI Python SDK compat suite

Pinned-version OpenAI SDK runs every `chat.completions.create` /
`models.list` / error path the gateway claims to support. If any of
these breaks, an end user who installed the OpenAI Python SDK and
switched their `base_url` to X-Beacon would hit it.

## Running locally

```sh
# from repo root
make compat-python
```

What `make compat-python` does:

1. Builds `mockupstream` and starts it on `127.0.0.1:19091`.
2. Builds and launches a gateway on `127.0.0.1:18080` with a compat
   profile (`configs/providers.compat.yaml`) that routes ALL traffic
   to the mock upstream above.
3. Runs `uv run pytest` against the suite with `XBEACON_BASE_URL` +
   `XBEACON_API_KEY` exported.
4. Tears both processes down on exit (success or failure).

## Manual run (already have gateway up)

```sh
export XBEACON_BASE_URL=http://127.0.0.1:18080
export XBEACON_API_KEY=sk-compat-test
cd test/compat/python
uv sync
uv run pytest -v
```

## Updating the OpenAI SDK pin

The `openai` version is intentionally pinned to a narrow range in
`pyproject.toml`. To bump it:

1. Edit the version constraint.
2. `uv lock` to refresh `uv.lock`.
3. `make compat-python` and confirm all tests still pass.
4. If any test fails: investigate whether it's a real gateway regression
   or an SDK-side breaking change. Document the SDK change in the PR
   if it's the latter.
