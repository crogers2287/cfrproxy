# Round-table consensus MCP

Define **agent profiles** — each a persona pinned to a model — and let a panel of different models deliberate a question, cross-critique each other, and have a moderator synthesize the result. It's exposed as an MCP server, so any MCP-capable agent (Claude Code, etc.) can convene the panel as a tool.

Because every panelist routes through cfrproxy, they can be a mix of any providers: a local model, a Claude subscription, an OpenAI model, and a Grok model can all sit at the same table.

## Agent profiles

WebUI → **Agents** tab → *Add profile*. Each profile has a name, a model (`provider/model` via the router), a persona (system prompt), and an optional temperature. Example panel:

| Profile | Model | Persona |
|---|---|---|
| Architect | anthropic/claude-opus | long-term structure, boundaries, simplicity |
| Engineer | codex/gpt-5-terra | ships-this-week feasibility, edge cases |
| Skeptic | grok/grok-4 | attacks assumptions, finds failure modes |
| Pragmatist | gemini/gemini-flash | cost, maintenance, anti-gold-plating |

## Run the MCP server

```bash
cfrproxy mcp        # stdio JSON-RPC MCP server
```

Register it in a harness:

```bash
claude mcp add roundtable -- cfrproxy mcp
```

## Tools

- **`roundtable`** — every enabled profile answers independently (in parallel), then (round 2) each sees the others and revises, then a moderator synthesizes *consensus / disagreements / recommendation / dissent*. Args: `question`, optional `context`, `profiles` (subset), `rounds` (1 or 2).
- **`consult`** — ask a single profile.
- **`list_profiles`** — list configured profiles.

## Settings

WebUI → Agents tab → *Round table settings*, or:

```bash
cfrproxy config set roundtable '{"moderator":"anthropic/claude-sonnet","rounds":2,"max_tokens":1200}'
```

`moderator` synthesizes the final answer (blank = the first panelist). `rounds` 1 = answers + synthesis; 2 = adds the cross-critique round.

### Compressing the shared context

A round table sends the same question + context to every panelist, so a long context is paid for N models × M rounds. Enable **`compress_context`** (WebUI → Round table settings, or the setting below) to summarize the shared context **once** before fan-out — each panelist then gets the short version instead of the full payload:

```bash
cfrproxy config set roundtable '{"moderator":"...","rounds":2,"compress_context":true}'
```

It uses the summarizer model from the [Context compression](compression.md) settings and works even if global compression is off. The panel output notes how much was compressed.

## When to use it

Decisions, designs, and reviews where independent perspectives beat a single model's confident take — architecture calls, "should we do X or Y", spotting failure modes a lone model would miss. It's slower and costs more than one call (N models × rounds + synthesis), so reserve it for the questions that matter.
