# Deployment

cfrproxy is one static binary. Build it, run `cfrproxy serve`, done. This doc covers running it as a service and exposing it safely.

## Build

```bash
make build     # writes cfrproxy.tmp then renames it into place
make install   # atomic copy to ~/.local/bin/cfrproxy
```

Do not run `go build -o cfrproxy .` on a checkout whose binary a running service execs:
the linker truncates the live executable first (INCIDENT-002 in `timeline.md`). `make build`
builds to a temp file and renames, which leaves the running process on the old inode. The
Makefile also stamps the build with the git commit; `cfrproxy version` and `GET /api/version`
report it.

## Run as a systemd user service

```ini
# ~/.config/systemd/user/cfrproxy.service
[Unit]
Description=cfrproxy universal LLM proxy
After=network.target

[Service]
ExecStart=%h/.local/bin/cfrproxy serve --addr :8420
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
```

```bash
systemctl --user daemon-reload
systemctl --user enable --now cfrproxy
loginctl enable-linger "$USER"   # survive logout / reboot
```

Redeploy with one command:

```bash
make deploy      # vet + test, keep a dated rollback copy, build, install, restart,
                 # then wait for /health 200, /admin/ 401 and /api/version == HEAD
make rollback    # restore the newest cfrproxy.bak-* and restart
```

Data lives in `~/.cfrproxy/` (override with `--data`). On first run it prints a generated WebUI password; reset it with `cfrproxy passwd --pass NEW`.

## Binding

- `cfrproxy serve` binds `:8420` on all interfaces by default; use `--addr 127.0.0.1:8420` to keep it loopback-only.
- The data plane (`/v1/...`, `/api/chat`) is keyless for **direct** connections from a trusted network, so local harnesses just work. Trusted by default: loopback, RFC1918 (`10/8`, `172.16/12`, `192.168/16`), the Tailscale range (`100.64/10`) and their IPv6 equivalents. Override with a comma-separated list: `cfrproxy config set trusted_cidrs "192.168.1.0/24,100.64.0.0/10"`.
- Any other peer, and any request that arrives with `X-Forwarded-For` / `X-Real-IP` (i.e. through a reverse proxy), must send a public API key or admin credentials. If no keys are configured such requests are refused, not waved through.

## Exposing it publicly — read this first

**Do not port-forward the raw port to the internet.** A keyless data plane would let anyone burn your subscriptions.

cfrproxy has a built-in gate: a request that arrives through a reverse proxy (identified by `X-Forwarded-For` / `X-Real-IP`) or from a peer outside `trusted_cidrs` requires an API key; direct trusted-network requests stay keyless. Set one or more keys:

```bash
cfrproxy config set public_api_keys "$(openssl rand -hex 24)"
# multiple keys allowed, comma-separated
```

Then callers coming through your reverse proxy must send `Authorization: Bearer <key>` (or `x-api-key: <key>`). Put cfrproxy behind a TLS-terminating reverse proxy (nginx, Caddy, Nginx Proxy Manager, Cloudflare Tunnel). A helper for Nginx Proxy Manager is in [`scripts/publish_via_npm.sh`](../scripts/publish_via_npm.sh).

The WebUI/management API at `/admin/` always requires HTTP basic auth, independently of the data-plane gate. State-changing admin requests must be `application/json` and are refused when a browser marks them `Sec-Fetch-Site: cross-site`, so cached credentials cannot be replayed by a form on another site. On first run the generated admin password is written to `~/.cfrproxy/admin-password.txt` (mode 0600) rather than printed into the journal.

## Health check

`GET /health` → `{"status":"ok"}` — unauthenticated, for load balancers and uptime monitors.
