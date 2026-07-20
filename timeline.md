# cfrproxy timeline

## 2026-07-20

### REQ-011 — 3-tier model selector: router → provider → model

Source: chat ("subset of providers inside the model selector … select the router, then the provider, then the model from that provider")

Explore finding: Hermes picker supports group→provider→model, but the group tier comes ONLY from hardcoded `hermes_cli/models.PROVIDER_GROUPS` (slug-membership table; no config/naming/family mechanism). So needs distinct provider rows + a PROVIDER_GROUPS entry.

| Item | Status | Evidence |
|---|---|---|
| 1. cfrproxy per-provider scoped mounts | 🟡 built | `/p/{provider}/v1/chat/completions` `/v1/models` `/v1/messages` `/api/chat` `/api/tags`; forces provider, bare model ids; `TestScopedProviderMount`; live: /p/fred 20, /p/ollama 14, /p/oauth 130. Commit 7fa6696 |
| 2. Provider tier in Hermes | 🟡 done | one custom provider `cfrproxy-<name>` per backend → scoped mount; picker shows fred(20)/ollama(14)/Nexum(17)/oauth(130) rows w/ live counts |
| 3. Router tier (group) | 🟡 done | `sync_hermes_cfrproxy.py` patches PROVIDER_GROUPS['cfrproxy'] (marker block before _SLUG_TO_GROUP); `group_providers` folds the 4 into one 'cfrproxy' group row; models.py imports clean |
| 4. Dynamic | 🟡 done | sync re-reads cfrproxy admin API; models within a provider always live; provider add/remove = one sync re-run. Ran across all 14 profiles+subprofiles; 7 gateways restarted clean |

Non-repo: `~/.hermes/profiles/*/config.yaml` (cfrproxy-* providers), `~/.hermes/hermes-agent/hermes_cli/models.py` (PROVIDER_GROUPS patch, marker-delimited, backup written). Commits 7fa6696, 5f40c45.

**REQ-011 status: COMPLETE.**

### REQ-010 — Dynamic model selector in Telegram for all Hermes agents + OAuth logins (Codex/Anthropic/SuperGrok)

Source: chat ("dynamic model selector inside of telegram for all of our Hermes agents … updates every time we add/remove models from the proxy" + "log in with our oauth device codes from codex, anthropic etc … route through an anthropic oauth proxy" + "supergrok as well")

Key finding (Explore agent): Hermes ALREADY has `/model` with a live-fetching inline-keyboard picker (`gateway/slash_commands.py:_handle_model_command` → `model_switch.list_picker_providers` → live `GET /v1/models`; custom providers with an api_key get their catalog replaced by live discovery). So the integration is config, not code.

| Item | Status | Evidence |
|---|---|---|
| 1. Dynamic selector, all agents | 🟡 done | cfrproxy injected as one custom provider in all 7 profile config.yaml (ash/canna/fogger/grant/haxor/max/winston; backups written); `list_picker_providers` for haxor shows cfrproxy row w/ 183 live models; gateways restarted clean |
| 2. Auto-updates on proxy change | 🟡 done | Hermes live-fetches cfrproxy `/v1/models`; 15-min stale-while-revalidate cache + `/model --refresh`; verified `fetch_api_models` → 183 |
| 3. OAuth logins (Codex device/Anthropic/etc.) | 🟡 done | `cfrproxy login codex|codex-device|claude|antigravity|kimi|supergrok` → CLIProxyAPI flows; CLIProxyAPI (:8317) registered as cfrproxy provider `oauth` (130 models) |
| 4. Anthropic OAuth routing preserved | 🟡 verified | `oauth/claude-sonnet-5` (anthropic dialect in) → pong |
| 5. SuperGrok | 🟡 verified | `-xai-login` wired; `oauth/claude-command-grok-4.5` → pong |

Non-repo changes: `~/.hermes/profiles/*/config.yaml` (cfrproxy custom provider), cfrproxy `oauth` provider in ~/.cfrproxy DB. Commits c0f94f8, 1257393; inject script + HERMES_INTEGRATION.md in repo.

**REQ-010 status: COMPLETE.**

### REQ-009 — Visible alert in chat when failover changes the model

Source: chat ("if we get routed to fall back can we have it flagged in the chat/toolcalls so we realize if the model changes like an alert?")

| Item | Status | Evidence |
|---|---|---|
| 1. Alert injected into response content | 🟡 built | "⚠️ [cfrproxy] X unavailable — failed over to Y/model (reason)" — first text_delta when streaming, content prefix otherwise; failover candidates skip passthrough so injection always possible; reason truncated to 160 chars |
| 2. Verified | 🟡 pass | TestFailover asserts alert in body; live deadtest: alert present in both non-streaming content and first streamed delta before fred's "pong" |

Commit d5cb29e; service restarted.

**REQ-009 status: COMPLETE.**

### REQ-008 — Nexum Qwen upstream timeouts killing omp runs

Source: phone screenshot (omp: "Qwen upstream request timed out … Retry failed after 3 attempts") + "the nexum API is getting hammered because of the new Qwen model"

| Item | Status | Evidence |
|---|---|---|
| 1. Per-provider fallback | 🟡 built | `fallback` column (+migration); candidate loop: 1 retry w/ 1.2s backoff on transient (transport, 408/429/5xx/524/529) then failover before any bytes reach client; 4xx auth/validation never fail over; surfaced in CLI/TUI/WebUI |
| 2. Nexum protected | 🟡 configured | `Nexum.fallback = fred/agents-a1` |
| 3. omp routed through proxy | 🟡 wired | `~/.omp/agent/models.yml` dialagram baseUrl → `http://127.0.0.1:8420/v1` (original URL preserved in comment; home-repo file, not committed here); bare `qwen-3.8-max-preview-thinking` verified → Nexum 200 |
| 4. Tests | 🟡 pass | `TestFailover` (503→retry→backup, trace notes failover), `TestNoFailoverOn401`; live proof: deadtest provider → "failover from deadtest (connection refused)" → fred answered pong |

Commit dc609c5; service restarted.

**REQ-008 status: COMPLETE.**

### REQ-007 — Claude Code → fred/agents-a1 400: "System message must be at the beginning"

Source: phone screenshot (API Error 400 provider fred, Jinja exception from agents-a1 chat template)

Root cause chain: Claude Code's Agent SDK injects **system-role messages mid-conversation** (SessionStart hook output). The anthropic parser passed the role through verbatim → outbound `[system, user, system]` → agents-a1's custom template hard-raises during llama.cpp parser generation → 400. Synthetic repros (stream/tools/history permutations) all passed; found by registering a capture provider and recording the exact outbound body from a real `claude -p` run (153KB, 53 tools, roles `[system, user, system]`).

| Item | Status | Evidence |
|---|---|---|
| 1. Mid-conversation system messages | 🟡 fixed | Anthropic parser folds them into top-level system (matches openai parser); `TestAnthropicMidConversationSystem` |
| 2. End-to-end | 🟡 verified | `cfrproxy claude --model fred/agents-a1 -p "Reply with the single word: pong"` → `pong`; trace: fred 200, 15.9s stream |

Commit c66014c; service restarted; capture provider removed.

**REQ-007 status: COMPLETE.**

### REQ-006 — fred traffic missing from live traces + refresh loses the traces tab

Source: chat ("I have the agents model selected with Fred as the provider, but it's not showing up in the live traces … when I refresh … takes me back to the main page")

| Item | Status | Evidence |
|---|---|---|
| 1. fred requests not in traces | 🟡 fixed | Root cause: fred provider was DISABLED (accidental card click); `fred/agents-a1` silently fell back to next enabled provider (trace 21: routed to ollama). Re-enabled fred; live curl routes to fred again (143ms) |
| 2. Silent fallback masked the misconfig | 🟡 fixed | Explicitly-addressed disabled provider now 503s with "provider X is disabled — enable it…" (verified with scratch provider) |
| 3. Refresh returns to main page | 🟡 fixed | Active tab now in URL hash (#traces); Playwright: after reload active tab=traces, 24 rows rendered |

SSE itself was healthy — traffic was flowing all along, just attributed to other providers. Commit df9eafe; service restarted.

**REQ-006 status: COMPLETE.**

### REQ-005 — Claude's /model picker only shows Claude models

Source: chat ("when launching Claude via the proxy it only shows Claude models. no option for cfrproxy models")

Root cause: Claude Code's /model window is a hardcoded preset list — it never queries the API for models (ollama launch has the same limitation and solves it by rewriting inbound model names).

| Item | Status | Evidence |
|---|---|---|
| 1. Model map (harness names → provider/model) | 🟡 built | settings-backed map, exact + trailing-`*` patterns; `cfrproxy map`, `GET/PUT /admin/api/modelmap`, WebUI editor on Providers tab |
| 2. Claude presets as switchable slots | 🟡 seeded | `claude-opus*`→Nexum/qwen-3.8, `claude-sonnet*`→fred/agents-a1, `claude-haiku*`→ollama/qwen2.5:7b; live: `claude-haiku-4-5-20251001` → answered by qwen2.5:7b |
| 3. Server-side fuzzy resolution for typed /model strings | 🟡 built | `ResolveModel` (map → case-fold provider prefix + FuzzyModel vs live scan → alias → cross-provider fuzzy → active default); live: typed `nexum/Qwen3.8` → qwen-3.8-max-preview-thinking |
| 4. Unknown models never error | 🟡 built | fallback = top enabled provider's default model |

Verify: `go test ./...` 5 packages ok; two live curls above. Commit (this). Service restarted.

**REQ-005 status: COMPLETE.**

### REQ-004 — Model scanning + ollama-launch-style harness commands

Source: chat ("scan the models in point for models that we can select" + "add in the launch commands like the way that ollama does it … cfrproxy claude --model nexum/Qwen3.8 … change models on the fly via the /model window")

| Item | Status | Evidence |
|---|---|---|
| 1. Scan provider endpoints for selectable models | 🟡 built | `ListModels` (openai+anthropic `/v1/models`, ollama `/api/tags`), 60s cache; live CLI scan: fred 20, ollama 14, Nexum 19 |
| 2. Expose scanned models to harness pickers | 🟡 built | data-plane `/v1/models` + `/api/tags` return 53 `provider/model` IDs; WebUI provider form gets Scan-models button + datalist; `GET /admin/api/providers/{id}/models` |
| 3. Launch commands (`cfrproxy claude/codex/opencode/omp/...`) | 🟡 built | `launch.go`: any binary on PATH; consumes `--model/--addr`, forwards the rest; execs with ANTHROPIC_*/OPENAI_*/OLLAMA_HOST → proxy; codex gets `-m`; health-check first |
| 4. Fuzzy model resolution | 🟡 built | exact > case-fold > unique substring > punctuation-blind; `nexum/Qwen3.8` → `Nexum/qwen-3.8-max-preview-thinking`; `TestFuzzyModel` table |

Verify: fake harness dump showed all env correct + arg passthrough + only-if-unset key handling. Real end-to-end: `cfrproxy claude --model nexum/Qwen3.8 -p "Reply with the single word: pong"` → launched actual Claude Code CLI → proxy → Nexum → `pong`. `go test ./...` 5 packages ok. Commit 353b8e3; service restarted.

**REQ-004 status: COMPLETE.**

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
