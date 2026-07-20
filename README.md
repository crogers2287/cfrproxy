# cfrproxy

Universal LLM proxy in a single Go binary: any harness dialect in, any provider out, with declarative transforms in between. The generic version of what `ollama launch claude --model glm-5.2:cloud` does — point Claude Code, Codex, OpenCode, or anything else at any provider (cloud or local Ollama) and cfrproxy translates the wire format both ways, including streaming.

## Quick start

```bash
go build -o cfrproxy .
./cfrproxy provider add --name fred --type openai --base-url http://fred:9069 --model agents-a1
./cfrproxy provider add --name ollama --preset ollama --model qwen2.5:7b
./cfrproxy serve                  # :8420; prints WebUI password on first run
```

Point harnesses at it:

| Harness | Setting |
|---|---|
| Claude Code | `ANTHROPIC_BASE_URL=http://localhost:8420` (`/v1/messages`) |
| Codex / OpenCode / OpenAI-compat | base URL `http://localhost:8420/v1` |
| Ollama-native consumers | `OLLAMA_HOST=http://localhost:8420` (`/api/chat`) |

Model names route requests:
- `fred/agents-a1` — provider `fred`, model `agents-a1` (`fred/` alone uses its default model)
- bare name (`agents-a1`) — matched against each provider's alias list
- anything else — highest-priority enabled provider (drag cards in the WebUI or `route set` to reorder)

## Surfaces

- **Data plane** — `POST /v1/chat/completions` (OpenAI), `POST /v1/messages` (Anthropic), `POST /api/chat` (Ollama NDJSON), plus `GET /v1/models`, `/api/tags`, `/health`. Streaming is re-framed between dialects (SSE chunks ↔ Anthropic events ↔ NDJSON), tool calls included. When inbound dialect == provider type and no transforms/doc-injection apply, bytes pass through untouched (raw flow).
- **CLI** — `provider add|list|rm|edit`, `route [set a,b,...]`, `test --name N`, `logs [-f]`, `transform list|add|rm|enable|disable`, `passwd`.
- **TUI** — `cfrproxy tui`: providers (add/edit/delete/reorder with J/K, toggle, test), transforms, live log tail, test prompt.
- **WebUI** — `http://localhost:8420/admin/` behind basic auth: provider card grid with drag-to-reorder priority, transform rule editor, live SSE request traces, docs panel (URL fetch or `.md` upload, inject toggle).

## Transforms

Declarative JSON rules stored in the DB, editable in the WebUI/TUI/CLI. `request` phase rewrites the outbound provider body; `response` phase rewrites the body returned to the consumer. Ops: `set`, `default`, `rename`, `delete`; paths are dot-separated (`options.num_ctx`). Scope by provider and/or inbound dialect.

```bash
./cfrproxy transform add --name pin-temp --phase request --provider fred \
  --rules '[{"op":"set","path":"temperature","value":0.1}]'
```

## Docs per provider

Attach a docs URL or upload Markdown per provider (WebUI Docs tab or `--doc-url/--doc-file`). With inject enabled, the Markdown is prepended as system context on every request routed to that provider.

## Storage & security

Everything lives in `~/.cfrproxy/` (override with `--data`): WAL-mode SQLite (`cfrproxy.db`) with an in-memory registry cache on the hot path (cross-process invalidation via `PRAGMA data_version`), and `secret.key` (0600). API keys are AES-256-GCM encrypted at rest, never returned by the API, and never logged — trace snippets record bodies only, auth headers are never persisted. WebUI/management API require basic auth (user `admin`; password generated on first run, reset with `cfrproxy passwd --pass NEW`).

## Tests

```bash
go test ./...   # crypto-at-rest, routing, transform ops, cross-dialect + stream re-framing
```
