# Deployment

cfrproxy is one static binary. Build it, run `cfrproxy serve`, done. This doc covers running it as a service and exposing it safely.

## Build

```bash
go build -o cfrproxy .
# optionally put it on PATH
install -m755 cfrproxy ~/.local/bin/cfrproxy
```

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

Data lives in `~/.cfrproxy/` (override with `--data`). On first run it prints a generated WebUI password; reset it with `cfrproxy passwd --pass NEW`.

## Binding

- `cfrproxy serve` binds `:8420` on all interfaces by default; use `--addr 127.0.0.1:8420` to keep it loopback-only.
- The data plane (`/v1/...`, `/api/chat`) is keyless — anything that can reach the port can use it. That's intentional for LAN use so local harnesses just work.

## Exposing it publicly — read this first

**Do not port-forward the raw port to the internet.** A keyless data plane would let anyone burn your subscriptions.

cfrproxy has a built-in gate: when a request arrives through a reverse proxy (identified by `X-Forwarded-For` / `X-Real-IP`), it requires an API key; direct LAN requests stay keyless. Set one or more keys:

```bash
cfrproxy config set public_api_keys "$(openssl rand -hex 24)"
# multiple keys allowed, comma-separated
```

Then callers coming through your reverse proxy must send `Authorization: Bearer <key>` (or `x-api-key: <key>`). Put cfrproxy behind a TLS-terminating reverse proxy (nginx, Caddy, Nginx Proxy Manager, Cloudflare Tunnel). A helper for Nginx Proxy Manager is in [`scripts/publish_via_npm.sh`](../scripts/publish_via_npm.sh).

The WebUI/management API at `/admin/` always requires HTTP basic auth, independently of the data-plane gate.

## Health check

`GET /health` → `{"status":"ok"}` — unauthenticated, for load balancers and uptime monitors.
