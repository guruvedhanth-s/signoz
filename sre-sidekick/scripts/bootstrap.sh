#!/usr/bin/env bash
# Bootstraps sre-sidekick's SigNoz access (PRD section 19): creates a
# service account, grants it the signoz-admin role, mints an API key, and
# registers the webhook notification channel the alert-driven detect stage
# (PRD section 12) listens on. Idempotent by name: re-running skips any
# step whose resource already exists rather than erroring or duplicating.
#
# Every HTTP call and response shape here was verified live against a
# throwaway SigNoz instance (v0.133.0) before being committed - this is
# not guessed from reading source alone.
#
# Requires: curl, jq. Requires the SigNoz root user to already exist -
# casting.yaml sets SIGNOZ_USER_ROOT_ENABLED/EMAIL/PASSWORD/ORG_NAME, which
# creates it automatically the first time SigNoz starts.
#
# Usage:
#   SIGNOZ_URL=http://localhost:8080 \
#   SIGNOZ_ADMIN_EMAIL=admin@example.com \
#   SIGNOZ_ADMIN_PASSWORD='...' \
#   SIGNOZ_ORG_NAME=sre-sidekick-demo \
#   SIDEKICK_WEBHOOK_URL=http://sre-sidekick:8082/webhook \
#   SIDEKICK_WEBHOOK_SECRET='...' \
#   ./scripts/bootstrap.sh
#
# Prints `export SIGNOZ_API_KEY=...` on success (eval this, or read the
# key from --out-file) and writes the same key to --out-file
# (default: .signoz-api-key, gitignored) so a second run can detect the
# service account/key already exist and skip re-minting one.

set -euo pipefail

SIGNOZ_URL="${SIGNOZ_URL:-http://localhost:8080}"
SIGNOZ_ADMIN_EMAIL="${SIGNOZ_ADMIN_EMAIL:?SIGNOZ_ADMIN_EMAIL is required - the root user email, matching casting.yaml}"
SIGNOZ_ADMIN_PASSWORD="${SIGNOZ_ADMIN_PASSWORD:?SIGNOZ_ADMIN_PASSWORD is required}"
SIGNOZ_ORG_NAME="${SIGNOZ_ORG_NAME:-sre-sidekick-demo}"
SERVICE_ACCOUNT_NAME="${SIDEKICK_SERVICE_ACCOUNT_NAME:-sre-sidekick}"
API_KEY_NAME="${SIDEKICK_API_KEY_NAME:-sre-sidekick-key}"
CHANNEL_NAME="${SIDEKICK_CHANNEL_NAME:-sre-sidekick-webhook}"
WEBHOOK_URL="${SIDEKICK_WEBHOOK_URL:-http://sre-sidekick:8082/webhook}"
WEBHOOK_SECRET="${SIDEKICK_WEBHOOK_SECRET:?SIDEKICK_WEBHOOK_SECRET is required - the shared secret detect.Handler checks (PRD section 20)}"
OUT_FILE="${1:-.signoz-api-key}"

for bin in curl jq; do
  command -v "$bin" >/dev/null 2>&1 || { echo "bootstrap: $bin is required" >&2; exit 1; }
done

api() {
  # api METHOD PATH [BODY] - always sends Authorization when $ACCESS_TOKEN
  # is set, always expects JSON, and fails loudly on a non-2xx status
  # rather than silently continuing with an empty/partial response.
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -X "$method" "${SIGNOZ_URL}${path}" -H "Content-Type: application/json")
  [ -n "${ACCESS_TOKEN:-}" ] && args+=(-H "Authorization: Bearer ${ACCESS_TOKEN}")
  [ -n "$body" ] && args+=(-d "$body")
  local response status
  response="$(curl "${args[@]}" -w $'\n%{http_code}')"
  status="${response##*$'\n'}"
  response="${response%$'\n'*}"
  if [ -z "$status" ] || [ "$status" -lt 200 ] || [ "$status" -ge 300 ]; then
    echo "bootstrap: $method $path failed (HTTP ${status:-?}): $response" >&2
    exit 1
  fi
  echo "$response"
}

echo "bootstrap: waiting for SigNoz at ${SIGNOZ_URL} ..." >&2
for _ in $(seq 1 60); do
  if curl -sS -o /dev/null -w '' "${SIGNOZ_URL}/api/v1/health" 2>/dev/null; then
    break
  fi
  sleep 2
done
curl -sS -o /dev/null "${SIGNOZ_URL}/api/v1/health" || { echo "bootstrap: SigNoz never became reachable at ${SIGNOZ_URL}" >&2; exit 1; }

echo "bootstrap: resolving org id for ${SIGNOZ_ORG_NAME} ..." >&2
CONTEXT="$(api GET "/api/v2/sessions/context?email=$(jq -rn --arg e "$SIGNOZ_ADMIN_EMAIL" '$e|@uri')&ref=$(jq -rn --arg r "$SIGNOZ_URL" '$r|@uri')")"
ORG_ID="$(echo "$CONTEXT" | jq -r --arg name "$SIGNOZ_ORG_NAME" '.data.orgs[] | select(.name == $name) | .id')"
if [ -z "$ORG_ID" ] || [ "$ORG_ID" = "null" ]; then
  echo "bootstrap: no org named '${SIGNOZ_ORG_NAME}' found - is SIGNOZ_USER_ROOT_ENABLED set on the SigNoz container? response: $CONTEXT" >&2
  exit 1
fi
echo "bootstrap: org id = ${ORG_ID}" >&2

echo "bootstrap: logging in as ${SIGNOZ_ADMIN_EMAIL} ..." >&2
SESSION="$(api POST "/api/v2/sessions/email_password" "$(jq -n \
  --arg email "$SIGNOZ_ADMIN_EMAIL" --arg password "$SIGNOZ_ADMIN_PASSWORD" --arg orgId "$ORG_ID" \
  '{email: $email, password: $password, orgId: $orgId}')")"
ACCESS_TOKEN="$(echo "$SESSION" | jq -r '.data.accessToken')"
[ -n "$ACCESS_TOKEN" ] && [ "$ACCESS_TOKEN" != "null" ] || { echo "bootstrap: login did not return an access token: $SESSION" >&2; exit 1; }

if [ -f "$OUT_FILE" ]; then
  echo "bootstrap: ${OUT_FILE} already exists, skipping service account/key creation." >&2
  echo "bootstrap: delete it (and the service account in SigNoz) to force re-provisioning." >&2
  API_KEY="$(cat "$OUT_FILE")"
else
  echo "bootstrap: creating service account '${SERVICE_ACCOUNT_NAME}' ..." >&2
  SA="$(api POST "/api/v1/service_accounts" "$(jq -n --arg name "$SERVICE_ACCOUNT_NAME" '{name: $name}')")"
  SA_ID="$(echo "$SA" | jq -r '.data.id')"

  echo "bootstrap: granting signoz-admin ..." >&2
  ROLES="$(api GET "/api/v1/roles")"
  ADMIN_ROLE_ID="$(echo "$ROLES" | jq -r '.data[] | select(.name == "signoz-admin") | .id')"
  [ -n "$ADMIN_ROLE_ID" ] && [ "$ADMIN_ROLE_ID" != "null" ] || { echo "bootstrap: no signoz-admin role found in org ${ORG_ID}" >&2; exit 1; }
  api POST "/api/v1/service_account_roles" "$(jq -n \
    --arg sa "$SA_ID" --arg role "$ADMIN_ROLE_ID" \
    '{serviceAccountId: $sa, roleId: $role}')" >/dev/null

  echo "bootstrap: minting API key '${API_KEY_NAME}' ..." >&2
  KEY="$(api POST "/api/v1/service_accounts/${SA_ID}/keys" "$(jq -n --arg name "$API_KEY_NAME" '{name: $name, expiresAt: 0}')")"
  API_KEY="$(echo "$KEY" | jq -r '.data.key')"
  [ -n "$API_KEY" ] && [ "$API_KEY" != "null" ] || { echo "bootstrap: key creation did not return a key: $KEY" >&2; exit 1; }

  printf '%s' "$API_KEY" > "$OUT_FILE"
  chmod 600 "$OUT_FILE"
fi

echo "bootstrap: registering webhook channel '${CHANNEL_NAME}' -> ${WEBHOOK_URL} ..." >&2
EXISTING="$(api GET "/api/v1/channels")"
if echo "$EXISTING" | jq -e --arg name "$CHANNEL_NAME" '.data[]? | select(.name == $name)' >/dev/null 2>&1; then
  echo "bootstrap: channel '${CHANNEL_NAME}' already exists, skipping." >&2
else
  # The inbound shape (POST) and outbound shape (GET, seen above) are
  # asymmetric for http_headers: POST requires the extra "Headers" wrapper
  # key (encoding/json decodes into a Go struct field named Headers with
  # no json tag), GET's response flattens it away. Verified live - do not
  # simplify this without retesting against a running SigNoz.
  api POST "/api/v1/channels" "$(jq -n \
    --arg name "$CHANNEL_NAME" --arg url "$WEBHOOK_URL" --arg secret "$WEBHOOK_SECRET" \
    '{name: $name, webhook_configs: [{url: $url, send_resolved: true, http_config: {http_headers: {Headers: {"X-Sidekick-Webhook-Secret": {values: [$secret]}}}}}]}')" >/dev/null
fi

echo "bootstrap: done." >&2
echo "export SIGNOZ_API_KEY=${API_KEY}"
