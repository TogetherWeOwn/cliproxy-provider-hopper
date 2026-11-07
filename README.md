# cliproxy-provider-hopper

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) v8 provider plugin
for [Hopper](https://withhopper.com), built on
[cliproxy-provider-kit](https://github.com/TogetherWeOwn/cliproxy-provider-kit).

- API-key authentication (`type: hopper` auth files or the management login page)
- Namespaced OpenAI-compatible chat models under the fixed `hopper/` prefix
- Streaming and non-streaming chat completions, tool calls and reasoning fields pass through unchanged
- Read-only quota cards for Hopper's five-hour and weekly USD spend windows

## Install

Copy `hopper.so` from a release archive to `plugins/linux/amd64/` and enable it:

```yaml
plugins:
  enabled: true
  configs:
    hopper:
      enabled: true
```

Add an auth file to the auth directory:

```json
{
  "type": "hopper",
  "api_key": "<hopper-api-key>",
  "label": "Hopper"
}
```

The auth ID is derived from the API key; an explicit `id` is rejected and the
same key may be loaded from only one file.

## Models

The plugin strips `hopper/` and calls `POST https://api.withhopper.com/v1/chat/completions`.
Discovery uses `GET /v1/models` for each key, so a model appears only when that
key is entitled to it. The static fallback catalog is:

| CLIProxy ID | Upstream ID |
|---|---|
| `hopper/deepseek-v4.1-flash` | `deepseek-v4.1-flash` |
| `hopper/deepseek-v4.1-flash-1017` | `deepseek-v4.1-flash-1017` |
| `hopper/deepseek-v4.1-flash-1017-abliterated` | `deepseek-v4.1-flash-1017-abliterated` |

Embedding models are not registered.

## Quota

The quota card polls `GET /v1/usage` and renders one bucket per window
(`5h` and `weekly`, labelled "5-hour limit" and "Weekly limit") with remaining fraction and reset time. Inference
responses carry `x-hopper-5h-*` and `x-hopper-7d-*` headers; the usage plugin
refreshes the card from them without an extra poll. The plan chip shows the
plan key reported by Hopper, and a Balance chip shows the prepaid credits
(`credits_remaining_usd`) from the latest usage poll.

These readings are classification hints only: they never select or skip an
account ahead of time.

## Errors

| Upstream | Code | Retry |
|---|---|---|
| 401 | `invalid_credentials` | none |
| 402 | `payment_required` | 1 hour |
| 403 | `entitlement_required` | 1 hour |
| 404 | `upstream_not_found` | none |
| 429, a window exhausted | `quota_exhausted` | until every exhausted window resets |
| 429, windows have headroom and the body names concurrency | `concurrency_limit` | 5 s |
| 5xx, 408 | `upstream_error` | retryable |

An upstream `Retry-After` header always wins. No response body or credential is
copied into an error.

## Development

```sh
go test -race ./...
go -C integration/hostcompat test -race ./...   # real v8.0.49 host loader
bash scripts/package-release.sh dist
```

## License

MIT
