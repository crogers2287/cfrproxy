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

Two chains exist. The **global fallback chain** (WebUI → Providers → *Auto fallback chain*, setting `global_fallback`) applies to every request and is the one to configure first. A provider can additionally name its **own** fallback (`provider/model`); that per-provider hop is **off by default** because it is invisible in the chain the UI shows and once quietly billed a paid provider for every local request during an outage. Enable it with the *Also honour each provider's own fallback field* checkbox under the auto fallback chain (setting `provider_fallback`). When on: on a transient error — connection failure, timeout, 408/429/5xx — cfrproxy retries once, then reroutes to the fallback. Fallbacks are followed **transitively** (A → B → C, cycle-safe, up to 3 hops):

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

## Thinking level

`reasoning_effort` on a provider (`off|low|medium|high|xhigh`, UI "Thinking level", CLI
`--reasoning`) is the reasoning effort sent to that provider's models when the client did not
choose one; `reasoning_force` (`--reasoning-force`) applies it even when the client did. A share
endpoint's own level wins over the provider's. Why it exists: a request with no reasoning field is
not "thinking off" — the model's chat template decides, and the Qwen3.8 family resolves
`reasoning_effort|default('xhigh')`, so an agent harness that never sets a level runs every turn at
the most expensive setting. Per dialect the proxy writes `reasoning_effort` (OpenAI-compatible,
which llama.cpp forwards to the template), `reasoning.effort` (Responses), `thinking` with a budget
kept under `max_tokens` (Anthropic) or `think` (Ollama). `off` on an OpenAI-compatible provider
becomes `chat_template_kwargs.enable_thinking=false`, which only llama.cpp / vLLM style servers
accept — do not set `off` on a cloud provider. `cfrproxy explain <model> --endpoint <share>` prints
the resolved level and where it came from; the trace note carries `thinking=<level>` when applied.
