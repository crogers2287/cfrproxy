# Context compression

An opt-in feature that cuts token bills on long conversations by summarizing older turns. It's off by default — read the trade-offs before enabling it.

## How it works

When a request's estimated tokens exceed a threshold, cfrproxy:

1. Keeps the most recent N messages verbatim.
2. Sends the older turns to a cheap **summarizer model**, which produces a faithful working summary (preserving decisions, file paths, identifiers, numbers, open questions).
3. Replaces the old turns with that single summary block.
4. Caches the summary by content hash, so an ongoing conversation only pays for compression once per prefix.

It's **fail-open**: any summarizer error leaves the request untouched. The cut point never splits a tool-call/result pair. The Live Traces note shows `compressed ~A→~B tok`.

## Configure

WebUI → Agents tab → *Context compression*, or:

```bash
cfrproxy config set compression '{
  "enabled": true,
  "summarizer": "anthropic/claude-haiku",
  "threshold_tokens": 24000,
  "keep_recent": 8,
  "target_words": 500
}'
```

## When to use it — and when not to

**Good fit:** long, mostly append-only conversations — research threads, document Q&A, chat. The savings are real (often 80-90% on the compressed portion).

**Be careful with agentic coding traffic.** Summarization is lossy. An agent that loses the exact state of a file three edits back can quietly go wrong. And rewriting the history **busts the provider's own prompt cache** (Anthropic/OpenAI cache on exact prefix bytes) — on a long stable prefix that was already being cached cheaply, compression can *cost more* than it saves.

The Live Traces dashboard shows per-model **cache-hit %**, so you can measure this directly: turn compression on, and if cache-hit drops on your cloud models, it's fighting their prompt cache.

**Recommendation:** leave it off for coding agents (they manage their own context); enable it for a specific high-volume, long-context, non-code workload where you've confirmed the savings.
