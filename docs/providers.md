# Providers

A provider is any endpoint cfrproxy can forward to. Add them in the WebUI (Providers tab) or the CLI.

## Provider types

| Type | For | Endpoint cfrproxy calls |
|---|---|---|
| `openai` | OpenAI, OpenRouter, xAI, most gateways, any OpenAI-compatible API | `/v1/chat/completions` |
| `anthropic` | Anthropic / Claude | `/v1/messages` |
| `ollama` | local Ollama, ollama.com cloud models | `/api/chat` |

Presets fill in the base URL: `--preset openrouter | openai | anthropic | ollama | supergrok`.

```bash
cfrproxy provider add --name openrouter --preset openrouter --key sk-or-... --model anthropic/claude-sonnet-4
cfrproxy provider add --name local --preset ollama --model qwen2.5:7b
cfrproxy provider add --name mygateway --type openai --base-url https://gateway.example.com/v1 --key sk-...
```

Base URLs are auto-normalized: cfrproxy adds the scheme, strips a pasted endpoint path, and probes `/v1` vs `/api/v1` to find the one that answers. Paste whatever you have.

## Routing priority

Bare/unknown model names go to the highest-priority enabled provider. Reorder by dragging cards in the WebUI, or:

```bash
cfrproxy route set openrouter,local,mygateway
```

## Model pinning (curated lists)

By default a provider exposes its whole catalog to model pickers. A large OAuth or gateway provider can list *hundreds* of models — unusable in a dropdown. Pin a curated subset:

```bash
cfrproxy provider edit --name openrouter --pinned "anthropic/claude-sonnet-4,openai/gpt-4o,google/gemini-2.5-flash"
```

Pickers then show only the pins. The full catalog is still reachable with `?all=1` on the scoped mount (`/p/openrouter/v1/models?all=1`) or the WebUI "Scan models" button.

You can also filter what a provider's *scan* returns with globs (useful to split one backend into categories):

```bash
# only expose gpt-* models from this provider
cfrproxy provider edit --name codex --filter "gpt-*,!gpt-*-mini"
```

## Fallback chains

Give a provider a fallback (`provider/model`). On a transient error — connection failure, timeout, 408/429/5xx — cfrproxy retries once, then reroutes to the fallback. Fallbacks are followed **transitively** (A → B → C, cycle-safe, up to 3 hops):

```bash
cfrproxy provider edit --name openrouter --fallback local/qwen2.5:7b
```

When a failover fires, the response leads with a visible notice so you know the model changed:

> ⚠️ [cfrproxy] openrouter unavailable — failed over to local/qwen2.5:7b (…)

4xx auth/validation errors never fail over (they'd fail identically anywhere). The Live Traces dashboard records every failover.

## Per-provider virtual mounts

Each provider is also addressable on its own at `/p/<name>/v1/...`:

- `/p/openrouter/v1/models` — only that provider's models, as bare ids
- `/p/openrouter/v1/chat/completions` — forces routing to that provider

This is what lets tools that only support a flat provider→model picker still drill down router → provider → model.

## Transforms

Declarative JSON rules rewrite the request sent to a provider, or the response returned to the consumer — scoped per provider and/or inbound dialect. Ops: `set`, `default`, `rename`, `delete`, on dot-paths.

```bash
# pin temperature on one provider's outbound requests
cfrproxy transform add --name pin-temp --phase request --provider mygateway \
  --rules '[{"op":"set","path":"temperature","value":0.2}]'
```

Everything here is also editable in the WebUI with no restart.
