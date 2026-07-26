# Demo environment rebuild log

A running record of tearing the SigNoz deployment down to bare volumes and
casting it again, so the recorded demo runs against a known-clean environment.

Every command here was actually run, in this order.
Timestamps are UTC.
Anything that failed is recorded as it failed, with the fix underneath, because
the point of this log is that the next person can reproduce the environment
without rediscovering the same problems.

Credentials are never written into this file.
They are read from the shell, exactly as `casting.yaml` and `bootstrap.sh`
expect.

## Why rebuild at all

The deployment had been up for 12 hours to 5 days depending on the container,
accumulating telemetry from unrelated experiments.
For a recorded demo that turns on specific SLO and burn-rate numbers, stale data
is a liability: it moves the numbers between takes and makes the run
irreproducible.
A fresh cast makes the take deterministic and proves the PRD section 19 claim
that the environment is reproducible by Foundry from a committed `casting.yaml`.

## Pre-teardown state, 2026-07-26

Captured before anything was destroyed.

### Containers

| Container | Compose project | Status before teardown |
|---|---|---|
| `signoz-signoz-0` | `signoz` | Up 12 hours (healthy) |
| `signoz-telemetrystore-clickhouse-0-0` | `signoz` | Up 12 hours (healthy) |
| `signoz-telemetrykeeper-clickhousekeeper-0` | `signoz` | Up 12 hours (healthy) |
| `signoz-ingester-1` | `signoz` | Up 12 hours |
| `signoz-metastore-postgres-0` | `signoz` | Up 5 days (healthy) |
| `signoz-mcp` | `signoz` | Up 4 days (reported unhealthy, see below) |
| `ods_postgres` | `neuskale-oms` | Up 42 hours (healthy), **not touched** |

### Volumes destroyed

All four are in compose project `signoz`.
No volume outside that project was touched.

- `signoz-telemetrystore-0-0-data` - ClickHouse data: all traces, logs, metrics.
- `signoz-metastore-postgres-0-data` - org, root user, API keys, dashboards, alert rules, notification channels.
- `signoz-telemetrykeeper-0-data` - ClickHouse Keeper coordination state.
- `signoz-telemetrystore-user-scripts` - ClickHouse user scripts.

### Endpoint reachability before teardown

| Endpoint | Result |
|---|---|
| `http://127.0.0.1:8080/api/v1/health` | `200` |
| `http://127.0.0.1:8000/mcp` | `401` (auth required, so the server is live) |
| `127.0.0.1:4318` | open (OTLP HTTP) |
| `127.0.0.1:8082` | closed (the sidekick's webhook, not running yet) |

## Findings carried forward

These were discovered while inspecting the old environment.
They are recorded here because they survive the rebuild and would otherwise be
rediscovered the hard way.

### 1. The MCP container's "unhealthy" status is a false alarm

`signoz-mcp` reported `unhealthy` with a failing streak of 37098.
Every health probe failed identically:

```
OCI runtime exec failed: exec failed: unable to start container process:
exec: "wget": executable file not found in $PATH
```

The healthcheck shells out to `wget`, which is not present in the image.
The server itself was serving correctly the whole time, answering `401` on
`/mcp` as it should when unauthenticated.

**Do not spend demo time on this.**
It is a defect in the stock image's healthcheck, not in the deployment, and
fixing it would mean editing the Foundry-generated deployment, which PRD
non-goal 2 forbids.
Expect the same red status after the rebuild and ignore it.

### 2. SigNoz returned a 500 on trace-key lookup

On 2026-07-24 the MCP server logged an upstream failure from SigNoz itself:

```
signoz_search_traces -> http://signoz-signoz-0:8080/api/v5/query_range
status 500: {"status":"error","error":{"type":"internal","code":"internal",
"message":"failed to get traces keys"}}
```

This matters for the demo: RCA evidence gathering calls `signoz_search_traces`,
and a 500 there degrades the diagnosis to `indeterminate`.
The agent handles that correctly rather than fabricating a result, but an
`indeterminate` diagnosis is not the demo we want to record.

Whether it recurs on a fresh cast is an open question to re-check after the
rebuild, once telemetry has been flowing long enough for trace keys to exist.
A plausible cause is querying for trace keys before any traces have been
ingested, which the rebuild will reproduce by definition in its first minutes.

### 3. Tooling verified present before teardown

Checked deliberately before destroying anything, so the rebuild could not strand
the environment:

- `foundryctl` at `/Users/sabari/.local/bin/foundryctl`.
- `casting.yaml` committed at the repo root, stock `docker/compose` casting with
  `mcp.spec.enabled: true` as its only change.
- No `.signoz-api-key` file existed, so no local credential was lost.

## Rebuild

Run in this order, all from the repo root.
Credentials come from the shell, never from a committed file.

```sh
docker compose -f pours/deployment/compose.yaml -p signoz down -v --remove-orphans
foundryctl cast -f casting.yaml
SIDEKICK_WEBHOOK_URL='http://host.docker.internal:8082/webhook' \
  sre-sidekick/scripts/bootstrap.sh /path/outside/the/repo/.signoz-api-key
cd sre-sidekick && go run ./cmd/demo-agent          # healthy baseline
go run ./cmd/reliability-agent preflight --profile examples/demo-agent.yaml
```

Notes on what differs from `DEMO.md`:

- The webhook channel points at `host.docker.internal:8082`, not
  `sre-sidekick:8082`, because the sidekick runs on the host via `go run`
  rather than as a container joined to the Foundry network.
  Use the container name instead when running the sidekick in Docker.
- `bootstrap.sh` writes `export SIGNOZ_API_KEY=<value>` to the file named by
  its first argument.
  The value contains `/` and `=`, so it must be single-quoted when appended to
  an env file, otherwise the shell tries to execute it.

Timings observed: SigNoz answered `/api/v1/health` with `200` about five seconds
after `cast` returned, and `bootstrap.sh` completed in a few seconds after that.

`preflight` passed all six checks on the fresh environment:

```
[ OK ] SigNoz
[ OK ] SigNoz MCP server
[ OK ] OpenRouter API key
[ OK ] Slack bot token
[ OK ] Slack app token
[ OK ] demo-agent telemetry
preflight: all checks passed
```

## Three bugs the live run found

None of these were visible in the test suite, because every test stubs the
telemetry source.
All three were found by running the real `diagnose` command against real SigNoz,
and each one on its own was enough to make the RCA demo impossible.

### Bug 1: the metrics query is rejected by the SigNoz v5 API

`TelemetrySource.querySignal` sent the same aggregation shape for every signal:

```json
"aggregations": [{"expression": "count()"}]
```

That is valid for logs and traces.
For metrics, SigNoz rejects the whole request:

```
unknown field "expression" in query spec for MetricAggregation
```

The RCA evidence gate asked for the metrics signal even though
`evaluateSnapshot` never reads `snapshot.Metrics`; it judges sufficiency on
logs, error spans, and exceptions only.
A failing query clears `Snapshot.QueryComplete`, and the gate treats an
incomplete query as insufficient evidence, so **every** diagnosis returned
`indeterminate`.

Fix: `evidenceProfile` now requests traces and logs only, the signals the gate
actually judges.

Still outstanding: the metrics query itself is still wrong for any caller that
does want metrics.
`examples/checkout-api.yaml` declares a metrics signal, so a Track A audit of
that profile still fails its metrics query.
Not on the demo path, so it is recorded here rather than fixed under deadline.

### Bug 2: the traces query used an order key that does not exist

The same function ordered every signal by `timestamp` then `id`.
Logs have an `id` field; traces do not, and SigNoz rejects the query outright:

```
field `id` not found
suggestions: valid keys are `serviceName`, `dbOperation`, `service.name`,
`deployment.environment`, `flags`, `status_message`, ...
```

So the traces query failed on every call, which again cleared `QueryComplete`.

Fix: the `id` tiebreaker is sent for the logs signal only.

### Bug 3: a capped sample was reported as a failed query

Both `logs.go` and `telemetry.go` did this when the row limit was reached:

```go
if limit > 0 && rowCount >= limit {
    snapshot.Partial = true
    snapshot.QueryComplete = false   // wrong
}
```

Those two flags mean different things.
`Partial` means "there may be more records than we fetched".
`QueryComplete` means "the query itself ran", and the RCA gate reads it as "we
cannot know what evidence exists here" and refuses to diagnose.

The effect was perverse: a service emitting enough telemetry to fill the row
limit could never be diagnosed, while a quiet service could.
More data made the product less able to help.

Fix: hitting the limit sets `Partial` only.
Track A is unaffected because it reads `Snapshot.Complete()`, which is
`QueryComplete && !Partial` and still returns false for a capped sample.

### After the three fixes

The same command that had returned `indeterminate` on every attempt cleared the
evidence gate and returned the correct verdict for a healthy service:

```
too few error samples to trust a pattern (0 found, 3 required)
```

That is the product working: the service really was healthy, so there was
nothing to diagnose, and it said so instead of inventing a root cause.

## The full loop, proven live

With `demo-agent --buggy` running, `diagnose` returned `status: diagnosed` in
about six seconds: SLO unhealthy at 3.64x burn and -2.64 error budget, root
cause "repeated timeouts in tool.search_kb after 5 seconds, causing agent.run to
fail", ten trace evidence links.
The numbers are computed deterministically and the prose is DeepSeek, which is
the boundary PRD section 5 non-goal 5 requires.

### Driving the alert path deterministically

The demo does not have to wait for SigNoz's alertmanager to fire on its own
schedule.
The detect webhook is the same code path the real alert takes, so posting an
alertmanager-shaped payload to it triggers the genuine loop instantly, which is
what makes a recorded take repeatable:

```sh
curl -X POST http://127.0.0.1:8082/webhook \
  -H "X-Sidekick-Webhook-Secret: $SIDEKICK_WEBHOOK_SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{
        "alertname":"SLO fast burn - grounded-answers",
        "service":"support-agent","environment":"local",
        "severity":"critical","slo":"grounded-answers","tier":"fast"}}]}'
```

Nothing is faked here.
The payload is what an alertmanager notification channel sends; everything
downstream - dedup, RCA, MCP evidence gathering, the LLM call, Block Kit
rendering, session creation - is the production path.

Start `watch` first:

```sh
go run ./cmd/reliability-agent watch \
  --config configs/sidekick.yaml \
  --signoz-url "$SIGNOZ_URL" --mcp-url "$SIGNOZ_MCP_URL" \
  --signoz-internal-url "$SIGNOZ_INTERNAL_URL" \
  --slo-config examples/support-agent-slo.yaml \
  --webhook-listen 127.0.0.1:8082
```

### Result

```
slack socket connected
slack message posted correlation_id=slack-support-agent-local-... kind=diagnosed
  service=support-agent channel=C0BKT4DBNCE message_ts=... attempts=1
incident session opened correlation_id=... thread_ts=...
```

Elapsed from webhook to posted message: about seven seconds.

The rendered message contains a header, the six grounding fields, a root cause
citing evidence IDs, evidence as SigNoz deep links capped at five of ten with a
context note, the advisory action labelled as advisory, Approve / Decline /
Close session buttons, and a correlation-ID footer.

### Environment facts worth keeping

- Slack workspace `Error404`, bot user `sre_sidekick`, channel `C0BKT4DBNCE`.
- The bot token lacks the `channels:read` scope.
  That does not affect posting, but `conversations.list` fails with
  `missing_scope`, so channels cannot be enumerated with this token.
- Evidence deep links point at `http://localhost:8080`, which resolves only on
  the machine running the demo.
  Fine for a recorded take; a shared environment would need `--signoz-url` set
  to a reachable host, since that flag is what gets rewritten into the links.
- Error budget and burn rate keep degrading while `--buggy` runs, so the numbers
  differ between takes.
  They read 3.64x shortly after the switch and 8.6x a few minutes later.
  Restart the demo-agent healthy, let it settle, then switch to buggy a known
  number of minutes before recording if a specific number is wanted on screen.
