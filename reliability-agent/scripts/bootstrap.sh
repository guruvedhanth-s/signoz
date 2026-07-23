#!/usr/bin/env bash
#
# bootstrap.sh - one-shot setup for the reliability agent against a fresh,
# Foundry-installed SigNoz. Creates the first admin, a service account with an
# admin role, a notification channel, and prints a service-account API key.
#
# Usage:
#   SIGNOZ_URL=http://localhost:8080 ./scripts/bootstrap.sh
#
# On success it prints:  export SIGNOZ_API_KEY=<key>
#
set -euo pipefail

URL="${SIGNOZ_URL:-http://localhost:8080}"
EMAIL="${ADMIN_EMAIL:-admin@demo.io}"
PASSWORD="${ADMIN_PASSWORD:-Password@123}"

# get <json-path> reads stdin JSON and prints data[...] following dotted keys.
get() { python3 -c "import sys,json;d=json.load(sys.stdin)['data']
for k in sys.argv[1].split('.'):
    d=d[k] if k else d
print(d)" "$1"; }

echo "==> registering first admin ($EMAIL) on $URL"
REG=$(curl -s -X POST "$URL/api/v1/register" -H "Content-Type: application/json" \
  -d "{\"orgDisplayName\":\"Demo\",\"orgName\":\"demo\",\"name\":\"Admin\",\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")
ORG_ID=$(echo "$REG" | get "orgId" 2>/dev/null || true)
if [ -z "${ORG_ID:-}" ] || [ "$ORG_ID" = "None" ]; then
  echo "!! register did not return an orgId (already set up?). Response:"; echo "$REG"
  echo "   This script targets a FRESH SigNoz install."; exit 1
fi
echo "    orgId=$ORG_ID"

echo "==> logging in"
TOKEN=$(curl -s -X POST "$URL/api/v2/sessions/email_password" -H "Content-Type: application/json" \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\",\"orgId\":\"$ORG_ID\"}" | get "accessToken")

echo "==> creating service account 'reliability-agent'"
SA_ID=$(curl -s -X POST "$URL/api/v1/service_accounts" -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" -d '{"name":"reliability-agent"}' | get "id")

echo "==> assigning admin role"
ADMIN_ROLE=$(curl -s "$URL/api/v1/roles" -H "Authorization: Bearer $TOKEN" \
  | python3 -c "import sys,json;[print(r['id']) for r in json.load(sys.stdin)['data'] if r.get('name')=='signoz-admin']")
curl -s -o /dev/null -X POST "$URL/api/v1/service_accounts/$SA_ID/roles" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d "{\"id\":\"$ADMIN_ROLE\"}"

echo "==> creating API key"
KEY=$(curl -s -X POST "$URL/api/v1/service_accounts/$SA_ID/keys" -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" -d '{"name":"agent-key","expiresAt":1893456000}' | get "key")

echo ""
echo "Done. Use this API key with the agent:"
echo ""
echo "  export SIGNOZ_API_KEY=$KEY"
