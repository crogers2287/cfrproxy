# cfrproxy — repo instructions

Go single-binary LLM proxy (OpenAI / Anthropic / Ollama dialects in and out, failover,
auto-router, share endpoints, admin WebUI). It is **production on this host**: the systemd user
unit `cfrproxy.service` execs `/home/crogers2287/cfrproxy/cfrproxy serve --addr :8420` straight
from this checkout, and every Hermes/omp/Claude Code/Codex harness on fred talks to it.

## Read first
- `timeline.md` — newest `REQ-NNN` at the top. Next number = top entry + 1. Log every substantive
  request there (template: `~/.claude/docs/timeline-format.md`). Only cfrproxy work belongs here;
  GPU / llama.cpp / Hermes-platform work goes in the homelab timeline (see global CLAUDE.md).
- `docs/architecture.md` for the request flow; `docs/share-endpoints.md` for `/e/{name}` policy.

## Build, test, deploy — use the Makefile, not hand-typed commands
```
make test       # go vet + go test (must be green before deploy)
make deploy     # test → dated rollback copy → build (temp+rename) → ~/.local/bin → restart → verify
make rollback   # newest cfrproxy.bak-* back into place + restart
make health     # /health 200, /admin/ 401, prints /api/version
cfrproxy version
```
- **Never** `go build -o cfrproxy .` while the service runs: it truncates the live executable
  (INCIDENT-002). `go build ./...` writes **no** binary at all, so it deploys nothing.
- Two binaries must match: `./cfrproxy` (what the unit runs) and `~/.local/bin/cfrproxy`
  (what `cfrproxy mcp` and shells use). `make deploy` updates both. Check with
  `curl -s localhost:8420/api/version` vs `git rev-parse --short HEAD`.
- Restart is `systemctl --user restart cfrproxy` (user unit, `Linger=yes`). Logs:
  `journalctl --user -u cfrproxy -n 50`.
- Keep at most 2 `cfrproxy.bak-*` binaries in the tree (`make clean`).

## Git
- This directory is the default cwd for **unrelated** homelab agents. Never `git add -A` /
  `git add .`; stage explicit paths. Commit `4ff12c3` is what happens otherwise.
- Branch `master` is the working branch; GitHub's default is `main`. Push both:
  `git push origin master master:main`.
- Conventional commits with scope: `feat(proxy):`, `fix(wire):`, `docs(timeline):`, `test(share):`.
- Never commit `*.bak*`, `*.log`, `*.pid`, `cfrproxy.db*`, `secret.key`, or the binary.

## Runtime layout
- Data: `~/.cfrproxy/` — `cfrproxy.db` (SQLite, WAL; open read-only with `sqlite3 -readonly`),
  `secret.key` (AES key for provider/share keys; losing it loses every key), `public_api_key.txt`,
  `cache-observability.jsonl` (per-request cache log), `kvwarm.json`, `cache/`.
- Tables that answer "what happened": `traces` (last 5000 requests, `req_snip`/`resp_snip`,
  provider, model, status, tokens), `usage_daily` (durable rollup, also `/admin/api/usage`),
  `endpoints` (share endpoints: `force_model`, `models` allow-list, `no_fallback`,
  `context_length`), `providers`, `settings` (JSON blobs: `model_map`, `global_fallback`,
  `model_pools`, `vision_models`, `auto_router`, `public_api_keys`).
- Sidecars: `cfrproxy-hermes-sync.timer` (every 2 min, `scripts/sync_hermes_cfrproxy.py`),
  `kvwarm.service` (`scripts/kvwarm.py`, prefix-cache warmer for the local llama.cpp/vLLM).

## Code map
- `internal/proxy/proxy.go` — `handleCore` is the whole request path (auth → body munging →
  compress/fusion/router → `ResolveModel` → candidate chain → retry loop → stream/translate →
  trace). Share-endpoint policy: `endpoint.go` + the `share-endpoint model policy` block.
- `internal/proxy/models.go` — `ResolveModel`: model_map → `provider/model` → alias → fuzzy.
- `internal/wire/` — dialect parse/build/stream per API (`openai`, `anthropic`, `responses`,
  `ollama`, `commandcode`). `internal/store/` — SQLite, single connection.
- `internal/api/` — `/admin/*` (Basic auth) + embedded `webui/index.html` (one vanilla-JS file).
- Root: `main.go` CLI, `launch.go` (`cfrproxy claude|codex|omp|opencode ...`), `mcp.go`
  (round-table MCP; HTTP client to the running server), `websearch.go`.

## Verifying a change end to end
```
curl -s localhost:8420/v1/models | head -c 300
curl -s localhost:8420/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"<provider>/<model>","messages":[{"role":"user","content":"ping"}],"max_tokens":8}'
sqlite3 -readonly ~/.cfrproxy/cfrproxy.db "select id,provider,model,status,err from traces order by id desc limit 5;"
```
Add the live output to the REQ entry as the verification stamp.
