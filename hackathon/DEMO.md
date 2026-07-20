# SLO Engine - Demo Runbook

This runbook reproduces the full SLO engine demo end to end.
Every step here has been run and verified locally against a live SigNoz, ClickHouse, and OTel collector.

## Prerequisites

Docker, and Go 1.25.x (the repo pins `go 1.25.7`; on newer toolchains prefix commands with `GOTOOLCHAIN=go1.25.7`).

## 1. Start the data plane

```bash
make devenv-up          # ClickHouse + OTel collector (+ zookeeper)
```

## 2. Build and run the server

```bash
GOTOOLCHAIN=go1.25.7 go build -o /tmp/signoz-community ./cmd/community/

SIGNOZ_SQLSTORE_SQLITE_PATH=/tmp/signoz.db \
SIGNOZ_WEB_ENABLED=false \
SIGNOZ_TOKENIZER_JWT_SECRET=secret \
SIGNOZ_ALERTMANAGER_PROVIDER=signoz \
SIGNOZ_TELEMETRYSTORE_PROVIDER=clickhouse \
SIGNOZ_TELEMETRYSTORE_CLICKHOUSE_DSN=tcp://127.0.0.1:9000 \
SIGNOZ_TELEMETRYSTORE_CLICKHOUSE_CLUSTER=cluster \
SIGNOZ_SLO_CONFIG_PATH=$PWD/hackathon/examples/support-agent.slo.yaml \
/tmp/signoz-community server
```

The `SIGNOZ_SLO_CONFIG_PATH` variable points the engine at the SLO-as-code file.

## 3. Create the first admin and get a token

```bash
curl -s -X POST localhost:8080/api/v1/register -H 'Content-Type: application/json' \
  -d '{"orgDisplayName":"Demo","orgName":"demo","name":"Admin","email":"admin@demo.io","password":"Password@123"}'

# note the orgId from the response, then:
TOKEN=$(curl -s -X POST localhost:8080/api/v2/sessions/email_password \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@demo.io","password":"Password@123","orgId":"<ORG_ID>"}' \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['accessToken'])")
```

## 4. The demo: watch the SLO state flip

### 4a. No telemetry yet -> indeterminate

```bash
curl -s localhost:8080/api/v1/slo -H "Authorization: Bearer $TOKEN"
# state: indeterminate  (the engine refuses to report a number it cannot compute)
```

### 4b. Seed healthy telemetry -> healthy

```bash
GOTOOLCHAIN=go1.25.7 go run ./hackathon/seed --requests 10000 --errors 50
curl -s localhost:8080/api/v1/slo -H "Authorization: Bearer $TOKEN"
# state: healthy, sli 0.995, errorBudgetRemaining 0.5, burnRate 0.5x
```

### 4c. Seed failing telemetry -> unhealthy

```bash
GOTOOLCHAIN=go1.25.7 go run ./hackathon/seed --requests 10000 --errors 2000
sleep 8   # allow ingestion
curl -s localhost:8080/api/v1/slo -H "Authorization: Bearer $TOKEN"
# state: unhealthy, sli 0.8, errorBudgetRemaining negative, burnRate 20x
```

## 5. Generate the SLO dashboard

```bash
curl -s -X POST localhost:8080/api/v1/slo/generate -H "Authorization: Bearer $TOKEN"
# creates "SLOs & Error Budgets [slo-generated]" with four panels.
# Re-running is idempotent: it updates the same dashboard, never duplicates.
```

## 6. The frontend page

The SLO page is served at `/slo` (route `ROUTES.SLO`).
It renders the score, trust-state badge (green/red/gold), SLI, target, remaining error budget, and burn rate per SLO.
An `indeterminate` SLO shows a gold badge with a tooltip explaining the SLO cannot be trusted.

## Verified results

| Telemetry | state | SLI | error budget left | burn rate |
|---|---|---|---|---|
| none | indeterminate | - | - | - |
| 0.5% errors | healthy | 0.995 | 0.50 | 0.5x |
| 20% errors | unhealthy | 0.80 | -19 (overspent) | 20x |
