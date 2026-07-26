---
name: cfrproxy-setup
description: Install cfrproxy on a new machine, auto-register existing OAuth subscriptions (Claude, Codex, Grok/xAI, Antigravity/Gemini, Kimi) as providers, expose them in Hermes/Telegram model pickers, and share scoped access with someone else. Use when setting up cfrproxy from scratch, when OAuth logins aren't showing up as providers, when models are missing from a picker, or when wiring one cfrproxy behind another.
---

# cfrproxy setup

Goes from a clean machine to "every model I own is routable, and my agents can pick them by name".

Five phases. Each ends in a command whose output tells you whether to continue.

---

## 0. Mental model

cfrproxy is one Go binary plus one SQLite file. Two planes:

- **Data plane** — what harnesses call: `/v1/chat/completions`, `/v1/messages`, `/api/chat`
- **Admin plane** — WebUI + `/admin/api/...`, basic-auth

Three ways to address a model:

| Form | Example | Meaning |
|---|---|---|
| `provider/model` | `claude/claude-sonnet-5` | explicit; always wins |
| bare model | `claude-sonnet-5` | fuzzy-matched across providers |
| virtual | `auto`, `auto-plan`, `fusion`, `auto:NAME` | router / synthesis pseudo-models |

Two scoped views onto the same proxy:

- `/p/{provider}/v1/...` — one provider, bare model ids. Used to give each provider its own OpenAI endpoint.
- `/e/{endpoint}/v1/...` — a **share endpoint**: key-authed, restricted to an allow-list. Used to hand someone else access.

---

## 1. Build and run

```bash
git clone https://github.com/crogers2287/cfrproxy && cd cfrproxy
go build -o cfrproxy .
./cfrproxy serve                      # prints a generated WebUI password on FIRST launch only
```

State lives in `~/.cfrproxy/` (`cfrproxy.db`, `secret.key`). API keys are AES-256-GCM encrypted at rest.

Lost the password: `./cfrproxy passwd --pass NEW`.

Every subcommand takes `--data DIR` to target a different instance. **`--data` must come before positional args** — Go's flag parser stops at the first non-flag:

```bash
./cfrproxy config --data /tmp/other set cliproxy_mgmt_key abc   # correct
./cfrproxy config set cliproxy_mgmt_key abc --data /tmp/other   # --data silently ignored
```

---

## 2. OAuth subscriptions (the big one)

Subscriptions — Claude, ChatGPT/Codex, SuperGrok, Antigravity, Kimi — are not API keys. They're reached through
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI), which holds the OAuth tokens and exposes one
OpenAI-compatible endpoint. cfrproxy then fronts it with **one provider per subscription**.

### 2a. Log in

Already logged in via CLIProxyAPI? Skip to 2b — the scan finds existing accounts.

```bash
./cfrproxy login claude          # also: codex, codex-device, antigravity, kimi, supergrok
```

Needs the CLIProxyAPI binary. Found on `PATH` as `cli-proxy-api`, or set `CLIPROXY_BIN`.

### 2b. Set the management key — the step that trips everyone

```bash
./cfrproxy config set cliproxy_mgmt_key <secret>
```

**Do not copy it out of `~/.cli-proxy-api/config.yaml`.** CLIProxyAPI stores
`remote-management.secret-key` **bcrypt-hashed** (a 60-char `$2a$...` string). Sending the digest as a bearer token
gives a bare `401` with no explanation. Use the plaintext secret you chose when you enabled remote management.

cfrproxy detects the hash and says so rather than letting you chase the 401.

The *data-plane* key (`api-keys:` in the same file) **is** plaintext, and the scan reads it automatically — you don't
need to supply that one.

### 2c. Scan

```bash
./cfrproxy oauth scan            # preview — changes nothing
./cfrproxy oauth scan --apply    # create the providers
```

```
CLIProxyAPI api-key: read from CLIProxyAPI config.yaml

AUTH          PROVIDER   ACTION       MODELS  DEFAULT / DETAIL
antigravity   gemini     created      9       gemini-3-flash
claude        claude     created      16      claude-sonnet-5
codex         codex      created      11      gpt-5.6-terra
xai           grok       created      13      grok-4.5
```

What it does per account: creates a provider pointing at CLIProxyAPI's `/v1`, with a `models_filter` that admits only
that subscription's families, and a default model chosen from the live catalog.

- **Idempotent.** Existing providers are reported `exists` and never modified — re-run freely, it won't clobber tuning.
- **Disabled accounts** are skipped.
- `--key K` overrides the data-plane key; otherwise it reuses one already stored on a provider with the same base URL,
  then falls back to `config.yaml`.

### 2d. Why `models_filter` is mandatory here

Every OAuth-backed provider shares **one** base URL. Without a filter, any provider could serve any model, and a
request for `gpt-5.6-terra` addressed to `claude` would quietly succeed against the wrong subscription.

The `claude` filter allow-lists real families (`claude-opus-*,claude-sonnet-*,claude-haiku-*,claude-fable-*,claude-3-*,claude-4-*`)
rather than excluding alias ones. CLIProxyAPI's `oauth-model-alias` config mints forks like `claude-gpt-5.6-luna` and
`claude-opencode-sonnet-5`, and every machine configures a different set — an exclusion list silently absorbs any
family you didn't think of. Verify:

```bash
./cfrproxy models --name claude
curl -s localhost:8420/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"claude/gpt-5.6-terra","messages":[{"role":"user","content":"hi"}]}'
# expect: model "gpt-5.6-terra" is not served by provider "claude"
```

---

## 3. Non-OAuth providers

```bash
./cfrproxy provider add --name openrouter --preset openrouter --key sk-or-... --model anthropic/claude-sonnet-4
./cfrproxy provider add --name local --preset ollama --model qwen2.5:7b
./cfrproxy provider add --name custom --type openai --base-url https://host/v1 --key sk-...
./cfrproxy models        # confirm every provider's live catalog
```

Base URLs are auto-discovered — paste a full endpoint URL and it strips back to the base.

---

## 4. Hermes / Telegram pickers

`scripts/sync_hermes_cfrproxy.py` writes one Hermes `custom_provider` per cfrproxy provider, each pointed at that
provider's `/p/<name>/v1` mount, and registers them as a `cfrproxy` group.

```bash
python3 scripts/sync_hermes_cfrproxy.py          # verbose, hands-off
python3 scripts/sync_hermes_cfrproxy.py --auto   # no-op unless the provider set changed; restarts gateways
systemctl --user restart 'hermes-gateway-*'      # required after a manual run
```

Result in the `/model` picker:

```
cfrproxy ▸ cfrproxy-grok ▸ grok-4.5
cfrproxy ▸ cfrproxy-claude ▸ claude-sonnet-5
```

Re-run only when providers are **added or removed** — models inside a provider are probed live.

**Gateways cache Python modules at import.** Any edit to Hermes' own source (including model metadata) needs
`systemctl --user restart 'hermes-gateway-*'` before it takes effect. A change that "didn't work" is usually this.

### Context windows

Hermes sizes context from the models.dev catalog, falling back to a substring table in
`agent/model_metadata.py`. A model missing from the catalog silently inherits a family catch-all — a 1M-context model
can show as 128k. Matching is **longest-key-first substring**, so `qwen3.8-max-preview` does *not* match
`qwen-3.8-max-preview-thinking` (note the dash). Add both spellings when a router serves its own slug.

Local servers report no context length at all; set them explicitly in `CONTEXT_LENGTHS` in the sync script.

---

## 5. Sharing access

A **share endpoint** is a key-authed, allow-listed view. Create one in the WebUI (Endpoints tab), then scope it:

- `models` empty → the whole catalog
- `models` = `Nexum/*` → only that provider (globs match against `provider/model`)
- `force_model` set → every request pinned to one model, ignoring what's asked

```bash
curl -s https://your-host/e/<name>/v1/models -H "Authorization: Bearer <endpoint-key>"
```

Denials are explicit: a model outside the allow-list returns **403**, a bad key **401**.

### Chaining into someone else's cfrproxy

The recipient adds the endpoint as an ordinary provider:

```bash
./cfrproxy provider add --name shared --type openai \
  --base-url https://your-host/e/<name>/v1 --key <endpoint-key>
./cfrproxy models --name shared
```

Model ids nest — they address `shared/Nexum/qwen-3.7-max` — and bare ids (`qwen-3.7-max`) also resolve, so pickers
work either way. Streaming passes through both hops.

---

## Troubleshooting

**A provider is listed but has no models.** Its scoped mount isn't resolving. Check for a stray space or case
difference in the provider name — `/p/Qwen%20/v1` and `/p/qwen/v1` both resolve now, but a stale harness config can
still point somewhere odd. Confirm directly:

```bash
curl -s localhost:8420/p/<name>/v1/models
```

**Requests answered by the wrong provider.** A `provider/model` prefix that matches nothing falls through to
fuzzy-matching and then to "highest-priority provider + its default model". Check the trace — it records the provider
actually used. An unknown `/p/` mount now returns 404 rather than rerouting.

**`Tool 'X' not found in provided tools` (400).** The client sent `tool_choice` naming a tool absent from its own
`tools` array. Inspect the harness's outbound request; the proxy passes `tool_choice` through unchanged, so this is
always client-side.

**Repeated `⚠️ failover: <model> active`.** Expected once per from→to pair per 30 min while a provider is down. The
full reason is on the trace and the WebUI errors panel, not in chat. Constant repetition means the pair keeps changing
— check which provider is failing.

**402 from a provider.** Treated as an exhausted account and failed over, not retried. Recharge or drop it from the
chain; otherwise the banner fires indefinitely.

**Telegram "message delivery failed".** Not a cfrproxy path. Check the gateway log for
`Flood control exceeded. Retry in N seconds` — Telegram rate-limiting the bot, usually from streaming message edits.

## Reference

```bash
./cfrproxy oauth scan [--apply] [--key K]   # register OAuth subscriptions
./cfrproxy provider list | add | edit | rm
./cfrproxy models [--name N]                # live catalogs
./cfrproxy test --name N                    # send a test prompt
./cfrproxy logs -f                          # follow traces
./cfrproxy route set a,b,c                  # priority order
./cfrproxy map 'claude-sonnet*' local/model # rewrite harness preset names
./cfrproxy config set KEY VALUE
```

Deeper topics: `docs/oauth.md`, `docs/providers.md`, `docs/share-endpoints.md`, `docs/auto-router.md`,
`docs/architecture.md`.
