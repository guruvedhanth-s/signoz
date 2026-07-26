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

`SIDEKICK_WEBHOOK_URL` must be an address **SigNoz** can reach, and SigNoz
runs in Docker, so it depends on where the sidekick runs:

| Sidekick runs as | Use |
|---|---|
| a container joined to the Foundry network (step 2) | `http://sre-sidekick:8082/webhook` |
| `go run` on the host, which step 6 below does | `http://host.docker.internal:8082/webhook` |

Getting this wrong is quiet: the channel registers fine, the sidekick starts
fine, and the alert simply never arrives. If you skipped step 2, use the
`host.docker.internal` form.

```sh
eval "$(cat .signoz-api-key | sed 's/^/export SIGNOZ_API_KEY=/')"
# or just: export SIGNOZ_API_KEY=$(cat .signoz-api-key)
```

## 4. Run demo-agent

```sh
go run ./sre-sidekick/cmd/demo-agent --endpoint http://localhost:4318
```

Emits three-signal (traces/logs/metrics) telemetry for `support-agent` in
the `local` environment. Leave it healthy for now.

No `run` subcommand: `demo-agent` takes flags only, and a leading `run`
would make Go's flag parser silently discard `--endpoint` and everything
after it. See step 8, where the same mistake costs you the whole incident.

Give it a few minutes before step 5. Both SLOs should read `healthy` with an
SLI of `1.000` before you start recording, and the completeness gate needs
some history before it will trust the telemetry at all.

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
  --mcp-url http://localhost:8000/mcp --signoz-internal-url http://signoz-signoz-0:8080 \
  --slo-config sre-sidekick/examples/support-agent-slo-demo.yaml \
  --window 5m \
  --webhook-listen 0.0.0.0:8082
```

Dials Slack over Socket Mode and serves the alert webhook from step 3. Wait
for `slack socket connected` before continuing.

`--signoz-internal-url` is the URL the **MCP server** uses to reach SigNoz,
sent as the `X-SigNoz-URL` header, so it has to resolve inside the MCP
container - the in-network service name, not `localhost`, which inside that
container means the container itself.

`--slo-config` and `--window` are the recording setup: the demo SLO config's
5m window is what makes the step 9 recovery visible within a take, and
`--window 5m` keeps the evidence window shown in the Slack message
consistent with it. Omit both for a normal run and the 1h defaults apply.

Sessions live in memory. Restarting `watch` kills the buttons on any message
already in the channel ("this session was lost"), and it is also how you
clear a session between takes, since re-firing the same service
deduplicates into the existing thread rather than posting a new diagnosis.

## 7. Honesty beat

Drop a required metric on its own SLO (a separate SLO from the one used in
step 8, or restore it before step 8 - the indeterminate step must never
block the real incident). The sidekick posts to Slack that telemetry is
incomplete and offers no root cause and no decision buttons.

## 8. Real incident

```sh
go run ./sre-sidekick/cmd/demo-agent --buggy --error-rate 0.8 --endpoint http://localhost:4318
```

Two details in that command are not cosmetic; both were found by running
this demo for real.

**There is no `run` subcommand.** `demo-agent` takes flags only. Go's `flag`
package stops parsing at the first non-flag argument, so
`demo-agent run --buggy` silently discards `--buggy` and every flag after
it, prints no error, and runs the agent **healthy**. Nothing breaks, no
alert fires, and there is nothing to diagnose - which looks exactly like a
broken sidekick rather than a mistyped command.

**Use `--error-rate 0.8`, not bare `--buggy`.** `--buggy` alone means a 100%
error rate, and in that mode `agent_grounded_answers_total` never increments
at all. Once the SLO window contains nothing but failing traffic the
good-path metric has zero samples, the completeness gate cannot tell
"everything failed" from "telemetry stopped", and it correctly refuses to
diagnose - so the demo degrades to `indeterminate` a few minutes after the
bad deploy. At 80% the good counter keeps incrementing, the gate stays
trusted indefinitely, and the burn rate is higher anyway.

The error rate rises, the short-window burn-rate alert fires, SigNoz posts
to the webhook, and the sidekick grounds on the SLO, gathers evidence via
MCP, runs the DeepSeek RCA loop, and posts a grounded diagnosis to Slack -
then stops for human review.

To trigger the incident on demand instead of waiting for SigNoz's
alertmanager, post the same payload it sends directly to the webhook - see
`RECORDING-SCRIPT.md`. Everything downstream is the production path.

## 9. Recovery

Revert `demo-agent` to healthy (re-run step 4), then click **Verify
recovery** on the approved message in Slack.

How long recovery takes is set by the SLO window, and it is longer than it
looks: the failing period keeps depressing the SLI until it ages out. With
the 1h window in `examples/support-agent-slo.yaml` the service is still
unhealthy an hour after the fix. `examples/support-agent-slo-demo.yaml` uses
a 5m window, where recovery lands about ten minutes after the fix.

Verify reporting "not recovered" before then is correct behaviour, not a
failure - the check is deterministic and will not claim success early.
Distinguish it from "the verify engine is not connected", which means the
check could not run at all.

## Cleanup

```sh
docker compose -f pours/deployment/compose.yaml down -v
```
