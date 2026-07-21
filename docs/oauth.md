# OAuth subscriptions as providers

You can bring your *subscription* logins — Claude, Codex/ChatGPT, xAI SuperGrok, Google Antigravity, Kimi — into cfrproxy as models, without API keys. cfrproxy doesn't reimplement each OAuth flow; it drives [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI), which already holds working client IDs, PKCE, and token refresh for all of these, and exposes them as an OpenAI-compatible endpoint.

## Setup

1. Run CLIProxyAPI (it listens on `:8317` by default). Set a management secret in its `config.yaml` (`remote-management.secret-key`).

2. Tell cfrproxy the management key:

   ```bash
   cfrproxy config set cliproxy_mgmt_key <the-secret>
   # optional if not on the default port:
   cfrproxy config set cliproxy_mgmt_url http://127.0.0.1:8317
   ```

3. Register CLIProxyAPI as a normal provider:

   ```bash
   cfrproxy provider add --name oauth --type openai --base-url http://127.0.0.1:8317/v1 --key <cliproxy-api-key>
   ```

   Its models now show up as `oauth/<model>` and flow into every picker.

## Interactive login (WebUI)

Open the **Accounts** tab. One click per provider:

- **Device-flow** providers (SuperGrok, Kimi) show a pairing code + link — enter the code at the provider's site, and the WebUI polls until it flips to logged-in. Works from your phone.
- **Browser-flow** providers (Claude, Codex, Antigravity) give a login link; after approving you land on a `localhost` URL that won't load — paste that full URL back into the WebUI to finish.

The Accounts tab also lists every account with enable/disable/delete.

Or from the CLI:

```bash
cfrproxy login claude       # or codex | codex-device | supergrok | antigravity | kimi
```

## Splitting one backend into categories

CLIProxyAPI can expose 100+ models under one endpoint. Rather than one giant `oauth` provider, register several category providers pointing at the same backend, each filtered to a family, so pickers stay tidy:

```bash
cfrproxy provider add --name claude --type openai --base-url http://127.0.0.1:8317/v1 --key <k>
cfrproxy provider edit --name claude --filter "claude-*,!claude-command-*" --pinned "claude-opus,claude-sonnet,claude-haiku"

cfrproxy provider add --name codex  --type openai --base-url http://127.0.0.1:8317/v1 --key <k>
cfrproxy provider edit --name codex --filter "gpt-*,codex-*"
```

## Anthropic OAuth routing

Because cfrproxy translates dialects, a Claude subscription model reached via `oauth/claude-sonnet` works whether the inbound request is Anthropic or OpenAI shaped — so Claude Code, Codex, and OpenCode can all use your Claude subscription through the one endpoint.
