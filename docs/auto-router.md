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

## `auto-plan` — plan-first execution

Set a **planner** model and cfrproxy exposes a second virtual model, `auto-plan`. It runs a planning stage first: a strong reasoner writes a short execution briefing (structured as *Ask → Done → Steps → Watch out*, following the [Fable Method](https://github.com/Sahir619/fable-method) plan mode), which is prepended to the executor's system context. Then it classifies and routes as usual.

```bash
cfrproxy config set auto_router '{ ... , "planner": "anthropic/claude-opus" }'
```

The trace note reads `planned auto→code→codex/gpt-5-terra`.

Use `auto` for everyday routing; use `auto-plan` when you want the executor to think through a plan before answering a gnarly task.

## Cost note

The classifier and planner are extra calls. The classifier is tiny (one word, ~1-2s). The planner is a full reasoning call, so `auto-plan` is best reserved for hard tasks, not chat. Direct model names skip both.
