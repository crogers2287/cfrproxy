# Skills: index, usage, groups, assignments

cfrproxy indexes every `SKILL.md` under your scan roots and can hand a **catalog** of skills to
any provider mount or share endpoint: only names and descriptions go into the system prompt, and
the model fetches a skill's full instructions through a self-authorizing load URL when a task
needs it (`GET …/skills/<name>?t=<token>`, no headers required).

## Usage counts

The Skills tab ranks skills by how much your agents actually use them:

- **here** — loads through cfrproxy's lazy-load URL (counted per skill per target).
- **Hermes / Claude / Codex / Prime** — mined from the agents' own session logs by
  `scripts/skill_usage_mine.py` (Hermes `skill_view` calls; Claude Code `Skill` invocations and
  `SKILL.md` reads; Codex and Prime `SKILL.md` reads via the aise index) and imported with
  `cfrproxy skills import-usage FILE.json`. Re-runs replace each source's counts, so run it on a
  timer.

## Groups

A group is a named bundle of skills **by name**. Because the index is rebuilt by rescans and the
same skill usually exists as several copies, a group never points at index rows: when a group is
assigned, each member name resolves to the best readable copy (a non-archived one first) at
request time. Members with no readable copy show as *missing* rather than vanishing.

Create groups in the Skills tab (select rows → *New group from selection*, or *Groups → New
group*) or from the shell:

```
cfrproxy skills group set coding-agents --desc "Claude/Codex sessions" --members caveman,prompt-master,hf-download
cfrproxy skills groups
cfrproxy skills assign endpoint w6800-test --groups haxor-homelab --skills ha-mcp
```

## Assignments

A target (provider or share endpoint) carries direct skills plus groups. The *Assignments* view
shows the expanded list, which copy will serve each name, and the exact catalog text that goes
into that target's system prompt. Direct assignments win over group members of the same name.

## Robustness

- A rescan that finds a skill's file gone re-points its assignments at another copy of the same
  name before pruning the row; the fetch handler does the same at request time.
- The index flags rows whose file is missing and names that exist as several differing copies.
- Load URLs carry a per-skill capability token derived from the data-dir secret; the token
  grants that one read only and keeps the catalog byte-stable for prompt caching.
