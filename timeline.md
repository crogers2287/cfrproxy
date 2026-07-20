# cfrproxy timeline

## 2026-07-20

### REQ-003 — Auto-try conventional URL variants when adding a provider

Source: chat ("fix it so that if we add a URL, it auto tries a couple different options based on conventional formatting")

| Item | Status | Evidence |
|---|---|---|
| 1. Normalize user-entered URLs | 🟡 built | `NormalizeBase`: scheme inference (http for local/private hosts), trim, strip pasted endpoint paths; table-driven test |
| 2. Probe conventional variants on save | 🟡 built | `DiscoverBase`: 1-token probe, ≠404/405 = exists; `/api/v1` candidate for openai routers; wired into API create/update, CLI add/edit, TUI form; WebUI toasts the resolution |

Verify: `go test ./...` 4 packages ok (4 new discovery tests incl. httptest mock router). Live: `"openrouter.ai"` → resolved `https://openrouter.ai/api/v1` (HTTP 401 = exists); full endpoint paste `https://dialagram.me/router/v1/chat/completions` → stripped to `.../router/v1`. Commit ae7aa42; service restarted.

**REQ-003 status: COMPLETE.**

### REQ-002 — Nexum provider test failed from WebUI

Source: chat ("I tried testing adding a provider  the test failed. test nexum")

| Item | Status | Evidence |
|---|---|---|
| 1. Nexum test 404 | 🟡 fixed | Base `https://dialagram.me/router/v1` + proxy's `/v1/chat/completions` = double `/v1` → 404. Path join now strips the duplicate version segment when base ends in `/v1` (or `/api` for ollama). Commit 87a4f7a. |

Verify: test endpoint → `{"ok":true,"content":"pong","model":"qwen-3.8-max-preview-thinking"}`; cross-dialect `/v1/messages` → `Nexum/...` returned `pong` with correct anthropic framing. Service restarted via systemd.

**REQ-002 status: COMPLETE.**

### REQ-001 — Build universal LLM proxy (provider registry, router, transforms, TUI/CLI, WebUI, Ollama-first)

Source: Governed Fable Prompt pasted in session (universal proxy replicating `ollama launch <harness> --model <m>` generically). Operator decisions: Go single binary, SQLite ("whichever is faster"), basic auth.

#### Items

| Item | Status | Evidence |
|---|---|---|
| 1. Provider registry (any provider, base URL, encrypted key, docs ref) | 🟡 built | `internal/store`; `TestKeyEncryptionAtRest` proves AES-GCM at rest + empty-key-keeps-key; fred + ollama registered live |
| 2. Request router (normalized schema → provider wire format → normalize back) | 🟡 built | `internal/wire` + `internal/proxy`; live: anthropic→openai (fred), openai→ollama, ollama→openai all returned "pong" |
| 3. Output transformer pipeline (declarative, per-provider/per-target) | 🟡 built | `internal/transform`; live: `tag-response` rule visibly added `proxy_tag` to response; `pin-temp` scoped to fred |
| 4. TUI/CLI full management | 🟡 built | CLI: provider/route/test/logs/transform/passwd all exercised live; TUI rendered under `script` pseudo-tty, exit 0 |
| 5. WebUI (card grid, drag reorder, docs panel, live trace, transform editor) | 🟡 built | `internal/api` + embedded `webui/index.html`; Playwright screenshots of all 4 tabs; SSE trace event received live; 401 without creds |
| 6. Ollama first-class (streaming + non-streaming, transformed to any harness) | 🟡 built | qwen2.5:7b via /v1/chat/completions (stream + non-stream) and /v1/messages; NDJSON out for /api/chat consumers |

#### Files
- `main.go` — CLI (serve/tui/provider/route/test/logs/transform/passwd), presets
- `internal/store/` — SQLite WAL + AES-256-GCM key encryption + registry cache (+ `data_version` cross-process invalidation)
- `internal/wire/` — normal form + openai/anthropic/ollama translators incl. stream re-framing + tool calls
- `internal/transform/` — declarative rule engine (set/default/rename/delete)
- `internal/proxy/` — inbound endpoints, routing, passthrough fast path, traces, SSE hub
- `internal/api/` — management REST + basic auth + SSE + embedded WebUI (`webui/index.html`)
- `internal/tui/` — Bubble Tea console (providers/transforms/logs/test)

#### Verify
- `go test ./...` — 3 packages ok (store, transform, wire; 12 tests incl. stream re-framing both directions)
- Live smoke on 127.0.0.1:8420 against fred:9069 (llama-swap agents-a1) and local Ollama 0.21.2 (qwen2.5:7b): passthrough raw-bytes proof (llama-swap `timings` field intact), cross-dialect streaming (anthropic SSE + ollama NDJSON), tool-call translation (get_weather → tool_use block), docs injection ("zanzibar" test), basic auth 401/200, SSE live trace, key-redaction in API responses
- Found+fixed during verify: server registry cache missed CLI writes from a second process → `PRAGMA data_version` check in `Providers()`

**REQ-001 status: COMPLETE.**
