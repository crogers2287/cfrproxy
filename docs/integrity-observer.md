# Output integrity observer

The output integrity observer is a per-provider and per-share-endpoint switch
for finding likely decode corruption while cfrproxy streams a response. The
first release is deliberately **observation only**: it records signals and
review labels, but never blocks, rewrites, retries, truncates, or replaces a
response.

The observer is a native Go adaptation of ideas from
[SIMURG](https://github.com/doofzoff/SIMURG). It adds useful evidence to the
proxy without adding a Python service or model dependency.

## What it gives us

- Visibility into repetition collapse, character/script drift, numeric or
  structural breakdown, entropy collapse, and abrupt statistical/semantic
  drift that otherwise look like successful HTTP 200 responses.
- Provider/model-level counts in the dashboard: observed responses, suspect
  or corrupt flags, reviews, confirmed false alarms, and reviewed misses.
- A labeled, deployment-specific calibration set. This matters because coding,
  multilingual, and tool-heavy traffic have very different normal shapes.
- A safe foundation for a later retry/failover policy, with a measured false
  positive rate instead of imported thresholds.

This is not a factuality checker. It also does not validate tool arguments,
schemas, citations, or whether an answer followed the prompt. Reasoning tokens
and tool JSON are excluded; only visible assistant text is observed.

## Configure a provider

Use the **Observe** switch on a provider card, or edit the card for model and
profile controls. The CLI equivalent is:

```bash
cfrproxy provider edit --name cloud \
  --integrity-mode observe \
  --integrity-models 'code-*,!*-vision' \
  --integrity-profile code
```

`--integrity-models` accepts comma-separated `*` globs. Patterns can match a
bare model (`code-*`) or a provider-qualified model (`cloud/code-*`). A `!`
exclusion always wins. Blank means every model served by that provider.

Profiles:

| Profile | Use for | Difference |
|---|---|---|
| `general` | English-dominant prose and ordinary chat | All signal families enabled |
| `code` | Coding agents and code-heavy responses | Dense digits, symbols, URLs, fences, paths, and install commands do not accuse by themselves |
| `multilingual` | Expected mixed/non-Latin text | Foreign-script drift rules are disabled |

If a request fails over, the policy is evaluated against the provider/model
that actually produced the response, so the trace and observation refer to the
same model.

## Configure a share endpoint

Every Share card has its own switch and three modes:

- `inherit` — use the actual serving provider's policy.
- `off` — never observe responses through this share URL.
- `observe` — observe responses through this share URL even if the serving
  provider is off; the share card's model globs and optional profile apply.

This makes it practical to canary observation with one stream of traffic
without turning it on for every caller of a provider.

## What gets recorded

Observed traces contain:

- state: `clean`, `suspect`, `corrupt`, or `skipped` when no visible assistant
  text was present (for example, a tool-only response);
- current and maximum score, reasons, onset, character count, and profile;
- a versioned set of feature checkpoints (first check near 350 visible
  characters, then approximately every 400 characters, capped at 64 samples);
- up to 2,000 visible characters of review context, captured only after a
  suspect/corrupt checkpoint;
- an operator label: `clean`, `corrupt`, or `uncertain`.

Open a row in **Live Traces** to inspect the evidence and assign a label.
Feature data is bounded, but flagged excerpts can contain sensitive model
output. cfrproxy retains the newest 5,000 traces plus up to 5,000 older
observed traces, so a narrow canary is not erased by unrelated traffic before
it accumulates a useful sample.
Use **Export observations** on that page to download the retained set as JSONL;
request and response snippets are omitted from the export, while bounded flag
excerpts and feature checkpoints are included.

## Finish plan: calibrated enforcement

Enforcement is intentionally not exposed as a mode in this pull request. The
next phase should start only after representative traffic has been recorded:

1. **Build ground truth.** Review every flag plus a stratified sample of clean
   responses. Before enforcing a profile, target at least 1,000 reviewed clean
   outputs for it and at least 250 reviewed outputs for each model/profile pair
   selected for the canary. Add known real or replayed corruption cases so
   recall is measurable rather than inferred from a handful of organic events.
2. **Calibrate and freeze.** Fit thresholds per profile (and per model family
   where justified), version the resulting configuration with the feature
   schema, and require the one-sided 95% false-positive upper bound to be below
   0.5% for every model/profile being enabled. Set an explicit recall target on
   the labeled corruption set.
3. **Add a stream commit boundary.** Enforcement must buffer the original
   provider frames for roughly the first 350 visible characters. A corrupt
   result before any client bytes are committed may cancel and retry/fail over.
   Once bytes have been sent, cfrproxy cannot safely “unsend” them or splice a
   second model into the same answer; a post-commit detection must use a
   dialect-native abort and let the client retry. Non-streaming responses can be
   checked in full before headers are committed.
4. **Canary narrowly.** Add `enforce` only to selected provider/model or share
   cards, with an immediate `off` switch. Track added time-to-first-token,
   aborts, successful recoveries, false positives, and fallback cost.
5. **Promote or roll back from evidence.** Expand only when the canary meets the
   frozen error budget. Keep tool/schema validation as a separate detector and
   never treat this observer as an answer-correctness guarantee.

The buffering in step 3 is a real latency trade-off; observe mode in this PR
makes each token available to the response writer before scoring it and
therefore does not introduce that commit hold.
