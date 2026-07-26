# cfrproxy timeline

## 2026-07-26

### REQ-028 — Qwen provider shows no models in the Telegram picker (scoped-mount name matching)

Source: chat ("I have Qwen loaded as a provider, but the models are not loading dynamically inside of the telegram model selector. It shows the provider as an option, but it does not have any models available under CFRproxy/cfrproxy-qwen")

#### Root cause
The provider was created as `"Qwen "` (**trailing space**), `sync_hermes_cfrproxy.py` baked that verbatim into every Hermes profile as `base_url: http://127.0.0.1:8420/p/Qwen /v1`, and the provider was later renamed to clean `"Qwen"`. Two independent bugs then kept the stale URL broken:

1. **`store.ProviderByName` was case/space-exact** (`p.Name == name`) while the rest of the codebase compares provider names with `EqualFold`. `/p/Qwen%20/v1/models` → no match → `scopedModelIDs` returned `nil` → **empty model list** = the reported symptom. The `DefaultModel` fallback below it never ran because the function bailed before reaching it.
2. **Worse, and silent: the chat path misrouted.** `handleCore` builds `reqModel = scope + "/" + model` → `"Qwen /qwen3.7-max"`; `ResolveModel`'s `EqualFold` prefix match failed, fell through the alias and fuzzy stages, and landed on rule 5 — *highest-priority enabled provider + its default model*. Qwen requests were being answered by **`fred` (local llama.cpp gguf)** with no error anywhere.

The sync script's `--auto` guard could never self-heal it: the state signature was `n.strip().lower()`, so `"Qwen "` and `"Qwen"` hashed identically and the rename never triggered a re-sync.

| Item | Status | Evidence |
|---|---|---|
| Scoped listing returns models for any spelling | 🟡 fixed | `ProviderByName`: exact match first, then trimmed + `EqualFold` fallback. Live: `/p/Qwen`, `/p/qwen`, `/p/Qwen%20`, `/p/QWEN` all return **8 models** |
| Scoped chat routes to the addressed provider | 🟡 fixed | `handle()` canonicalises the path segment to the stored provider name before `handleCore`. Live traces: all four spellings → `Qwen/qwen3.7-max [200]` (was `fred/qwen36-27b-vl`) |
| Unknown mount is loud, not a silent reroute | 🟡 fixed | `/p/nosuchprovider/...` → **HTTP 404** `unknown provider mount: nosuchprovider` (previously answered 200 from an unrelated provider) |
| Bad names can't be stored | 🟡 fixed | `SaveProvider` trims `Name`/`BaseURL` |
| Sync script can't re-bake the bad URL | 🟡 fixed | `hermes_entry()` strips the name; state signature is case-preserving so a case-only rename re-syncs |
| Regression tests | ✅ | `TestProviderByNameIsTrimmedAndCaseInsensitive`, `TestProviderByNameExactMatchWins`, `TestSaveProviderTrimsName`, `TestScopedMountNameIsCanonicalised`. **Verified non-vacuous**: reverting the `handle()` fix fails the suite with `/p/Qwen%20 chat was answered by the wrong provider: from-decoy` — the decoy is deliberately the higher-priority fallback so the test reproduces the production misroute, not just the 404 |

#### Files modified
- `internal/store/store.go` — `ProviderByName` exact→loose lookup; `SaveProvider` trims name/base_url
- `internal/proxy/proxy.go` — `handle()` canonicalises the `/p/{provider}` segment, 404s unknown mounts
- `internal/store/store_test.go`, `internal/proxy/proxy_test.go` — regression tests
- `scripts/sync_hermes_cfrproxy.py` — trim name in `hermes_entry()`; case-preserving state signature

#### Deploy + verify
- `go vet ./...` clean, `go test ./...` all packages pass
- rebuilt `./cfrproxy` + `~/.local/bin/cfrproxy`, `systemctl --user restart cfrproxy.service` → active
- No Hermes re-sync was required: the code fix makes the existing stale `/p/Qwen /v1` configs work as-is. Hermes caches held no cfrproxy entries (it probes live), so the picker repopulates without a gateway restart.

**REQ-028 status: COMPLETE.**

### REQ-029 — qwen3.8-max-preview capped at 128k + failover banner spam

Source: chat ("yeah apply it. also, the failover is putting way too much garbage in every message. qwen failover message is repeated 4 times. we don't need that much detail just have it say something like ⚠️ failover: qwen3.8-max active") + Telegram screenshot of the Canna Marketing Group showing the banner stacked 4× in one message.

| Item | Status | Evidence |
|---|---|---|
| 1. 128k cap on `qwen3.8-max-preview` | 🟡 fixed (in Hermes, not cfrproxy) | Not a cfrproxy limit — compression is disabled and no such constant exists in the repo. `get_model_context_length` fell through to the catch-all `"qwen": 131072`. Added `"qwen3.8-max-preview": 1000000` in `~/.hermes/hermes-agent/agent/model_metadata.py`. Live endpoint **accepted a 300,065-token prompt (HTTP 200)**, confirming 131,072 was a client-side floor |
| 1b. …still showed 131k after 1 | 🟡 fixed | **Two more causes.** (a) **Wrong slug covered.** Nexum serves the model as `qwen-3.8-max-preview-thinking` — *dashed*. Lookup is longest-key-first **substring** match (`model_metadata.py:1530`), so the undashed key can't match the dashed spelling and it still hit the catch-all. The dashed slug is **absent from every models.dev catalog snapshot on the box** (Nexum's own spelling), so nothing else could supply it. Added `"qwen-3.8-max-preview": 1000000`, covering the `-thinking` suffix too. (b) **Stale in-memory module.** Gateways had been up since 12:48 UTC; Python imports the table once, so the 13:24 edit was invisible to them. Restarted `hermes-gateway-*` (13:25–13:26, after the edit) |
| Verified in real profile context | ✅ | Via the gateways' own `venv/bin/python` with `HERMES_HOME` set per profile — `fogger` and `canna` both resolve `qwen3.8-max-preview` **and** `qwen-3.8-max-preview-thinking` → **1,000,000**. Catch-all unharmed: `qwen-max`→131,072, `qwen3-coder`→262,144, `qwen3.5:27b`→131,072. No stale context string in `picker_output_cache.json` (it caches model lists only), so no cache clearing was needed |
| 2. Banner far too verbose | 🟡 fixed | Was `⚠️ [cfrproxy] grok unavailable — failed over to Qwen/qwen3.8-max-preview (grok: usage cap (HTTP 402) {"error":"Grok Build usage balance exhausted"})`. Now `⚠️ failover: qwen3.8-max-preview active` |
| 3. Repeated 4× per message | 🟡 fixed | Root cause: the banner is injected **per model call**, and a harness tool-loop makes several calls per user turn. New `failoverNotices` cache (`internal/proxy/globalfallback.go`) keyed on `conversationFingerprint + provider/model`, 10-min TTL, 4096-entry bound — announced once, then quiet. TTL means a still-down provider re-announces rather than failing over silently forever |
| No diagnostic loss | ✅ | Full reason still on `tr.Err` → Live Traces + WebUI errors panel: `failover from grok (grok: usage cap (HTTP 402) {"error":"Grok Build usage balance exhausted"})` |
| Regression tests | ✅ | `TestFailoverBannerAnnouncedOncePerConversation` (4-call tool loop → 1 banner; a *different* conversation still gets its own). `TestFailover` updated + now asserts the verbose form does **not** leak into content. **Verified non-vacuous**: disabling the dedupe fails with `got 4 copies` — reproducing the screenshot exactly |

#### Files modified
- `internal/proxy/proxy.go` — terse banner, gated on `noticeCache.announce(...)`
- `internal/proxy/globalfallback.go` — `failoverNotices` suppression cache
- `internal/proxy/proxy_test.go` — dedupe test, `resetFailoverNotices` helper, updated assertions
- `~/.hermes/hermes-agent/agent/model_metadata.py` (**outside this repo**) — `qwen3.8-max-preview` context entry

#### Deploy + verify
- `go vet ./...` clean, `go test ./...` all packages pass; rebuilt + `systemctl --user restart cfrproxy.service`
- Live: 4 calls of one conversation through the exhausted `grok` → failover to `Qwen/qwen3.8-max-preview`, banner on call 1 only, calls 2–4 clean

**REQ-029 status: COMPLETE.**

#### Pre-existing, untouched
`gofmt -l` flags `internal/store/store.go` (field alignment in the `Trace` struct), plus `internal/api/api.go`, `internal/tui/tui.go`, `internal/wire/{anthropic,ollama,openai}.go`, `launch_test.go`. Unrelated to REQ-028/029 — not reformatted, to keep those diffs clean.

## 2026-07-23

### REQ-027 — Auto-router without destroying cache hits (+ 402 failover fix)

Source: chat ("how can we use the autorouter without raping the cache hit?")

#### Why the auto-router was cache-hostile
Prompt caching is per (provider, model, prefix) and matches from token 0. Two behaviours broke it:
1. **`auto-plan` folded the plan into `req.System`** — the plan text differs every turn, so the HEAD of the prefix changed each request → 100% miss on the entire (100k-token) prefix, every turn.
2. **Per-turn re-classification** — turn 1 → grok, turn 2 → claude means each turn hits a model that never saw the prefix → cold every time.
(The classifier itself was already cheap: last user message only, 2000-char cap. Not a problem.)

| Item | Status | Evidence |
|---|---|---|
| Sticky routing (conversation affinity) | 🟡 built | `internal/proxy/routesticky.go`: `conversationFingerprint` = sha256(system + first user msg) — stable as a conversation grows; `stickyRoutes` TTL map (default 30 min, 4096-entry bound). Pin on first successful classification, reuse after — which also **skips the classifier call** on later turns (cheaper + faster). Shows as `·sticky` in Live Traces |
| Default ON for existing configs | 🟡 built | `Sticky *bool` — nil (absent in stored JSON) = on; unit-tested |
| Plan briefing moved out of the prefix | 🟡 fixed | `appendBriefing()` attaches to the **final user message** instead of `system`, so everything before it stays byte-identical (falls back to system only if there's no user turn). Unit-tested that system + earlier turns are untouched |
| UI | 🟡 built | Auto router panel: "Sticky routing" checkbox + "Sticky window (minutes)" + hint explaining the cache cost |
| **Measured end-to-end** | ✅ | one conversation, 3 turns through `auto` → same model each turn, cache **0% → 91% → 98%** (turn 3 `auto→default·sticky→claude/claude-sonnet-5`) |

#### Bonus finding — 402 was hard-failing instead of failing over
The live test surfaced that **grok returns HTTP 402 `{"error":"Grok Build usage balance exhausted"}`** and **Deepseek returns 402 `"Insufficient Balance"`** — both accounts are out of balance. Neither string matched `usageExhausted`, and 402 isn't in `transientStatus`, so requests **hard-failed instead of using the chain**.
- Fixed status-based rather than by string whack-a-mole: **any 402 (Payment Required) = exhausted account → fail over**. Also added "balance exhausted", "usage balance", "out of credits" for providers that send it as 4xx/429.
- Verified: `grok/grok-4.5` now returns 200 via `codex/gpt-5.6-terra` with `failover from grok (usage cap HTTP 402)`.
- Unit test pins all four real production bodies (xai, deepseek, z.ai, anthropic) plus negative cases.

#### Files modified
- new `internal/proxy/routesticky.go` — sticky cache, fingerprint, `appendBriefing`, config helpers
- `internal/proxy/autoroute.go` — `Sticky`/`StickyTTLMinutes` config; sticky lookup before classify; pin after classify
- `internal/proxy/proxy.go` — `appendBriefing` instead of system mutation; 402 → failover; extra exhaustion strings
- `internal/api/webui/index.html` — sticky checkbox + TTL field + hint; load/save wiring

#### Operational note
Deepseek and grok (xAI Build) balances are **exhausted** — that's the "running out of tokens" being hit. cfrproxy now routes around both automatically; recharge if you want them back in rotation.

**REQ-027 status: COMPLETE.**

### REQ-026 — Global auto-fallback chain (UI) + prompt-cache verification

Source: chat ("when I run enough tokens on one proxy setup, I wanna be able to have a fallback chain… regardless of what model we're using, if it runs out of tokens or errors out, it will fall back… Need this in the UI") and ("make sure we are hitting the api cache as much as humanly possible, verify that's in the code")

#### Part A — global auto-fallback chain

Per-provider `fallback` only covered providers wired by hand. Added a **global** ordered chain that applies to every request, tried after the addressed provider's own chain.

| Item | Status | Evidence |
|---|---|---|
| Global chain in the data path | 🟡 built | `internal/proxy/globalfallback.go`: `GlobalFallback{enabled,targets}` from setting `global_fallback`; `appendGlobalFallback()` appends candidates in `handleCore` after the per-provider transitive chain |
| Dedupe per provider+**model** | 🟡 built | `pairKey(provID, model)` — models on one provider have independent quotas (opus capped while sonnet serves, REQ-019), so same-provider/different-model is a valid hop. Unit-tested |
| Triggers = "out of tokens or errors out" | 🟡 built | transient (408/429/5xx), usage/quota exhaustion, **context overflow** (new `contextExceeded()`), and connection errors. Genuine 4xx (bad request/auth/unknown model) deliberately excluded — they fail identically everywhere and would burn hops; unit-tested for false positives |
| Credit/balance exhaustion added | 🟡 fixed | test caught that z.ai `1113 "Insufficient balance… Please recharge."` (the REQ-023 error) wasn't matched → added "insufficient balance", "no resource package", "please recharge", "credit balance is too low", "insufficient credits" to `usageExhausted` so it fails over immediately instead of burning retries |
| Share-endpoint safety | 🟡 built | scoped endpoints are never routed past their model allow-list (`modelAllowed` check); `maxGlobalHops=5` caps latency |
| UI | 🟡 built | Providers tab → "Auto fallback chain": enable toggle, ordered target rows (model picker + ↑/↓/Del), Add/Save, live count pill; unknown/stale targets stay visible rather than silently dropped |
| API | 🟡 built | `GET/PUT /admin/api/global-fallback` via existing `settingJSON` helpers |
| **End-to-end verified** | ✅ | throwaway provider on a dead port, **no** per-provider fallback → request rescued by the chain: served `claude-sonnet-5`, `⚠️ [cfrproxy] fbtest unavailable — failed over…` injected, trace `failover from fbtest`; test provider deleted |

Chain currently set (during testing, editable in UI): `claude/claude-sonnet-5 → grok/grok-4.5 → codex/gpt-5.5`, enabled.

#### Part B — prompt-cache verification (asked to verify it's in the code)

Audit + **empirical** proof rather than code-reading alone:

| Finding | Evidence |
|---|---|
| Caching is working, ~86% fleet-wide | traces: passthrough 2256 reqs @ **86%** hit (120.5M in / 103.7M cached); translated 214 reqs @ **82%** |
| Translation does **not** cost cache | same 3k-token prompt twice on each path → openai(passthrough) **92%** and anthropic-inbound(translated) **92%** — identical, so the translated path is byte-stable and cacheable |
| No `cache_control` handling existed | grep: only HTTP `Cache-Control` headers; cache tokens were read for reporting only |
| …but it was **moot** today | all 13 enabled providers are `openai` (12) or `ollama` (1) — **zero** anthropic-dialect, and OpenAI-compat/deepseek/grok/glm do automatic server-side prefix caching (no markers needed). Claude goes via CLIProxyAPI as openai-compat and already shows ~93% cached |
| Real gap: `BuildAnthropicRequest` had zero cache support | 🟡 fixed — system now emitted as a block array with `cache_control:{"type":"ephemeral"}`. Anthropic's hierarchy is tools→system→messages, so one breakpoint on system covers tool definitions too. Short prompts (<4096 chars ≈1024 tok) stay a plain string since Anthropic ignores caching below that. Unit-tested both branches; forward-looking (fires when a direct `type: anthropic` provider is added) |
| No regression | post-deploy: both paths still 92%; full suite (5 packages) green |

#### Files modified
- new `internal/proxy/globalfallback.go` — config, `appendGlobalFallback`, `pairKey`, `contextExceeded`
- `internal/proxy/proxy.go` — global chain in candidate build; context-overflow failover branch; `usageExhausted` credit/balance strings; softErrs on cap/overflow
- `internal/api/api.go` — `global-fallback` GET/PUT + handlers
- `internal/api/webui/index.html` — "Auto fallback chain" panel + `renderGF/gfAdd/gfDel/gfMove/gfSet/loadGlobalFallback/saveGlobalFallback`; hooked into `fillAllModelSelects()` and `showTab('providers')`
- `internal/wire/anthropic.go` — `antBlock.CacheControl`, `cacheEphemeral`, `minCacheableChars`, system cache breakpoint

**REQ-026 status: COMPLETE.**

### REQ-025 — Director agent overflows q27b context ("cannot compress further")

Source: screenshot — @Director87bot (ash) on q27b/cfrproxy-fred, "Context: 256,000 tokens"; conversation went Context too large 134,782→compress→75,819→"⚠ Context length exceeded (75,819). Cannot compress further."

#### Root cause
cfrproxy-fred custom_provider entries had **no `context_length`**, so Hermes assumed a **256k default**. But fred is a local llama-swap server: q27b (Qwen3.6-27B, `--parallel 1`) has **n_ctx = 98,304**, and input + reserved output + MTP/vision overhead must fit — so a 75,819-token request already overflowed. Hermes let the convo balloon to 134k before compressing (thinking it had 256k), then couldn't get under the real window → hard fail. The sync script's `hermes_entry()` never set context_length, and it **rewrites these entries every run** (2-min auto-sync), so any manual edit would be wiped.

| Item | Status | Evidence |
|---|---|---|
| Find real context | ✅ | llama-swap `/upstream/q27b/props` → n_ctx=98304; run script `--parallel 1`, run-qwen36-27b-mtp-vision.sh |
| Set accurate context in sync | 🟡 fixed | `CONTEXT_LENGTHS = {"fred": 65536}` + `hermes_entry` emits `context_length` — survives the every-2-min re-sync. 65536 leaves safe headroom under 98304 (below the 75,819 failure point) |
| Apply to all profiles | ✅ | manual sync run (non-auto rewrites configs); cfrproxy-fred `context_length: 65536` verified in ash + haxor |
| Reload Director | ✅ | restarted `hermes-gateway-ash` (@Director87bot); active. ash default is grok-4.5 — q27b was a session switch |

#### Files modified
- `scripts/sync_hermes_cfrproxy.py` — `CONTEXT_LENGTHS` map + `hermes_entry` context_length

#### Bot→gateway map (via Telegram getMe, for reference)
ash=@Director87bot, canna=@Fred87bot, fogger=@Fogger87bot, grant=@Grantcfr_bot, haxor=@Haxor87bot, max=@mm87bot_bot, winston=@Winston87bot

**REQ-025 status: COMPLETE.** fred models now advertised at 65536; Hermes compresses at ~0.7×65536≈46k, staying under q27b's real 98304 window. Other 6 running gateways have the config staged; picked up on next restart.

#### Follow-up — user bumped q27b n_ctx to 131072
Confirmed via `/upstream/q27b/props` (n_ctx=131072). Raised `CONTEXT_LENGTHS["fred"]` 65536→**98304** (32k margin under 131072 for output + ~22k MTP/vision overhead). Re-synced all profiles, restarted ash. Compression now ~0.7×98304≈69k.

### REQ-024 — deepseek "reasoning_content must be passed back" errors

Source: chat + screenshot (errors panel showing `Deepseek/deepseek-v4-pro HTTP 400: "The reasoning_content in the thinking mode must be passed back to the API."`)

#### Root cause
deepseek-v4-pro is a reasoning model — its assistant replies include `reasoning_content`, which deepseek REQUIRES be passed back on the next turn. cfrproxy's wire schema had no `reasoning_content` field, so:
- **Passthrough** deepseek requests (2038/2051) preserved it (raw bytes) ✓
- **Translated** requests (11: failover + auto-route targets → forced translation) parsed messages into `wire.Msg`/`Delta`/`Response` which dropped `reasoning_content`. A translated *response* stripped it → harness stored an assistant turn without it → next turn omitted it → deepseek 400. Only 3 hard failures (0.15%); newly visible via the REQ-022 errors panel.

| Item | Status | Evidence |
|---|---|---|
| Preserve reasoning_content across translation | 🟡 fixed | added `ReasoningContent` to `wire.Msg`+`Response`, `Reasoning` to `wire.Delta`; openai dialect parses/builds it on request messages, response, and stream deltas (read+write) |
| Round-trip proven | ✅ | throwaway test: request msg, response, and stream delta all retain reasoning_content; existing wire tests still pass |
| No regression | ✅ | passthrough deepseek still 200; compile clean |

#### Files modified
- `internal/wire/types.go` — `Msg.ReasoningContent`, `Response.ReasoningContent`, `Delta.Reasoning`
- `internal/wire/openai.go` — `oaiMsg.reasoning_content` + parse/build; `oaiResp` message reasoning_content + parse; `BuildOpenAIResponse` emit; `ReadOpenAIStream`/`WriteOpenAIStream` delta preserve

#### Deploy
- both binaries rebuilt (temp+`mv`), service restarted; deepseek passthrough verified 200

Note: fix covers the openai↔openai path (deepseek's dialect, and what all failing traces used, inbound=openai). anthropic-inbound→deepseek would need thinking-block mapping — not implemented (no evidence of that path failing).

**REQ-024 status: COMPLETE.**

### REQ-023 — "grok rate-limited, not even showing it hitting the proxy" (Haxor agent)

Source: chat + screenshots (Haxor stuck on "model provider is rate-limiting", grok-4.5 switch not taking; "its not even showing it hitting the proxy")

#### Root cause — it was never grok, and never cfrproxy
Haxor gateway logs: the failing calls were `provider=zai base_url=https://api.z.ai/api/coding/paas/v4 model=glm-5.2` → **HTTP 429 code 1113 "Insufficient balance or no resource package. Please recharge."** — i.e.:
1. The failing model was **glm-5.2**, not grok. (grok-4.5 was the *new* main model; the switch was blocked by a stuck turn.)
2. glm-5.2 used a **Hermes-native `zai` provider that hit z.ai DIRECTLY**, bypassing cfrproxy → invisible in Live Traces (answers "not showing it hitting the proxy").
3. **Hermes's `ZAI_API_KEY` is out of balance**, but **cfrproxy's glm key is healthy** — proven: `/p/glm/v1` glm-5.2 → 200, glm-4.6 → 200 (1951ms, in traces), while direct z.ai → 429.
4. A pre-switch turn stuck retrying glm-5.2 (10× long backoffs, ~10min) blocked the grok-4.5 switch from taking effect.

| Item | Status | Evidence |
|---|---|---|
| Diagnose invisible failures | ✅ | gateway logs show direct-z.ai path; cfrproxy glm works, direct z.ai out of balance |
| Route glm through cfrproxy (all agents) | 🟡 fixed | repointed `providers.zai.base_url` `https://api.z.ai/api/coding/paas/v4` → `http://127.0.0.1:8420/p/glm/v1` in all 13 profiles (backed up `.bak-zaireroute-*`). glm now uses cfrproxy's healthy key + is visible + gets failover |
| Break the stuck Haxor turn | 🟡 fixed | restarted `hermes-gateway-haxor` → stuck glm turn killed (final 429 logged on shutdown), new pid clean @16:31:30; main model = grok-4.5 via cfrproxy |
| Apply reroute to other 12 agents | 🔴 pending | config changed but not yet loaded — needs gateway restart (deferred to avoid disrupting non-stuck live sessions) |

#### Files modified
- `~/.hermes/profiles/*/config.yaml` (×13) — `providers.zai.base_url` → cfrproxy glm mount

#### Note
Underlying z.ai account (`ZAI_API_KEY`) is out of balance — recharge if direct z.ai is wanted elsewhere. cfrproxy's glm key is on a working plan; centralizing through it is the fix.

#### Follow-up — real culprit was the haxor-**coder** sub-agent
The `zai` errors persisted after the reroute because the user was on the **haxor-coder** sub-agent (spawned inside the haxor gateway), whose OWN `model:` block hardcoded `provider: zai / default: glm-5.2 / base_url: https://api.z.ai/api/coding/paas/v4`. The `model.base_url` overrides the `providers.zai.base_url` reroute — so glm-5.2 kept hitting dead z.ai directly, and the main-bot `/model grok-4.5` switch never touched the sub-agent.
- Fix: set `haxor-coder/config.yaml` model block → `provider: custom:cfrproxy-grok / default: grok-4.5 / base_url: http://127.0.0.1:8420/p/grok/v1` (matches main haxor; cfrproxy-grok custom_provider confirmed present). Restarted gateway → stuck glm turn killed, coder now on grok via cfrproxy.
- Related: haxor-ops/research/reviewer default to `deepseek-v4-pro` via a `deepseek` provider — not yet verified as direct-vs-cfrproxy; left as-is.

**REQ-023 status: COMPLETE — haxor-coder now on grok via cfrproxy. Other 12 zai-reroutes staged (restart to apply); haxor-ops/research/reviewer deepseek path unverified.**

### REQ-022 — WebUI error logging (see exactly what an error is, e.g. grok rate limit)

Source: chat ("trying to use the proxy with grok4.5 but its telling me its being rate limited. we need to add logging to the webui so we can see when errors like this arise exactly what it is")

#### Findings
- grok-4.5 logs **200s** through the proxy — no rate-limit reached it as a hard error. Real 429s in the DB were **glm-5.2** ("service temporarily overloaded", z.ai code 1305) → 502 after retries.
- Errors were already fully captured (`snippetMax=2000`; err field complete) and shown on trace row-click, but scrolled away — poor discoverability.
- **Key gap:** transient 429/5xx that the proxy **retries and recovers** were logged only as the final 200 — a retried rate-limit was completely invisible. That's the likely grok case.

| Item | Status | Evidence |
|---|---|---|
| 1. Always-visible errors panel | 🟡 built | "⚠ Recent errors" panel atop Live Traces: full provider error text inline (time · provider/model · HTTP status · latency), live via SSE, seeded from history, count badge + Clear. Verified: forced glm bad-model → full "Unknown Model code 1211" rendered |
| 2. Surface retried/recovered transients | 🟡 built | proxy `handleCore`: `softErrs` accumulates transient (429/5xx/overload) errors hit during retry; on eventual same-provider success sets `tr.Err="recovered after transient: …"` → shows as amber **RETRIED · recovered** in the panel (distinct from red hard failures) |
| 3. No false positives | ✅ verified | clean grok request → 200 with empty err (not flagged); recovered/warn traces use `.warn` (amber), hard failures `.err` (red) |

#### Files modified
- `internal/api/webui/index.html` — errors panel HTML + `isErr`/`errRow`/`pushErr`/`clearErrs`; loadTraces seeds panel; SSE pushes live; `.warn` row style; traceRow warn-vs-err split
- `internal/proxy/proxy.go` — `softErrs` capture in candidate loop; recovered-transient note on successful trace

#### Deploy
- `go build` OK; both binaries redeployed (temp+`mv`); service restarted. Frontend pieces confirmed in served HTML.

**REQ-022 status: COMPLETE.** Next grok rate-limit through the proxy will appear in the panel — red if it hard-fails (502), amber "RETRIED" if it recovers. If it shows in *neither*, the rate-limit isn't traversing cfrproxy (client-side).

### REQ-021 — roundtable `consult` timing out ("context deadline exceeded while awaiting headers")

Source: screenshot — `mcp__roundtable__consult` Failed, profile Engineer, hard 14K-token llama.cpp perf question. Error: `Post "http://127.0.0.1:8420/v1/chat/completions": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`

#### Root cause
Engineer → `codex/gpt-5.6-terra` (reasoning model). `chatViaProxy` used one const `perCallTimeout=100s` for both roundtable panelists AND single consults. A reasoning model on a hard 14K-token prompt exceeds 100s before first byte (non-streamed → proxy holds the connection until the model finishes). Measured baseline: gpt-5.6-terra = 16s on a small prompt, so the model's fine — the 14K reasoning prompt just needs more than 100s. Proxy upstream timeout is 10min (not the limit).

| Item | Status | Evidence |
|---|---|---|
| 1. Separate consult vs panel timeout | 🟡 fixed | `chatViaProxy` gains a `timeout time.Duration` param. New `consultTimeout=300s` (single call, user waits, no panel to stall); roundtable panelists/critique/moderator/summarizer keep `perCallTimeout=100s` (one slow model can't stall the panel) |
| 2. Build + deploy | ✅ | `go build` OK; both binaries redeployed via temp+`mv` (over busy files); service + gateways restarted → gateway MCP procs respawned on new binary (verified pids @04:47 on current inode) |

#### Files modified
- `mcp.go` — `consultTimeout` const; `chatViaProxy(…, timeout, acc)` signature; 5 call sites (4 roundtable→perCallTimeout, 1 consult→consultTimeout)

#### Note
3 pre-existing MCP procs (user's active `claude` + `omp` sessions, pids 115614/116016/413587) still hold the old deleted binary (100s) until those sessions relaunch — not force-killed to avoid breaking live sessions. Hermes agents + any newly-started CLI use 300s now.

**REQ-021 status: COMPLETE.**

### REQ-020 — Telegram picker not auto-updating + "OAuth blocked" + add Ollama Cloud account

Source: chat ("model selector in telegram is not showing models as they are being added… added glm but it doesn't show"; "none of the models work with oauth via the proxy, anthropic must be blocking it"; "we are also missing the option to add ollama cloud as an account")

| Item | Status | Evidence |
|---|---|---|
| 1. glm not in Telegram picker | 🟡 fixed | cfrproxy already exposed 8 glm models; picker just unsynced. Ran `sync_hermes_cfrproxy.py` → `custom:cfrproxy-glm` now in all 13 profiles + models.py PROVIDER_GROUPS; caches cleared; gateways restarted |
| 2. "OAuth blocked by Anthropic" | ✅ disproven | Every OAuth model ponged through proxy on main + scoped `/p/{provider}/v1` mounts: claude sonnet/haiku/opus, codex gpt-5.5/5.6, grok-4.5, gemini. Only opus-4-8-under-load fails (its Max allowance, REQ-019). OAuth path fully functional |
| 3. Ollama Cloud as an account | 🟡 built | Accounts-tab tile "🦙 Ollama Cloud" (API-key, not OAuth) → creates/enables `ollama-cloud` provider (base `https://ollama.com/v1`). Key pulled from user's `OLLAMA_API_KEY` (bash RC), validated (200), provider live with **18 models** (deepseek-v4, glm-5.1, kimi-k2.6, minimax, gpt-oss, qwen3.5, nemotron…); `gemma4:31b` → "pong from ollama cloud" |
| 4. Make picker auto-update | 🟡 built | `sync_hermes_cfrproxy.py --auto`: change-detecting (provider-set signature in `~/.hermes/.cfrproxy_sync_state`), no-op when unchanged, auto-restarts gateways only on change. systemd user timer `cfrproxy-hermes-sync.timer` every 120s; 0600 env file `~/.config/cfrproxy-sync.env` holds admin pass. Verified no-op on unchanged set |

#### Root cause (picker)
Picker groups (`cfrproxy-<name>`) come from Hermes `custom_providers` + `PROVIDER_GROUPS`, written by the sync script — which had to be run manually after each cfrproxy provider add/remove. Models *within* a provider were always live (Hermes probes `/v1/models`); only add/remove needed the re-sync. Now automated.

#### Files modified
- `internal/api/webui/index.html` — Accounts-tab "API-key accounts" section + `saveOllamaCloud()` JS
- `scripts/sync_hermes_cfrproxy.py` — `--auto` change-detecting mode, state signature, conditional gateway restart
- New: `~/.config/systemd/user/cfrproxy-hermes-sync.{service,timer}`, `~/.config/cfrproxy-sync.env` (0600)

#### Deploy + verify
- `go build -o /home/crogers2287/cfrproxy/cfrproxy .` (service binary path, NOT ~/.local/bin) + `systemctl --user restart cfrproxy`
- Tile + JS confirmed in served `/admin/` HTML; ollama-cloud provider enabled, 18 models, live completion OK
- Timer enabled+active; service no-ops on unchanged set

Note: service runs `/home/crogers2287/cfrproxy/cfrproxy`; `~/.local/bin/cfrproxy` (used by `cfrproxy mcp` procs) was "text busy" and left as-is (WebUI/failover changes not needed by MCP path).

**REQ-020 status: COMPLETE.**

### REQ-019 — Claude models intermittently failing in Hermes ("out of extra usage")

Source: chat ("claude models still failing in hermes" + "none of my models are capped")

#### Root cause
Not a metered key and not a persistent cap. CLIProxyAPI has exactly one Claude auth (single OAuth account `claude-crogers2287@gmail.com.json`, `claude-api-key: []` empty). Under sustained concurrent Hermes load, Anthropic's rolling usage window on that account momentarily trips and returns `400 invalid_request_error` "You're out of extra usage…" (real `request_id`). Low-volume curls always land in a moment of headroom (verified: 8/8 concurrent opus = 200 at test time). The failing rows are all the large Hermes `system=[BLOCKED: SOUL.md…]` requests; fable/sonnet/haiku (lighter weight) and grok stayed 200 in the same window.

#### The bug
That cap arrives as a **400**, which `transientStatus` (408/429/5xx only) did not treat as failover-worthy — so the Hermes agent hard-failed instead of degrading.

| Item | Status | Evidence |
|---|---|---|
| 1. Detect usage/quota exhaustion in 4xx bodies | 🟡 fixed | `usageExhausted()` in proxy.go — matches "out of extra usage", "usage limit", "insufficient_quota", "quota exceeded", "resource_exhausted"; unit-tested against real Anthropic string + negative cases (no misfire on missing-messages / model-not-found) |
| 2. Fail over on cap instead of hard-fail | 🟡 fixed | send loop: 4xx + `usageExhausted` → break attempt loop → next candidate (rather than `resp=r2` → hard 400). Genuine 4xx bodies restored via `io.NopCloser(bytes.NewReader(eb))` for the normal error path |
| 3. Give claude a fallback target | 🟡 set | `providers.fallback` claude → `grok/grok-4.5` (traces show grok-4.5 handling 130k-tok Hermes reqs fine) |
| 4. No false failover on healthy traffic | ✅ verified | post-restart opus `pong` = 200, served by claude, no ⚠️ alert injected |

#### Files modified
- `internal/proxy/proxy.go` — `usageExhausted()` helper + usage-cap failover branch in the candidate send loop

#### Deploy + verify
- `go build -o ~/.local/bin/cfrproxy .`; `systemctl --user restart cfrproxy` (active)
- claude fallback set via SQL: `grok/grok-4.5`
- unit test (throwaway) PASS; live opus 200 clean; 8/8 concurrent opus 200 at test time

**REQ-019 status: COMPLETE** (failover now absorbs the intermittent cap; when opus trips, Hermes transparently gets grok-4.5 with a ⚠️ notice instead of a hard error).

## 2026-07-21

### REQ-018 — Per-model token burn + cache-hit on Live Traces

Source: chat ("track token burn and cache hit on the live traces for each model when it's used")

| Item | Status | Evidence |
|---|---|---|
| 1. Capture tokens all paths | 🟡 built | translated non-stream (from norm), translated stream (usage-capturing tee on final delta), passthrough (usage scanned from copied bytes, non-stream + streamed, no mutation); traces gain prompt/completion/cached cols + migration |
| 2. Cache-hit read | 🟡 built | OpenAI `prompt_tokens_details.cached_tokens`, Anthropic `cache_read_input_tokens` (max-accumulated across message_start/delta) |
| 3. Per-model panel | 🟡 built | `/admin/api/stats` GROUP BY provider/model (reqs/errors/in/out/cached/hit%/avg-latency); "Token burn by model" table above traces, auto-refresh on SSE; per-row in/out/cached cols |
| 4. Verified | 🟡 live | repeat 2815-tok prompt → 2304 cached (82%) recorded on trace; grok auto-router 189k in/126k cached (67%) over 54 reqs; all-models 65% hit |

Note: rows predating the feature show 0 tokens (historical). Commit d20dea7.

**REQ-018 status: COMPLETE.**

## 2026-07-21

### REQ-017 — Round-table consensus MCP + agent profiles + context compression

Source: chat ("consensus mcp setup led by the webui … agent profiles assigned to different models … round table mcp that uses the proxy … research token compression methods … check recent GitHub 1k+ stars, check my gh starred")

Research: microsoft/LLMLingua (LLMLingua-2, 2-5x, python; LiteLLM shipping it on the proxy hot path Apr 2026 — validates pattern); kompact (compression proxy, 40-70%); user's stars: pxpipe 6.5k⭐ (text→image token cuts for Fable vision, future strategy), LoopTroop (LLM councils). Chosen v1: proxy-side compaction (summarize old turns via cheap model, hash-cached) — fail-open, no python dep; llmlingua sidecar can slot behind same config later.

| Item | Status | Evidence |
|---|---|---|
| 1. Agent profiles (WebUI-led) | 🟡 built | agent_profiles table + CRUD API + Agents tab (cards, provider→model dropdowns, persona, temp, enable); seeded Architect(opus-4-8)/Engineer(gpt-5.6-terra)/Skeptic(grok-4.5)/Pragmatist(gemini-3-flash) |
| 2. Round-table MCP | 🟡 built | `cfrproxy mcp` stdio JSON-RPC (initialize/tools/list/tools/call); tools roundtable/consult/list_profiles; parallel round 1 → cross-critique round 2 → moderator synthesis (settings `roundtable`, WebUI section); registered in Claude Code (`claude mcp add roundtable`) |
| 3. Live round table | 🟡 verified | 4 models deliberated key-storage question, sonnet-5 synthesis, 9.4KB output incl per-panelist positions |
| 4. Context compression | 🟡 built+verified | `Compress()` in request path (settings `compression`, WebUI section, opt-in): 7675→657 tok (91%) with buried fact still answered correctly; cache-hit on repeat; fail-open; tool-pair-safe cut point |

Commit (this). Note: compression left disabled by default — enable in WebUI Agents tab.

**REQ-017 status: COMPLETE.**


### REQ-016 — OMP "current model does not use thinking" on grok-4.5 via auto router

Source: chat. Three stacked causes found via logs:
1. OMP-side: cfrproxy models registered with `reasoning: false` → OMP refuses thinking mode. Fixed: auto/auto-plan/opus-4-8/grok-4.5 now `reasoning: true` + effort-mode thinking + compat block in ~/.omp/agent/models.yml (restart OMP to load).
2. Proxy: demo transforms (tag-response/pin-temp) still enabled — polluted every response with proxy_tag AND forced translation path which DROPPED reasoning params. Removed.
3. Wire layer didn't carry reasoning: `reasoning_effort` (openai) and `thinking` (anthropic) now preserved same-dialect and mapped cross-dialect (effort↔budget tiers). Auto/auto-plan requests pinned to translation path (passthrough would drop the plan briefing). `TestReasoningPreserved`; live grok-4.5 w/ effort=high → correct answer, no proxy_tag.

**REQ-016 status: COMPLETE (OMP restart needed on user side).**

## 2026-07-20

### REQ-015 — OAuth categories, dropdown fix, trace legend, planner (Fable), omp/opencode via public URL

Source: chat (multiple asks: dropdowns broken; split oauth catch-all into per-category providers; explain kimi trace + symbols; planning model for auto router; /prompt-master for injected prompts; test api.skinnyc.pro in omp+opencode; fable-method into plan mode; "CFR proxy not showing in OMP")

| Item | Status | Evidence |
|---|---|---|
| 1. oauth split | 🟡 done | `models_filter` (globs+`!`); providers claude(15)/codex(11)/gemini(8+)/grok(13)/command(42)/opencode(36) on CLIProxyAPI backend; oauth deleted; map+auto_router repointed; Hermes re-synced (9-member cfrproxy group), gateways restarted |
| 2. Auto router dropdowns | 🟡 fixed | real provider→model select pairs fed by scoped mounts; unpinned configured values still selectable; Playwright: populated, no console errors |
| 3. Trace symbols | 🟡 done | legend line + row tooltips; kimi-k2.6 trace explained (my public-gate verification calls); ⚠ rows were my failover tests |
| 4. Planner / auto-plan | 🟡 done | `auto-plan` virtual model: Fable Method plan-mode briefing (structure: Ask/Done/Steps/Watch out) prepended as system ctx, then classified routing; prompts optimized via /prompt-master; live note `planned auto→code→codex/gpt-5.6-terra` |
| 5. omp provider | 🟡 done | `cfrproxy` provider in ~/.omp/agent/models.yml (public URL + key, 9 models incl auto/auto-plan); `omp --model cfrproxy/auto -p` → pong (visible in picker after omp restart) |
| 6. opencode provider | 🟡 done | provider in ~/.config/opencode/opencode.json; `opencode run -m cfrproxy/claude/claude-sonnet-5` → pong via public URL |

Commit 98ab143.

**REQ-015 status: COMPLETE.**

### REQ-014 — Publish api.skinnyc.pro → cfrproxy via NPM — IN-PROGRESS (blocked on operator)

Source: chat ("setup api.skinnyc.pro to work with the router please, use nginx proxy manager, details should be in the obsidian vault")

Findings: vault had no NPM runbook; Mnemosyne recall → home NPM = **Tower (192.168.1.5), admin UI :7818**, container `NginxProxyManager` (linuxserver image, /config mount at /mnt/user/appdata/NginxProxyManager). DNS `api.skinnyc.pro` already → home IP 72.186.4.163 (DNS-only). NPM admin API returns 500 for ALL logins: container logs show `SQLITE_IOERR: disk I/O error` on database.sqlite — host-side read is clean (116MB/s) ⇒ stale FUSE fd inside container (classic Unraid shfs issue). Fix = `docker restart NginxProxyManager` — **blocked by permission classifier (remote state change), needs operator**.

| Item | Status | Evidence |
|---|---|---|
| 1. Public-exposure security gate | 🟡 done | proxied requests (XFF/X-Real-IP) require key from `public_api_keys`; LAN unaffected. Verified 200/401/200. Key generated + set. Commit 27d8db2 |
| 2. NPM proxy host | 🔴 blocked | `scripts/publish_api_skinnyc.sh` ready (create/update host + LE cert + verify); needs NPM restart first |

Resolution (operator approved): backed up DB, `docker restart NginxProxyManager` → API healthy (real "Invalid email or password" instead of 500). Actual NPM user found in DB: crogers2287@yahoo.com (memory creds were carl's). Existing host id 41 `api.skinnyc.pro` → Tower:8087 (dead, nothing listening; id 40 deleted predecessor) — repointed to fred:8420 (websockets+block-exploits). **HTTPS blocked: Let's Encrypt has PAUSED issuance for api.skinnyc.pro** (prior failed-validation spam from the dead host) — operator must click the unpause link, then re-request cert. Live over HTTP: /health ok, keyless 401, keyed pong (0.9s).

HTTPS completed after operator unpause: cert #98 issued, ssl_forced on, valid chain (ssl_verify 0), http→https 301. omp + opencode base URLs flipped to https, opencode re-verified pong over TLS.

**REQ-014 status: COMPLETE (HTTPS).**

### REQ-013 — Curated pins + fallback chains in UI + auto router

Source: chat ("build fallback chains into the UI … auto router … orchestrator model that delegates … model selector is still messy, all of the oauth models are showing … keep the top three models from each provider")

| Item | Status | Evidence |
|---|---|---|
| 1. Messy selector | 🟡 fixed | `pinned_models` per provider; aggregate /v1/models 197→18; scoped oauth 143→7 pins; full catalog via `?all=1`; Hermes picker caches cleared so Telegram lists shrink |
| 2. Fallback chains | 🟡 built | transitive candidate chain (cycle-safe, 3 hops); cards show resolved chain (Nexum → fred/agents-a1 → ollama/qwen2.5:7b); live: dead1→dead2→fred answered pong w/ alert |
| 3. Auto router | 🟡 built | virtual model `auto`; classifier oauth/gemini-3.1-flash-lite buckets code/reasoning/quick/long/vision; WebUI editor section; verified: code→gpt-5.6-terra, quick→claude-haiku-4-5, reasoning→claude-opus-4-8 (trace notes `auto→bucket→model`) |
| 4. Trace note column | 🟡 built | additive migration; auto/failover annotations render 🔀 without error styling |

Ops findings: local Ollama runner crashing (GPUs fully claimed by agents-a1 + TWO ollama daemons running: /usr/local/bin + /bin — flagged to operator, not killed); classifier moved to cloud flash-lite; `claude-haiku*` map slot moved off flaky local → oauth/claude-haiku-4-5. Commit 7e21220; service restarted.

**REQ-013 status: COMPLETE.**

### REQ-012 — Full OAuth login in the proxy WebUI (incl. SuperGrok)

Source: chat ("add oauth login directly to the proxy … full featured … log into my super grok subscription … interactive login for the common providers in the webui")

Approach: drive CLIProxyAPI's OAuth flows (it owns working client IDs/PKCE/refresh) from cfrproxy's WebUI via its management API — not a reimplementation.

| Item | Status | Evidence |
|---|---|---|
| 1. Management-API access | 🟡 done | remote-management secret rotated (old was bcrypt-only, unrecoverable; backup written; service restarted, IP-ban cleared); plaintext stored via new `cfrproxy config set cliproxy_mgmt_key` (hidden on `get`) |
| 2. Backend endpoints | 🟡 built | `/admin/api/oauth/{accounts,status,callback,cancel,{provider}/start}` proxying `/v0/management/*` (internal/api/oauth.go) |
| 3. WebUI Accounts tab | 🟡 built | login buttons Claude/Codex/SuperGrok/Antigravity/Kimi; device-flow code display + link + 2.5s status polling; paste-back field for browser flows; account table w/ enable/disable/delete |
| 4. SuperGrok verified | 🟡 live | real xAI device flow issued code Q7GW-2GKY (and D3G2-3QMV via UI click-through), status=wait, cancel ok — completing the login is just entering the code at accounts.x.ai |
| 5. Existing accounts render | 🟡 verified | antigravity/claude/codex listed w/ status; Playwright screenshot, zero console errors |

Commit 5a0ef90; service restarted. Note: management docs at help.router-for.me/management/api; xai+kimi are device flows (no callback), claude/codex/antigravity are browser flows (paste-back).

**REQ-012 status: COMPLETE.**

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
