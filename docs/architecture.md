# Architecture

cfrproxy is a single Go binary with two planes:

- **Data plane** — the endpoints your harnesses call: `/v1/chat/completions` (OpenAI), `/v1/messages` (Anthropic), `/api/chat` (Ollama).
- **Management plane** — the WebUI, REST API, and live trace stream at `/admin/`, behind HTTP basic auth.

State is one SQLite file (`~/.cfrproxy/cfrproxy.db`, WAL mode) plus a `0600` `secret.key` for encrypting API keys.

## Request lifecycle

```
inbound request (any dialect)
  │
  ├─ parse to a normalized schema (internal/wire)
  ├─ resolve the model → (provider, real model id)     internal/proxy/models.go
  │     • model map rewrite  • provider/model prefix  • alias  • fuzzy  • default
  ├─ auto-route?  classify → pick bucket model         internal/proxy/autoroute.go
  ├─ compress?    summarize old turns if over budget   internal/proxy/compress.go
  ├─ build the candidate chain (primary + fallbacks)
  │
  └─ for each candidate until one succeeds:
        ├─ translate normalized → provider dialect      internal/wire
        ├─ apply request-phase transforms               internal/transform
        ├─ send; retry once on transient (408/429/5xx)
        ├─ on transient failure → next candidate (failover)
        └─ on success:
              ├─ translate response back to the inbound dialect
              │   (or copy bytes untouched on the raw fast path)
              ├─ apply response-phase transforms
              ├─ capture usage (tokens, cache-read)
              └─ record a trace → SQLite + live SSE
```

## The wire layer (`internal/wire`)

The heart of the proxy. A `Request`/`Response` normalized form sits in the middle, and each dialect has a parser and a builder:

- **openai.go** — chat-completions, SSE streaming, `reasoning_effort`.
- **anthropic.go** — `/v1/messages`, event-stream framing, `thinking` blocks, mid-conversation system-message folding.
- **ollama.go** — `/api/chat`, NDJSON streaming.

Streaming is re-framed *between* dialects: an OpenAI SSE stream from the provider can be delivered to the client as Anthropic events, or Ollama NDJSON, tool calls included. Reasoning controls (`reasoning_effort` ↔ `thinking` budget) are mapped across dialects, not dropped.

**Raw fast path:** when the inbound dialect already matches the provider type and no transforms / auto-routing / compression apply, the provider's bytes are streamed through untouched (usage is still scanned out for the dashboard). Any request that needs rewriting takes the full translate path.

## Packages

| Package | Responsibility |
|---|---|
| `internal/store` | SQLite persistence, AES-256-GCM key encryption, provider cache, per-model stats |
| `internal/wire` | Normalized schema + per-dialect parse/build + stream re-framing |
| `internal/transform` | Declarative JSON rewrite rules (set/default/rename/delete) |
| `internal/proxy` | Data plane: routing, candidate chain, auto-router, compression, tracing |
| `internal/api` | Management REST + SSE + embedded WebUI + OAuth account proxy |
| `internal/tui` | Bubble Tea management console |
| `main.go`, `launch.go`, `mcp.go` | CLI, harness launcher, MCP server |
