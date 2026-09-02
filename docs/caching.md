# Prompt-cache care: sticky routes, prefix capture, warmup, cache log

Local inference is dominated by prefill. On fred, 11 of 101 long requests that missed the prefix
cache accounted for 67 % of all prefill work (TTFB 9.6 s vs 1.3 s warm). Several cfrproxy
features exist only to keep that cache warm; this page says what each does and how to read it.

## Sticky routing (auto-router)

`auto` classifies each request into a bucket. A conversation whose turns land on different
models pays a cold prefix every turn, so the router pins a conversation to the model it was
first routed to (key = system prompt + first user message, 30 min TTL) and skips the classifier
on later turns. On by default; `"sticky": false` in the `auto_router` setting turns it off.

## Prefix capture + warmup (`kvwarm`)

The proxy records the static head of every request served by a local upstream — system prompt
plus tool schemas, canonicalised and content-addressed — into `~/.cfrproxy/cache/`. The
`kvwarm.service` sidecar (`scripts/kvwarm.py`, config `~/.cfrproxy/kvwarm.json`) replays those
manifests against llama.cpp / vLLM after a model (re)load and between real requests, so the
first request of a session finds its prefix resident. It waits for the inference slot to be idle,
decodes 16 tokens to catch cache corruption, and never competes with real traffic
(`Nice=10`, idle I/O class). `CFRPROXY_PREFIX_CACHE=off` disables recording.

## Cache observability log

llama.cpp-family upstreams return a `timings` object with the only ground truth about prefix
reuse: tokens restored from cache, tokens re-prefilled, and why a restore missed. cfrproxy copies
it onto the trace (`cached_tokens`, TTFB, post-processing µs) and appends one JSON line per
request to `~/.cfrproxy/cache-observability.jsonl`, labelled by client (`claude-code`, `codex`,
`omp`, `hermes`, …) so hit rates are attributable per harness. `CFRPROXY_CACHE_LOG=off`
disables the file. The WebUI's Live Traces tab shows cache-hit % per model from the same data.

## Pool affinity

A logical model served by several instances (see [pools.md](pools.md)) has a separate KV cache
per instance. Affinity pools send a conversation back to the instance that already holds its
KV, and a new conversation to the instance that has served its static prefix before, before
considering load.

## What cfrproxy will not do to the prefix

- Context compression and caveman compression never touch the system prompt or tool schemas,
  and caveman is deterministic per message, so earlier turns are not rewritten as a
  conversation grows.
- An `auto-plan` briefing is appended to the final user message, not the system prompt.
- Mid-conversation system messages are hoisted so they do not void the Claude Code prefix.
