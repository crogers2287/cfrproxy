# Hermes ↔ cfrproxy: dynamic model selector

Every Hermes agent's Telegram `/model` picker lists all models behind cfrproxy,
live. Add or remove a provider in cfrproxy and the picker reflects it (within the
cache window, or immediately with `/model --refresh`).

## How it works — router → provider → model drill-down

Hermes ships a `/model` command with a live-fetching inline-keyboard picker
(`gateway/slash_commands.py:_handle_model_command` →
`hermes_cli/model_switch.list_picker_providers` → live `GET <base>/v1/models`).
It supports three tiers: **group → provider → model**. cfrproxy maps onto all
three:

- **Router tier (group):** a `cfrproxy` entry in `hermes_cli/models.PROVIDER_GROUPS`
  folds the cfrproxy sub-providers under one "cfrproxy" group button.
- **Provider tier:** each cfrproxy provider is a Hermes custom provider
  `cfrproxy-<name>` pointing at that provider's **scoped mount**
  `http://HOST/p/<name>/v1` — so it lists only that provider's models and
  routes only to it.
- **Model tier:** the scoped mount's `/v1/models` is probed live, so each
  provider shows its real current catalog (a-provider 20, ollama 14, a-provider 17,
  oauth 130 at install).

So in Telegram: `/model` → **cfrproxy** → pick **a-provider / ollama / a-provider / oauth**
→ pick a model.

## Sync (run after adding/removing a cfrproxy provider)

```bash
CFRPROXY_ADMIN_PASS=<webui-pass> \
  ~/.hermes/hermes-agent/venv/bin/python \
  ~/cfrproxy/scripts/sync_hermes_cfrproxy.py
systemctl --user restart 'hermes-gateway-*'
```

`sync_hermes_cfrproxy.py` reads cfrproxy's live provider list and regenerates
(a) the per-provider `custom_providers` entries in every Hermes profile and
(b) the `PROVIDER_GROUPS['cfrproxy']` member list in `models.py` (idempotent,
marker-delimited, backups written). **Models within a provider are always live
— only adding/removing a whole provider needs a re-run.** Re-run this after any
Hermes reinstall too, since it re-applies the `models.py` group patch.

### Legacy single-provider mode

Earlier setup registered cfrproxy as one flat provider (`scripts/inject_cfrproxy.py`,
183 models in a single list, no provider tier). The sync script removes those
entries. Use `inject_cfrproxy.py` only if you want the flat single-row variant.

## What was changed

Injected into `~/.hermes/profiles/<name>/config.yaml` under `custom_providers:`
for ash, canna, fogger, grant, haxor, max, winston (backups: `config.yaml.bak-cfrproxy-*`):

```yaml
- name: cfrproxy
  base_url: http://127.0.0.1:8420/v1
  api_key: cfrproxy          # any non-empty value → triggers live /models discovery
  api_mode: chat_completions
  discover_models: true
```

`api_key` set (any value; cfrproxy's data plane ignores it) makes the picker
treat live `/v1/models` as source of truth and replace the configured list with
the full live catalog.

Re-inject after a Hermes reinstall with
`scratchpad/inject_cfrproxy.py` (idempotent; skips profiles already carrying the block).

## Usage in Telegram

- `/model` → picker; pick **cfrproxy** → drill into any model → confirm.
  Switch is per-session (`_session_model_overrides`); `--global` persists to config.
- `/model cfrproxy/openrouter/anthropic/claude-sonnet-4` → direct switch (note the double slash:
  Hermes provider `cfrproxy`, cfrproxy model `openrouter/anthropic/claude-sonnet-4`).
- `/model --refresh` → bust the 15-min picker cache for an immediate re-scan.

## Freshness

Picker cache TTL is 15 min, stale-while-revalidate (serves stale instantly,
refreshes in the background), overridable via `HERMES_PICKER_CACHE_TTL`.
`/model --refresh` forces an immediate refresh. So a model added to cfrproxy
shows up on the next `/model --refresh`, or automatically within ~15 min.

## OAuth-backed models (Codex, Claude, Antigravity, Kimi, SuperGrok)

cfrproxy provider **oauth** points at CLIProxyAPI (`127.0.0.1:8317`), which holds
the subscription OAuth logins and exposes them as `oauth/<model>` (130 models:
`oauth/claude-sonnet-5`, `oauth/gpt-5.6-terra`, `oauth/claude-command-grok-4.5`, …).
Log in / add accounts with:

```
cfrproxy login codex          # or codex-device for headless device-code flow
cfrproxy login claude         # Anthropic OAuth
cfrproxy login antigravity
cfrproxy login kimi
cfrproxy login supergrok      # xAI / Grok OAuth
```

Accounts stack in CLIProxyAPI's auth dir; new models appear under `oauth` on the
next scan, so they flow into the Telegram picker automatically. Anthropic OAuth
routing is preserved — cfrproxy translates any inbound dialect to CLIProxyAPI's
Claude endpoint (`oauth/claude-sonnet-5` verified end-to-end).
