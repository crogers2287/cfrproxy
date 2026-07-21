# Fusion

Send the virtual model **`fusion`** and cfrproxy asks several models the same question in parallel, then a **judge** model synthesizes one best answer from their drafts — modeled on [OpenRouter's Fusion](https://openrouter.ai/blog/announcements/fusion-beats-frontier/). Unlike the [round-table MCP](roundtable.md) (which returns a transcript for an agent), fusion returns a single clean answer through the normal chat endpoint, so any tool can use it just like a regular model.

## How it works

1. Your request goes to each **participant** model in parallel (they answer independently).
2. Their drafts are handed to the **judge** model, which weighs them for consensus, contradictions, gaps, and unique insights, then writes one final answer to your original request.
3. That judge answer streams back like any completion — failover, tracing, and token accounting all apply. The trace note reads `fusion(N)→<judge>`.

The final answer contains no mention of the drafts or that multiple models were involved — you get one authoritative response.

## Configure

WebUI → Providers tab → *Fusion*, or:

```bash
cfrproxy config set fusion '{
  "enabled": true,
  "participants": ["codex/gpt-5-terra", "gemini/gemini-flash", "grok/grok-4"],
  "judge": "anthropic/claude-opus",
  "max_tokens": 2000
}'
```

Then send `model: "fusion"` to any endpoint.

## When to use it

Fusion is 2-3× slower and pricier than a single call (N drafts + a synthesis), so reserve it for hard questions where quality matters — design decisions, tricky debugging, analysis. For routine work, use a direct model or the [auto-router](auto-router.md).

Tips: pick 2-4 diverse participants (different model families disagree more usefully), and a strong reasoner as the judge. Even fusing a model with itself tends to beat a single call.
