# Hermes ↔ cfrproxy: dynamic model selector

Every Hermes agent's Telegram `/model` picker lists all models behind cfrproxy,
live. Add or remove a provider in cfrproxy and the picker reflects it (within the
cache window, or immediately with `/model --refresh`).

## How it works

Hermes already ships a `/model` command with a live-fetching inline-keyboard
picker (`gateway/slash_commands.py:_handle_model_command` →
`hermes_cli/model_switch.list_picker_providers` → live `GET <base>/v1/models`).
cfrproxy is registered as **one custom provider** in each profile; because it
aggregates fred + ollama + Nexum + the CLIProxyAPI OAuth subs, that single row
surfaces everything (183 models at install time).

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
- `/model cfrproxy/fred/agents-a1` → direct switch (note the double slash:
  Hermes provider `cfrproxy`, cfrproxy model `fred/agents-a1`).
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
