# reliability-agent

A third-layer reliability service for **stock, unmodified SigNoz**.
It evaluates SLOs (and, later, audits telemetry quality) by talking to SigNoz over its public API - no SigNoz source changes, so the deployment stays reproducible by Foundry.

This is an independent Go module: it does not import any SigNoz package.

## What it does today (Track B)

- Reads SLO definitions as code (`slo.yaml`).
- Computes the ratio SLI, error budget, and burn rate by querying stock SigNoz at `POST /api/v5/query_range`.
- Applies the trust-aware state machine: `healthy` / `unhealthy` / `indeterminate`.
- Prints a report.
- `--emit`: writes `slo_*` metrics back into SigNoz over OTLP.
- `generate`: creates (or idempotently updates) an SLO dashboard in SigNoz.

## Auth

Create a service account and API key in SigNoz (Settings -> Service Accounts, admin/editor role), then pass the key.
The agent sends the header `SIGNOZ-API-KEY: <key>`.

## Run

```bash
go build -o agent ./cmd/agent

SIGNOZ_URL=http://localhost:8080 \
SIGNOZ_API_KEY=<service-account-api-key> \
./agent slo --config slo.yaml --emit

# create/update the SLO dashboard inside SigNoz:
./agent generate
```

Example output:

```text
SLO report for service "support-agent"

SLO                    STATE    SLI     TARGET  BUDGET LEFT  BURN
successful-agent-runs  healthy  99.50%  99.00%  50.00%       0.50x
```

## Verified end to end

Against a live stock SigNoz + ClickHouse + collector, with a real service-account API key:

| Telemetry | state | SLI | budget left | burn |
|---|---|---|---|---|
| none | indeterminate | - | - | - |
| 0.5% errors | healthy | 99.50% | 50.00% | 0.50x |
| 20% errors | unhealthy | 80.00% | -1900% | 20.00x |

Seed telemetry with `go run ../hackathon/seed --requests 10000 --errors 50`.

## Layout

```text
cmd/agent/           CLI entrypoint (slo subcommand)
internal/slo/        types, config, budget/burn math, state machine, engine (pure, unit-tested)
internal/signoz/     HTTP client for stock SigNoz (query_range; SIGNOZ-API-KEY auth)
```

## Roadmap

- Generated burn-rate alerts via `POST /api/v2/rules`.
- More SLI types (latency threshold, completeness, grounded answers).
- Telemetry Health Auditor (Track A) + the real completeness gate.
