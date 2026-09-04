# Auto-router

Send the virtual model **`auto`** and cfrproxy classifies the request into a task bucket, then delegates to the model *you* mapped for that bucket. It's an orchestrator: one cheap classifier decides, your chosen models execute.

## How it works

1. A small, fast **classifier model** reads a trimmed view of the request (last user message + tool count) and answers with one bucket word.
2. cfrproxy routes the request to the model mapped for that bucket.
3. The choice is recorded on the Live Traces row as `auto→<bucket>→<model>`.

Default buckets: `code`, `reasoning`, `quick`, `long`, `vision`, plus `default`. Any failure (classifier down, unparseable answer) falls through to the `default` route — the auto path never hard-fails.

## Configure it

In the WebUI (Providers tab → Auto Router), or via settings:

```bash
cfrproxy config set auto_router '{
  "enabled": true,
  "classifier": "local/qwen2.5:7b",
  "routes": {
    "code":      "codex/gpt-5-terra",
    "reasoning": "anthropic/claude-opus",
    "quick":     "anthropic/claude-haiku",
    "long":      "gemini/gemini-flash",
    "vision":    "openrouter/qwen-vl",
    "default":   "codex/gpt-5-terra"
  }
}'
```

Use a small local model as the classifier if you have one — it's the cheapest option and adds only ~1-2s. A fast cloud model works too.

## Smart mode — profile → live registry → local-first selector

The bucket table above breaks every time a local model is renamed, unloaded or resized, and it
cannot tell a two-line edit from an architecture rewrite. Smart mode replaces the table with a
**profile** and a **derived registry**:

1. The classifier answers one word — `routine`, `careful` or `hard` — and cfrproxy measures the
   rest itself: prompt tokens, depth, tool count, image present. Nothing in the profile names a
   model, so it never goes stale. `"classify":"heuristic"` skips the model call entirely (tools +
   depth/tokens → careful; a few "architect/refactor/security/proof" words → hard).
2. The tier's candidate list is walked **in operator order** (write local first). Entries may be
   globs (`fred/tiel*`, `fred/*flash-next*`) or pool names. For each candidate cfrproxy checks
   facts it already has or can probe in milliseconds:
   - **served** — in the provider's listing or a pool name
   - **local** — the provider answers llama-swap's `/running` (or is ollama / listed in
     `local_providers`); a cloud subscription behind a loopback bridge is correctly *not* local
   - **warm / cold / busy** — `/running` state and `/upstream/<m>/slots`
   - **context** — provider override → llama-swap meta → published catalog
   - **vision** — llama-swap `isVision` → `vision_models` globs
   - **health** — failed + fellback share in `usage_daily` over the last `health_window_days`
3. The first candidate that passes every hard gate wins: served, sighted when the request has an
   image, window ≥ 1.1× the prompt, and local only under `local_max_tokens`. Candidates that are
   merely busy, cold or unhealthy are a last resort (in that order) — a cold llama-swap load
   costs minutes, so a warm cloud model beats it. Sticky pins are re-validated against the hard
   gates every turn, so a conversation that outgrows its local window escalates by itself.

```bash
cfrproxy config set auto_router '{
  "enabled": true, "classifier": "codex/gpt-5.6-luna",
  "routes": {"default": "codex/gpt-5.6-luna"},
  "smart": {
    "enabled": true,
    "local_max_tokens": 150000,
    "tiers": {
      "routine": ["fred/tiel-kvx-w6800", "fred/ornith", "fred/qwen38-flash-next-kvx", "ccbudget/deepseek/deepseek-v4-flash"],
      "careful": ["fred/qwen38-flash-next-kvx", "fred/ornith", "codex/gpt-5.6-terra"],
      "hard":    ["claude/claude-fable-5", "codex/gpt-5.6-terra"]
    }
  }
}'
```

Optional keys: `vision` (a list that replaces the tier for image requests), `prefer_warm` /
`skip_busy` (default true), `health_window_days` (3), `health_min_requests` (20),
`health_max_fail_rate` (0.5), `local_providers` / `cloud_providers`, `log` (default true).
`routes.default` is the last resort when nothing in any tier qualifies.

**Cold-prefill budget.** A new conversation whose static prefix no local instance has served is
judged by `prompt tokens / measured prefill rate`; over `max_cold_prefill_seconds` (30) a viable
cloud model wins. Before giving up on a local model the router asks kvxd (dry-run probe) whether
a KV-Rosetta artifact already covers the prompt; a seeded harness prefix turns a 67 s cold prefill
into a few seconds of tail. Seed the standing prefixes once per harness version:

```bash
cfrproxy kvx prefixes --client claude-code                       # recorded static prefixes
cfrproxy kvx seed --model fred/ornith-kvx-w6800 --client claude-code   # render, prefill, admit, pin
```

**Is it working?** `cfrproxy route trajectories` (or the Routing activity panel) shows one row per
conversation: model sequence, turns, cache-hit %, latency, escalations and the first turn's kvx
verdict.

**Why did it pick that?**

```bash
cfrproxy explain auto --tier careful --tokens 60000 --tools 12
cfrproxy explain auto --text "rewrite the auth layer" --image        # asks the classifier
curl -u admin:… 'localhost:8420/admin/api/explain?model=auto&tokens=60000&tools=12&tier=careful'
```

prints the profile, every candidate with its verdict (`chosen`, `viable`, `busy`, `cold`,
`unhealthy`, `blind`, `too small`, `beyond local_max_tokens`, `not served`) and the facts behind
it. The trace note reads `auto→careful→fred/ornith` (`·sticky` on pinned turns), and every
decision is appended to `~/.cfrproxy/route-decisions.jsonl` — the training rows for a future
sidecar classifier (`CFRPROXY_ROUTE_LOG=off` disables it).

## `auto-plan` — plan-first execution

Set a **planner** model and cfrproxy exposes a second virtual model, `auto-plan`. It runs a planning stage first: a strong reasoner writes a short execution briefing (structured as *Ask → Done → Steps → Watch out*, following the [Fable Method](https://github.com/Sahir619/fable-method) plan mode), which is prepended to the executor's system context. Then it classifies and routes as usual.

```bash
cfrproxy config set auto_router '{ ... , "planner": "anthropic/claude-opus" }'
```

The trace note reads `planned auto→code→codex/gpt-5-terra`.

Use `auto` for everyday routing; use `auto-plan` when you want the executor to think through a plan before answering a gnarly task.

## Cost note

The classifier and planner are extra calls. The classifier is tiny (one word, ~1-2s). The planner is a full reasoning call, so `auto-plan` is best reserved for hard tasks, not chat. Direct model names skip both.
