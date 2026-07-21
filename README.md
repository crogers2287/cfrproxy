# cfrproxy

**One endpoint for every LLM you use.** Point Claude Code, Codex, OpenCode, omp, or any OpenAI/Anthropic/Ollama-speaking tool at cfrproxy, and route it to *any* provider behind the scenes — cloud APIs, OAuth subscriptions, or local models — with automatic failover, task-based auto-routing, and a live dashboard.

[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE) ![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go) ![single binary](https://img.shields.io/badge/deploy-single%20binary-blue)

---

Ever wanted to use your Claude subscription in Codex? Or route your coding agent to a local model for cheap tasks and a frontier model for hard ones — without touching the agent's config every time? Or just *see* which model is burning your tokens?

cfrproxy is the generic version of what `ollama launch claude --model glm-5.2:cloud` does for one tool: it sits between your harnesses and your providers, speaks every dialect, and translates on the fly. It's a **single Go binary** — no Python, no compose stack, no dependencies.

```
   Claude Code ┐                          ┌ OpenAI / OpenRouter / xAI …
   Codex       ┤                          ┤ Anthropic
   OpenCode    ┼──▶  cfrproxy  ──▶ route  ┼ Ollama (local)
   omp         ┤     :8420                 ┤ OAuth subs (Claude/Codex/Grok…)
   your app    ┘   translate + fail over   └ any OpenAI-compatible endpoint
```

## What it does

- **Speaks every dialect both ways.** Inbound OpenAI (`/v1/chat/completions`), Anthropic (`/v1/messages`), or Ollama (`/api/chat`) — translated to whatever the target provider wants, including streaming and tool calls. Your Anthropic-only tool can talk to an OpenAI model and vice-versa.
- **🔀 Auto-router.** Send the model `auto` and a small classifier buckets each request (code / reasoning / quick / long / vision) and delegates to the model *you* mapped for that task. `auto-plan` adds a planning stage that briefs the executor first.
- **♻️ Failover chains.** Give a provider a fallback; on a timeout or 5xx cfrproxy retries once, then transparently reroutes down the chain — with a visible ⚠️ notice injected into the response so you know the model changed.
- **📊 Live dashboard.** A built-in WebUI (and TUI) shows every request in real time: which model, token burn, **cache-hit %**, latency, and auto-route decisions — per model.
- **🔑 OAuth subscriptions as providers.** Bring your Claude, Codex, Grok/SuperGrok, Gemini, and Kimi *subscriptions* in as models (via [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)) — interactive login right in the WebUI.
- **🗜️ Context compression** (opt-in): summarize old conversation turns to cut token bills on long chats, cached per prefix, fail-open.
- **🧠 Round-table consensus MCP.** Define agent *profiles* (a persona pinned to a model) and let a panel of different models deliberate a question, cross-critique, and synthesize — exposed as an MCP tool any agent can call.
- **Declarative transforms, model pinning, docs injection**, and more — all editable in the UI, no code.

Everything is managed from the WebUI or CLI; the only state is one SQLite file. API keys are **AES-256-GCM encrypted at rest** and never logged.

## Quickstart

```bash
go build -o cfrproxy .

# add a provider (any OpenAI-compatible endpoint works)
./cfrproxy provider add --name openrouter --preset openrouter --key sk-or-... --model anthropic/claude-sonnet-4
./cfrproxy provider add --name local --preset ollama --model qwen2.5:7b

# run it — prints a generated WebUI password on first launch
./cfrproxy serve
#   data plane : /v1/chat/completions  /v1/messages  /api/chat
#   webui      : http://localhost:8420/admin/
```

Point a harness at it:

| Harness | How |
|---|---|
| **Claude Code** | `ANTHROPIC_BASE_URL=http://localhost:8420` — or just `cfrproxy claude` |
| **Codex / OpenCode / OpenAI-compatible** | base URL `http://localhost:8420/v1` |
| **Ollama-native tools** | `OLLAMA_HOST=http://localhost:8420` |

Or launch any harness through cfrproxy directly (`ollama launch`-style), and it inherits every model:

```bash
cfrproxy claude --model openrouter/anthropic/claude-sonnet-4
cfrproxy codex  --model local/qwen2.5:7b
cfrproxy opencode          # any binary on PATH
```

## Model routing, in plain terms

A model name tells cfrproxy where to send the request:

- `openrouter/gpt-4o` — provider `openrouter`, model `gpt-4o`. Both halves are fuzzy-matched, so `openrouter/GPT4o` or a partial name usually resolves.
- `auto` / `auto-plan` — let the auto-router pick (see [docs/auto-router.md](docs/auto-router.md)).
- A bare name — matched against provider aliases, then every provider's live model list.
- Anything unrecognized — the highest-priority enabled provider's default model, so nothing hard-errors.

You can also **map** fixed harness names (`cfrproxy map 'claude-sonnet*' openrouter/anthropic/claude-sonnet-4`) so Claude Code's built-in Opus/Sonnet/Haiku picker becomes switchable slots — the same trick `ollama launch` uses.

## Docs

| Guide | What's inside |
|---|---|
| [docs/architecture.md](docs/architecture.md) | How a request flows through the proxy; the wire-translation layer |
| [docs/providers.md](docs/providers.md) | Provider types, model pinning, fallback chains, transforms |
| [docs/auto-router.md](docs/auto-router.md) | Task classification, planning stage, per-bucket model mapping |
| [docs/oauth.md](docs/oauth.md) | Bringing Claude/Codex/Grok/Gemini/Kimi subscriptions in via OAuth |
| [docs/roundtable.md](docs/roundtable.md) | Agent profiles + the consensus MCP server |
| [docs/compression.md](docs/compression.md) | Context compression: how it works and when to use it |
| [docs/deployment.md](docs/deployment.md) | systemd, exposing it publicly (safely), the API-key gate |
| [HERMES_INTEGRATION.md](HERMES_INTEGRATION.md) | Optional: wiring cfrproxy into the Hermes agent platform |

## CLI reference

```
cfrproxy serve      run the proxy + WebUI
cfrproxy tui        full-screen management console
cfrproxy provider   add | list | rm | edit
cfrproxy route      show / set routing priority
cfrproxy map        map harness model names to providers
cfrproxy models     scan providers' live model lists
cfrproxy test       send a test prompt to a provider
cfrproxy logs       tail request traces
cfrproxy transform  add/edit declarative request/response rewrites
cfrproxy login      OAuth login (codex | claude | supergrok | antigravity | kimi)
cfrproxy mcp        round-table consensus MCP server (stdio)
cfrproxy <harness>  launch a harness through the proxy (claude, codex, opencode, …)
```

## Security notes

- API keys are encrypted at rest (AES-256-GCM; key in a `0600` file beside the DB) and never logged.
- The WebUI and management API are behind HTTP basic auth.
- The data plane is **keyless on your LAN** (so local tools just work) but **requires an API key when reached through a reverse proxy** — see [docs/deployment.md](docs/deployment.md) before exposing it to the internet.

## License

MIT — see [LICENSE](LICENSE).

> Built as a personal homelab tool and shared in case it's useful. Contributions and issues welcome.
