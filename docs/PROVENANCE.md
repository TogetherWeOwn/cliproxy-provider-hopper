# Provenance

This plugin is a port of the MIT-licensed
[TogetherWeOwn/cliproxy-provider-synthetic](https://github.com/TogetherWeOwn/cliproxy-provider-synthetic)
v0.2.3. The auth, model and executor wiring keep that plugin's structure; the
Synthetic quota shapes and the Infinite stream aggregator were removed.

The Hopper request, model, usage and header shapes come from live responses of
the public Hopper API observed on 2026-11-07 (`/v1/models`, `/v1/chat/completions`,
`/v1/usage`). Hopper publishes no rate-limit body format; the 429 concurrency
classification is a conservative heuristic that never downgrades an exhausted
window.
