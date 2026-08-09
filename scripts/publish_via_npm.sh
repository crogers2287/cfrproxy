#!/bin/bash
# Publish a public hostname -> cfrproxy via Nginx Proxy Manager (optional).
# Creates/updates the proxy host with a Let's Encrypt cert. Idempotent.
#
# Set these for your environment:
#   NPM_URL   NPM admin API base (e.g. http://npm-host:81)
#   DOMAIN    public hostname (must already point DNS at your NPM host)
#   FWD_HOST  host running cfrproxy (e.g. 127.0.0.1 or a LAN IP)
#   FWD_PORT  cfrproxy port (default 8420)
#   LE_EMAIL  Let's Encrypt contact email
#
# SECURITY: before exposing cfrproxy publicly, set an API key so the data
# plane requires auth from outside your LAN:
#   cfrproxy config set public_api_keys "$(openssl rand -hex 24)"
set -euo pipefail

NPM="${NPM_URL:?set NPM_URL, e.g. http://npm-host:81}"
DOMAIN="${DOMAIN:?set DOMAIN, e.g. api.example.com}"
FWD_HOST="${FWD_HOST:-127.0.0.1}"
FWD_PORT="${FWD_PORT:-8420}"
LE_EMAIL="${LE_EMAIL:?set LE_EMAIL}"

read -rp "NPM identity (email): " NPM_ID
read -rsp "NPM password: " NPM_SECRET; echo

TOK=$(curl -sf -X POST "$NPM/api/tokens" -H 'Content-Type: application/json' \
  -d "{\"identity\":\"$NPM_ID\",\"secret\":\"$NPM_SECRET\"}" | python3 -c 'import json,sys;print(json.load(sys.stdin)["token"])')
echo "authenticated."
AUTH=(-H "Authorization: Bearer $TOK")

EXISTING=$(curl -sf "${AUTH[@]}" "$NPM/api/nginx/proxy-hosts" | python3 -c "
import json,sys
for h in json.load(sys.stdin):
    if '$DOMAIN' in h.get('domain_names',[]):
        print(h['id']); break")

BODY=$(cat <<JSON
{"domain_names":["$DOMAIN"],
 "forward_scheme":"http","forward_host":"$FWD_HOST","forward_port":$FWD_PORT,
 "allow_websocket_upgrade":true,"block_exploits":true,"caching_enabled":false,
 "access_list_id":"0","certificate_id":"new","ssl_forced":true,
 "http2_support":true,"hsts_enabled":false,"hsts_subdomains":false,
 "meta":{"letsencrypt_email":"$LE_EMAIL","letsencrypt_agree":true,"dns_challenge":false},
 "advanced_config":"","locations":[]}
JSON
)

if [ -n "$EXISTING" ]; then
  echo "updating proxy host id $EXISTING"
  curl -sf "${AUTH[@]}" -X PUT "$NPM/api/nginx/proxy-hosts/$EXISTING" -H 'Content-Type: application/json' -d "$BODY" | head -c 200
else
  echo "creating $DOMAIN -> $FWD_HOST:$FWD_PORT (+ Let's Encrypt)"
  curl -sf "${AUTH[@]}" -X POST "$NPM/api/nginx/proxy-hosts" -H 'Content-Type: application/json' -d "$BODY" | head -c 200
fi
echo; sleep 8
curl -s -m 15 "https://$DOMAIN/health" && echo " <- health OK"
