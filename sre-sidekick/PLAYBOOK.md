# Local deployment playbook

Everything needed to run SRE Sidekick on your own machine, from an empty Docker
to a Slack thread containing a grounded diagnosis.

Follow it top to bottom the first time. Each step ends with a check, because
almost every failure in this stack is silent - a wrong label matches no series,
a wrong URL is never reached, and both look exactly like "the agent is broken".

Roughly 20 minutes, most of it waiting for containers.

## What you end up with

| Component | Where | What it is |
|---|---|---|
| SigNoz + MCP | `localhost:8080`, `localhost:8000` | stock, cast by Foundry |
| demo-app | `localhost:8090` | the observed application, with fault sliders |
| sre-sidekick `watch` | `localhost:8082` | the agent: alert receiver + Slack adapter |
| Slack | your workspace | where diagnoses arrive |

Substitute your own application for demo-app once the loop works. Do not start
there - if something is wrong you will not know whether it is your
instrumentation or the setup.

## Prerequisites

- **Docker** with Compose, and enough headroom for ClickHouse (~4 GB).
- **Go 1.25+**.
- **[`foundryctl`](https://github.com/SigNoz/foundry)**: `curl -fsSL https://signoz.io/foundry.sh | bash`
- **A Slack app** with a bot token (`xoxb-…`) and an app-level token (`xapp-…`),
  Socket Mode enabled, Interactivity enabled, and the bot invited to your
  on-call channel. Scopes: `chat:write`, `app_mentions:read`, `commands`.
- **An OpenRouter API key** for the RCA reasoner.

Slack tokens are the most common blocker. Get them first; everything else is
scriptable.

## 1. Cast SigNoz

```sh
export SIGNOZ_ADMIN_EMAIL='admin@example.com'
export SIGNOZ_ADMIN_PASSWORD='ChangeMe123!'   # must satisfy SigNoz's policy
foundryctl cast -f casting.yaml
```

This is stock SigNoz plus `mcp.spec.enabled: true`. Nothing in `pours/` is
hand-edited - `cast` regenerates it, which is why it is gitignored.

**Check:**

```sh
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/api/v1/health   # 200
```

Roughly five seconds after `cast` returns.

> `docker ps` will show `signoz-mcp` as **unhealthy**. Ignore it. The image's
> healthcheck shells out to `wget`, which is not in the image; the server itself
> is fine and answers `401` on `/mcp` when unauthenticated. Fixing it would mean
> editing the Foundry-generated deployment, which the PRD forbids.

## 2. Provision access

```sh
export SIDEKICK_WEBHOOK_SECRET="$(openssl rand -hex 32)"

SIGNOZ_URL=http://localhost:8080 \
SIGNOZ_ORG_NAME=sre-sidekick-demo \
SIDEKICK_WEBHOOK_URL='http://host.docker.internal:8082/webhook' \
sre-sidekick/scripts/bootstrap.sh .signoz-api-key
```

Creates a service account, mints an API key, and registers the notification
channel SigNoz posts alerts to.

**`SIDEKICK_WEBHOOK_URL` must be reachable from inside Docker**, and which value
you want depends on where the sidekick runs:

| Sidekick runs as | Use |
|---|---|
| `go run` on the host (this playbook) | `http://host.docker.internal:8082/webhook` |
| a container on the Foundry network | `http://sre-sidekick:8082/webhook` |

Get this wrong and nothing errors: the channel registers, the agent starts, and
the alert simply never arrives.

**Check:** the script prints `bootstrap: done.` and writes the key.

```sh
export SIGNOZ_API_KEY="$(cat .signoz-api-key)"
```

If your key file already contains `export SIGNOZ_API_KEY=…`, source it instead.
Quote it either way - the value contains `/` and `=`.

## 3. Environment

Everything reads from the environment. Keep these in one file you source, and
keep that file out of git.

| Variable | Purpose |
|---|---|
| `SIGNOZ_URL` | SigNoz base URL, e.g. `http://localhost:8080` |
| `SIGNOZ_API_KEY` | service-account key from step 2 |
| `SIGNOZ_MCP_URL` | `http://localhost:8000/mcp` |
| `SIGNOZ_INTERNAL_URL` | how the **MCP container** reaches SigNoz: `http://signoz-signoz-0:8080` |
| `SIDEKICK_WEBHOOK_SECRET` | shared secret the alert receiver checks |
| `OPENROUTER_API_KEY` | RCA reasoner |
| `SLACK_BOT_TOKEN` | `xoxb-…` |
| `SLACK_APP_TOKEN` | `xapp-…` |

`SIGNOZ_INTERNAL_URL` catches people out: it is resolved *inside the MCP
container*, so `localhost` there means the MCP container itself, not SigNoz.

## 4. Configure

`configs/sidekick.yaml` ships with working defaults. Secrets are never stored in
it - the `*_env` keys name environment variables.

Set `notify.slack.default_channel` to your channel. The rest can wait:

| Section | Default | Change when |
|---|---|---|
| `storage.driver` | `memory` | you want sessions to survive a restart (`file` or `postgres`) |
| `notify.slack.authorization` | commented out (permissive) | more than one person can click Approve |
| `notify.slack.enable_mutations` | `false` | you want the agent to change things in SigNoz |
| `llm.limits` | generous | you care about spend |
| `presentation` | PRD 13.4 defaults | you want different conclusion thresholds |

**Mutations are off by default and should stay off** until authorization is
configured. With no roles set, any channel member can approve.

## 5. Run an application to observe

```sh
cd demo-app
go run ./cmd/payments &
go run ./cmd/api &
```

Open <http://localhost:8090> and click **Start continuous traffic**.

Leave it running. **An SLO is a rate**: a window with no requests is not
healthy, it is `indeterminate` with "no data in SLO window".

**Check:** orders appear in the log panel, confirmed in ~43 ms.

Using your own application instead? It needs `service.name` and
`deployment.environment` resource attributes, `trace_id` on log records, and a
profile plus SLO config under `examples/`. Copy `demo-checkout-api.yaml` and
`demo-checkout-slo.yaml` as templates.

## 6. Preflight

```sh
cd sre-sidekick
go run ./cmd/reliability-agent preflight --profile examples/demo-checkout-api.yaml
```

**Check:** six `[ OK ]` lines. There is no fallback path by design - fix
whatever it names before continuing, because every later failure will be harder
to attribute.

## 7. Verify the deterministic engine before adding AI

Do this before starting the agent. If the numbers are wrong, no amount of
reasoning on top will help.

```sh
go run ./cmd/reliability-agent slo --config examples/demo-checkout-slo.yaml --output json
go run ./cmd/reliability-agent audit --profile examples/demo-checkout-api.yaml --lookback 10m
```

**Check:** both SLOs `healthy` with `sli` near 1.0, and audit rules passing.

Wait a few minutes after starting traffic. Metric ingestion lags about a minute,
and the completeness gate needs history before it will trust anything.

## 8. Start the agent

```sh
go run ./cmd/reliability-agent watch \
  --config configs/sidekick.yaml \
  --signoz-url "$SIGNOZ_URL" \
  --mcp-url "$SIGNOZ_MCP_URL" \
  --signoz-internal-url "$SIGNOZ_INTERNAL_URL" \
  --slo-config examples/demo-checkout-slo.yaml \
  --window 5m \
  --webhook-listen 127.0.0.1:8082
```

**Check:** `slack socket connected`, and port 8082 listening.

You will also see `Slack authorization is not configured; interactive decisions
are permissive`. That is correct for a first run and is telling you the truth
about who can approve.

## 9. Write results back into SigNoz

```sh
go run ./cmd/reliability-agent generate \
  --config examples/demo-checkout-slo.yaml \
  --webhook-url "$SIDEKICK_WEBHOOK_URL"
```

Creates the dashboard, the notification channel, and burn-rate alert rules for
each SLO and tier. Idempotent - safe to re-run.

The dashboard is fed by the sidekick's own metrics, which are emitted only when
you run:

```sh
go run ./cmd/reliability-agent slo --config examples/demo-checkout-slo.yaml --emit-otlp
go run ./cmd/reliability-agent audit-watch --profile examples/demo-checkout-api.yaml --interval 5s --emit-otlp
```

`slo --emit-otlp` is **one-shot**. For a continuously populated dashboard - and
for SigNoz's own alert rules to fire on `slo_mwmb_firing` - run it on a loop:

```sh
while true; do
  go run ./cmd/reliability-agent slo --config examples/demo-checkout-slo.yaml --emit-otlp
  sleep 30
done
```

There is no `slo-watch` daemon yet. `audit-watch` is the continuous one.

**Check:** open the dashboard. Panels populate about a minute after emission -
querying immediately returns zero series even though ClickHouse already has the
samples. The three `telemetry_quality_*` panels are fed by `audit-watch
--emit-otlp` specifically.

## 10. Drive an incident

Break it from the demo-app UI: drag **`payments_errors`** to **80%**.

Use 80%, not 100%. At 100% the good-path metric stops being emitted entirely, so
the completeness gate cannot distinguish "everything failed" from "telemetry
stopped" and correctly refuses to diagnose. That refusal is a feature worth
demonstrating deliberately - just not by accident.

Wait 2-3 minutes, confirm the SLO broke, then trigger the loop:

```sh
curl -X POST http://127.0.0.1:8082/webhook \
  -H "X-Sidekick-Webhook-Secret: $SIDEKICK_WEBHOOK_SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{
        "alertname":"SLO fast burn - checkout-availability",
        "service":"checkout-api","environment":"local",
        "severity":"critical","slo":"checkout-availability","tier":"fast"}}]}'
```

This is exactly what a SigNoz notification channel posts, so everything
downstream is the production path. It just starts when you say so.

**Check:** a diagnosis appears in Slack within ~6 seconds, with grounding
fields, evidence links, and Approve / Decline / Close session buttons.

## 11. Close the loop

1. **Click Approve** - the message rewrites, buttons retire, a thread reply
   confirms nothing was executed, and a **Verify recovery** button appears.
2. **Click Verify recovery** before fixing anything - it reports *not
   recovered*. That is the deterministic check being honest.
3. **Clear the faults** in the UI, wait ~10 minutes for the failing period to
   age out of the 5m window, then **Verify** again - recovered.

You have now run the full loop: detect, ground, diagnose, communicate, act,
verify.

## Operating modes

| Command | What it does |
|---|---|
| `preflight` | checks every dependency, names what is missing |
| `audit` | one-shot Track A telemetry-quality audit |
| `audit-watch` | continuous Track A auditing, with alerting and optional OTLP |
| `slo` | one-shot Track B evaluation, optional OTLP emission |
| `generate` | creates/updates the dashboard, channel, and burn alerts |
| `diagnose` | one-shot RCA for a service, printed as JSON |
| `watch` | the daemon: Slack adapter plus the alert webhook receiver |
| `server` | HTTP API for profiles, audits, and SLO evaluation |

`diagnose` is the best debugging tool when Slack misbehaves: it runs the same
pipeline and prints the whole diagnosis, including why it refused.

## Talking to it in Slack

With `watch` running, mention the bot in your channel:

- `@sidekick how is checkout-api doing?`
- `@sidekick what's my burn rate?`
- `@sidekick is the telemetry trustworthy?`

Questions resolve to a fixed set of intents, each backed by a deterministic
function that computes the answer from SigNoz. The model chooses which to call
and phrases the result; **it never produces the numbers**. An unrecognised
question gets a capability list rather than an improvised answer.

Inside an incident thread, follow-ups are answered against that incident's
frozen diagnosis instead.

## When something goes wrong

| Symptom | Cause | Fix |
|---|---|---|
| `preflight` fails on MCP | wrong `SIGNOZ_INTERNAL_URL` | must resolve inside the MCP container |
| Webhook returns 401 | secret mismatch | must equal what `watch` started with |
| Webhook 200, no Slack message | same service inside the 10-min dedup window | Close session, or restart `watch` |
| `indeterminate`, "no data in SLO window" | no traffic | generate load |
| `indeterminate`, telemetry not trusted | a fault at 100%, or a metric missing | drop to 80%; check the gate reason |
| `indeterminate`, "too few error samples" | fewer than 3 errors yet | wait, re-fire |
| `0 of N dependencies have data` | label spelling mismatch | check `service_label` / `environment_label` against the real series |
| Dashboard panels empty | ingestion lag | wait ~1 minute |
| Buttons do nothing | Interactivity disabled | enable in Slack app settings |
| Buttons say "session was lost" | `watch` restarted | in-memory sessions; use the newest message |
| Alert rules never fire | SLO metrics not emitted continuously | run `slo --emit-otlp` on a loop |

The gate reason is almost always the fastest diagnosis. `2 of 2 dependencies
have data` versus `0 of 2` tells you immediately whether the problem is your
query or your labels.

## Before running this anywhere real

This is an MVP. Read this section as a checklist, not a disclaimer.

- **State is in memory** unless `storage.driver` is set. A restart loses open
  incidents and forgets approvals.
- **Single instance only.** No leader election, so two replicas double-diagnose
  and double-spend.
- **Anyone in the channel can approve** until `authorization` is configured.
- **Mutations are off by default.** Leave them off until authorization is set
  and you have an audit trail you trust.
- **The audit trail is `slog`** unless a store is configured.
- **The webhook listener has no timeouts** and no health endpoint.
- **The 5m SLO windows in the demo configs are a demo concession.** Real
  services want 30d.

`hackathon/REBUILD-LOG.md` records defects found by running this against a live
SigNoz, including two still open.
