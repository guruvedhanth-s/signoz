# Reliability Agent

Define your SLOs as code.
Let the agent measure them against a stock SigNoz, refuse to trust numbers built on incomplete telemetry, and push the results back into SigNoz as metrics, dashboards, and alerts.

The reliability agent is a **third layer**: an independent service that runs beside an unmodified, Foundry-installed SigNoz and integrates only through its public API and OTLP.
Nothing in SigNoz is patched or forked, so the whole stack stays reproducible.

---

## Why

Traditional observability tells you whether a request failed and how slow it was.
It does not tell you whether your service is meeting a reliability objective, or whether the telemetry behind that objective can even be trusted.

This agent answers both:

1. Is the service meeting its SLO, with a real error budget and burn rate?
2. Is the telemetry complete enough to believe the answer?

The second question is the differentiator.
A dashboard computed from spans that are missing `service.name`, or from a service that stopped emitting a key metric, looks fine and lies.
The agent reports that case as `indeterminate` instead of a false green.

---

## Concepts

| Term | Meaning |
|---|---|
| SLI | Service Level Indicator: a measured ratio, `good / total`, in the range 0..1. |
| SLO | Service Level Objective: the target the SLI must meet (for example 99%). |
| Error budget | The failure the SLO allows: `1 - target`. A 99% SLO permits 1%. |
| Burn rate | How fast the budget is being spent. `1.0` exhausts it exactly by the end of the window; `20x` is twenty times too fast. |
| Trust state | `healthy`, `unhealthy`, or `indeterminate` (see below). |

### The trust state machine

```text
healthy       = telemetry is complete AND SLI >= target
unhealthy     = telemetry is complete AND SLI <  target
indeterminate = telemetry is incomplete, so the SLO cannot be trusted
```

An SLO with `requires_completeness: true` is gated: if its service's expected metrics are missing, the agent returns `indeterminate` and does not even compute the number.

---

## SLO-as-code

Everything the agent measures is declared in one YAML file.
See `slo.yaml` for the default and `slo.examples.yaml` for every SLI type.

```yaml
service: support-agent
environment: local

# Telemetry-completeness gate. SLOs with requires_completeness are trusted only
# when these metrics have data in SigNoz.
completeness:
  expected_metrics:
    - agent_requests_total
    - agent_success_total

slos:
  - name: successful-agent-runs
    description: Agent runs should complete without an error.
    type: ratio
    target: 99            # 99 or 0.99 both accepted
    window: 30d           # supports d, h, m, s
    good_query: agent_success_total
    total_query: agent_requests_total
    requires_completeness: true
```

### Top-level fields

| Field | Required | Description |
|---|---|---|
| `service` | yes | Service the SLOs describe. Labels the emitted metrics. |
| `environment` | no | Free-form environment tag. |
| `completeness.expected_metrics` | no | Metric names that must have data for gated SLOs to be trusted. Omit to trust all telemetry. |
| `slos` | yes | List of SLO definitions. |

### SLO fields

| Field | Applies to | Description |
|---|---|---|
| `name` | all | Unique name. Used as the metric/alert label and for idempotent dashboard/alert generation. |
| `type` | all | One of `ratio`, `latency_threshold`, `completeness`, `grounded_answers`. |
| `target` | all | Objective as a percentage (`99`) or fraction (`0.99`). |
| `window` | all | Evaluation window: `30d`, `7d`, `12h`, `45m`. |
| `good_query` / `total_query` | `ratio`, `completeness`, `grounded_answers` | Single-vector PromQL expressions. SLI is `good / total`. |
| `latency_metric` / `threshold_ms` | `latency_threshold` | Histogram metric name and latency budget. The agent builds the queries. |
| `requires_completeness` | all | When true, gate the SLO on `completeness.expected_metrics`. |

### SLI types

All four types reduce to a `good / total` ratio; the type gives semantic meaning and decides how the two queries are obtained.

| Type | SLI | How the queries are formed |
|---|---|---|
| `ratio` | `good / total` | Your `good_query` and `total_query` as written. |
| `latency_threshold` | requests under the budget / total | Built from the histogram: `sum(<metric>_bucket{le="<sec>"}) / sum(<metric>_count)`. |
| `completeness` | complete runs / total | Your `good_query` (a completeness marker) over `total_query`. |
| `grounded_answers` | grounded answers / total | Your `good_query` (a grounded-verdict count) over `total_query`. |

---

## Quick start

### 1. Bring up stock SigNoz (reproducible via Foundry)

From the repo root, where `casting.yaml` and `casting.yaml.lock` live:

```bash
curl -fsSL https://signoz.io/foundry.sh | bash   # installs foundryctl (once)
foundryctl cast -f casting.yaml                   # stock SigNoz + collector
```

### 2. Get an API key (one command)

```bash
cd reliability-agent
SIGNOZ_URL=http://localhost:8080 ./scripts/bootstrap.sh
# copy the printed line:
export SIGNOZ_API_KEY=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx=
```

### 3. Evaluate SLOs

```bash
go build -o agent ./cmd/agent
./agent slo --config slo.yaml
```

```text
SLO                    STATE    SLI     TARGET  BUDGET LEFT  BURN
successful-agent-runs  healthy  99.50%  99.00%  50.00%       0.50x
```

---

## Commands

### `agent slo`

Evaluate every SLO and print a report.

| Flag | Default | Description |
|---|---|---|
| `--config` | `slo.yaml` | SLO-as-code file. |
| `--signoz-url` | `$SIGNOZ_URL` or `http://localhost:8080` | SigNoz base URL. |
| `--api-key` | `$SIGNOZ_API_KEY` | Service-account API key. |
| `--emit` | off | Also emit `slo_*` metrics back into SigNoz over OTLP. |
| `--otlp-endpoint` | `$OTLP_ENDPOINT` or `localhost:4318` | Collector OTLP HTTP endpoint. |

### `agent generate`

Create (idempotently) the SLO dashboard, a notification channel, and a fast-burn alert per SLO.

| Flag | Default | Description |
|---|---|---|
| `--config` | `slo.yaml` | SLO-as-code file. |
| `--signoz-url` | `$SIGNOZ_URL` or `http://localhost:8080` | SigNoz base URL. |
| `--api-key` | `$SIGNOZ_API_KEY` | Service-account API key (admin, to manage channels). |

---

## What the agent writes into SigNoz

### Metrics (via `--emit`, over OTLP)

| Metric | Labels | Meaning |
|---|---|---|
| `slo_compliance` | `service`, `slo`, `window` | Measured SLI, 0..1. |
| `slo_state` | `service`, `slo` | `0` unhealthy, `1` healthy, `2` indeterminate. |
| `slo_error_budget_remaining` | `service`, `slo` | Remaining budget as a fraction. |
| `slo_burn_rate` | `service`, `slo`, `window` | Current burn rate. |

### Dashboard and alerts (via `generate`)

- A dashboard titled `SLOs & Error Budgets [reliability-agent]` with a panel per `slo_*` metric.
- A webhook notification channel (`reliability-agent-default`).
- A fast-burn threshold alert per SLO on `slo_burn_rate` (a plain metric threshold, not a formula, so it avoids upstream formula-alert bugs).

Generation is idempotent: re-running updates in place and never duplicates.

---

## How it integrates with SigNoz

All through public, unmodified SigNoz interfaces, authenticated with a service-account key (`SIGNOZ-API-KEY`):

| Purpose | Endpoint |
|---|---|
| Evaluate SLIs and check metric presence | `POST /api/v5/query_range` |
| Emit results | OTLP to the collector (`4317` / `4318`) |
| Create/update dashboard | `GET`/`POST`/`PUT /api/v1/dashboards` |
| Ensure notification channel | `GET`/`POST /api/v1/channels` |
| Create burn-rate alert | `POST /api/v2/rules` |

The read path is deterministic: the same telemetry yields the same score.
No LLM is involved in scoring.

---

## Run as a container

```bash
docker build -t reliability-agent:demo .
docker run --rm \
  -e SIGNOZ_URL=http://host.docker.internal:8080 \
  -e SIGNOZ_API_KEY=$SIGNOZ_API_KEY \
  reliability-agent:demo slo --config /slo.yaml
```

---

## Verified behavior

Run end to end against a stock, Foundry-installed SigNoz:

| Situation | State | SLI | Error budget left | Burn |
|---|---|---|---|---|
| No data | `indeterminate` | - | - | - |
| Expected metric missing | `indeterminate` | - | - | - |
| 0.5% errors, telemetry complete | `healthy` | 99.50% | 50.00% | 0.50x |
| 20% errors | `unhealthy` | 80.00% | -1900% | 20.00x |

The full arc:

```text
bad instrumentation  -> indeterminate  (telemetry cannot be trusted)
fix instrumentation  -> healthy        (real error budget and burn rate)
regression           -> unhealthy      (burn 20x, fast-burn alert fires)
```

See `DEMO.md` for the complete runbook.

---

## Layout

```text
cmd/agent/           CLI: slo, generate
internal/slo/        types, config, SLI evaluators, budget/burn math, trust state
                     machine, completeness gate, dashboard/alert builders (pure, tested)
internal/signoz/     HTTP client for stock SigNoz (query_range, dashboards, rules, channels)
internal/otlp/       OTLP emitter for slo_* metrics
scripts/bootstrap.sh one-shot setup on a fresh SigNoz
slo.yaml             default SLO config
slo.examples.yaml    every SLI type
```

## Roadmap

- Full Telemetry Health Auditor (Track A): richer completeness checks (attributes, trace trees) feeding the gate.
- Multi-window multi-burn-rate alerts (the math already exists in `internal/slo`).
