# Share endpoints

Hand someone else a URL + API key that reaches **only** the models you allow — without giving them your keys or your whole proxy. Manage them in the WebUI **Share** tab (or the REST API).

## What an endpoint is

Each endpoint has:
- a **name** — used in its URL: `https://your-host/e/<name>/v1`
- its own **API key** — auto-generated, shown in the WebUI to copy; revoke by deleting/disabling
- a **model policy** — either:
  - **Force model**: every request routes to one model you pick (including `auto` — so you can give someone your auto-router without exposing individual models), or
  - **Allow-list**: the requested model must be one you selected (empty = all models)

## Create one

WebUI → **Share** → *New endpoint*. Pick a name, generate a key, choose either a forced model or a set of allowed models, save. Copy the URL and key and send them.

The recipient points any OpenAI-compatible tool at:
```
base URL:  https://your-host/e/<name>/v1
api key:   <the endpoint key>
```
Their tool's model picker (`GET /e/<name>/v1/models`) shows only what you allowed.

## Enforcement

- **Key required, always** — unlike the LAN data plane, share endpoints demand their key even locally. Wrong/missing key → 401.
- **Model policy** — a request for a model outside the allow-list → 403. A forced model overrides whatever the caller sends. `cfrproxy explain <model> --endpoint <name>` shows exactly which rule would apply.
- **Context cap** — `context_length` on the endpoint caps the window advertised to its clients (and what the proxy will fit a request into), so a shared key cannot run 256k-token sessions on a box sized for 64k.
- **No fallback** — `no_fallback` keeps a request on the model it asked for; the caller gets the real error instead of a silent reroute onto a provider they were never granted.
- **Caveman** and **skills** — per-endpoint tool-result compression ([caveman.md](caveman.md)) and lazy-loaded skills assigned to the endpoint. The skill catalog injected into the system prompt lists a load URL per skill; that URL carries a per-skill token (`?t=…`) so a plain `GET` with no headers fetches the SKILL.md — agents whose only HTTP tool cannot send the endpoint key still lazy-load. The token grants only that one read.
- Everything else still applies: failover, tracing (their calls show in your Live Traces), token accounting, and — if the forced model is `auto`/`auto-plan` — your auto-router.

## API

```
GET    /admin/api/endpoints
POST   /admin/api/endpoints        {name, models?, force_model?, note?, enabled}
PUT    /admin/api/endpoints/{id}
DELETE /admin/api/endpoints/{id}
```
`models` is a comma-separated allow-list (globs allowed, e.g. `gpt-*`). `force_model` overrides it. A blank `api_key` on create auto-generates one; blank on update keeps the existing key.
