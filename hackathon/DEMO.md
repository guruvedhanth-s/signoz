# SRE Sidekick demo runbook

Reproduces the PRD section 23 demo: a stock, Foundry-cast SigNoz + MCP, the
`sre-sidekick` container joined to its network, and the full alert-driven
incident loop ending in a Slack thread.

Every command below is meant to be run from the repo root unless noted.

## 0. Prerequisites

- Docker and Docker Compose.
- [`foundryctl`](https://github.com/SigNoz/foundry) (`curl -fsSL https://signoz.io/foundry.sh | bash`).
- A Slack app (bot token `SLACK_BOT_TOKEN`, app-level token `SLACK_APP_TOKEN`
  with Socket Mode enabled) invited to the on-call channel.
- An OpenRouter API key (`OPENROUTER_API_KEY`).

## 1. Cast stock SigNoz + MCP

```sh
export SIGNOZ_ADMIN_EMAIL=admin@example.com
export SIGNOZ_ADMIN_PASSWORD='ChangeMe123!'   # meets SigNoz's password policy
foundryctl cast -f casting.yaml
```

This is the stock `docker/compose` casting with one change from foundryctl's
own example: `mcp.spec.enabled: true`. Nothing in `pours/` or the SigNoz
deployment itself is hand-edited (PRD non-goal 2) - `pours/` is regenerated
by `cast` every time, which is why it is gitignored rather than committed.

SigNoz comes up at `http://localhost:8080`, the MCP server at
`http://localhost:8000/mcp`.

## 2. Join the sidekick to the Foundry network

```sh
docker build -t sre-sidekick:latest sre-sidekick/
docker network connect $(docker compose -f pours/deployment/compose.yaml \
  config --format json | jq -r '.networks | keys[0]') sre-sidekick 2>/dev/null || true
```

This is a command on our own container, not a change to the Foundry-cast
deployment (PRD non-goal 2). If you are not running the sidekick as a
container yet (e.g. running `go run ./cmd/reliability-agent` on the host
during development), skip this step and use `http://localhost:8080` /
`http://localhost:8000/mcp` directly instead of the in-network service
names below.

## 3. Bootstrap SigNoz access

```sh
export SIDEKICK_WEBHOOK_SECRET="$(openssl rand -hex 32)"
SIGNOZ_URL=http://localhost:8080 \
SIGNOZ_ADMIN_EMAIL="$SIGNOZ_ADMIN_EMAIL" \
SIGNOZ_ADMIN_PASSWORD="$SIGNOZ_ADMIN_PASSWORD" \
SIGNOZ_ORG_NAME=sre-sidekick-demo \
SIDEKICK_WEBHOOK_URL=http://sre-sidekick:8082/webhook \
SIDEKICK_WEBHOOK_SECRET="$SIDEKICK_WEBHOOK_SECRET" \
sre-sidekick/scripts/bootstrap.sh
```

This logs in as the root user casting.yaml created, then creates (or
reuses, if already present) a `sre-sidekick` service account with the
`signoz-admin` role, mints an API key, and registers the webhook
notification channel `sre-sidekick-webhook` pointing at the sidekick's
`watch` receiver with `SIDEKICK_WEBHOOK_SECRET` as a request header - the
same secret `detect.Handler` verifies (PRD sections 12, 19, 20).

```sh
eval "$(cat .signoz-api-key | sed 's/^/export SIGNOZ_API_KEY=/')"
# or just: export SIGNOZ_API_KEY=$(cat .signoz-api-key)
```

## 4. Run demo-agent

```sh
go run ./sre-sidekick/cmd/demo-agent run --endpoint http://localhost:4318
```

Emits three-signal (traces/logs/metrics) telemetry for `support-agent` in
the `local` environment. Leave it healthy for now.

## 5. Preflight

```sh
go run ./sre-sidekick/cmd/reliability-agent preflight \
  --signoz-url http://localhost:8080 --api-key "$SIGNOZ_API_KEY" \
  --mcp-url http://localhost:8000/mcp --signoz-internal-url http://localhost:8080
```

All six checks (SigNoz, MCP, OpenRouter key, Slack bot token, Slack app
token, demo-agent telemetry) must pass before continuing - there is no
fallback path by design (PRD section 19).

## 6. Start watch

```sh
go run ./sre-sidekick/cmd/reliability-agent watch \
  --signoz-url http://localhost:8080 --api-key "$SIGNOZ_API_KEY" \
  --mcp-url http://localhost:8000/mcp --signoz-internal-url http://localhost:8080 \
  --webhook-listen 0.0.0.0:8082
```

Dials Slack over Socket Mode and serves the alert webhook from step 3.

## 7. Honesty beat

Drop a required metric on its own SLO (a separate SLO from the one used in
step 8, or restore it before step 8 - the indeterminate step must never
block the real incident). The sidekick posts to Slack that telemetry is
incomplete and offers no root cause and no decision buttons.

## 8. Real incident

```sh
go run ./sre-sidekick/cmd/demo-agent run --buggy --endpoint http://localhost:4318
```

The error rate rises, the short-window burn-rate alert fires, SigNoz posts
to the webhook, and the sidekick grounds on the SLO, gathers evidence via
MCP, runs the DeepSeek RCA loop, and posts a grounded diagnosis to Slack -
then stops for human review.

## 9. Recovery

Revert `demo-agent` to healthy (re-run step 4). The short-window burn rate
falls within a couple of minutes; approve the diagnosis in Slack.

## Cleanup

```sh
docker compose -f pours/deployment/compose.yaml down -v
```
