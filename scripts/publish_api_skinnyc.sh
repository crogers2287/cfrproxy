#!/bin/bash
# Publish api.skinnyc.pro -> cfrproxy (fred:8420) via the home NPM on Tower.
# Prereq: NPM admin API healthy (docker restart NginxProxyManager on Tower
# fixes the stale-FUSE SQLITE_IOERR state). Idempotent.
set -euo pipefail

NPM="${NPM_URL:-http://192.168.1.5:7818}"
DOMAIN="api.skinnyc.pro"
FWD_HOST="192.168.1.195"
FWD_PORT="8420"
LE_EMAIL="${LE_EMAIL:-crogers2287@gmail.com}"

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
  echo "proxy host exists (id $EXISTING) — updating"
  curl -sf "${AUTH[@]}" -X PUT "$NPM/api/nginx/proxy-hosts/$EXISTING" \
    -H 'Content-Type: application/json' -d "$BODY" | head -c 200
else
  echo "creating proxy host $DOMAIN -> $FWD_HOST:$FWD_PORT (+ Let's Encrypt)"
  curl -sf "${AUTH[@]}" -X POST "$NPM/api/nginx/proxy-hosts" \
    -H 'Content-Type: application/json' -d "$BODY" | head -c 200
fi
echo
sleep 8
echo "verify:"
curl -s -m 15 "https://$DOMAIN/health" && echo " <- health OK"
echo "data plane requires a key from outside (expect 401):"
curl -s -m 15 -o /dev/null -w '%{http_code}\n' "https://$DOMAIN/v1/chat/completions" \
  -H 'Content-Type: application/json' -d '{"model":"x","messages":[]}'
