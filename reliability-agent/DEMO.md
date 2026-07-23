# Reliability Agent - Demo Runbook

End-to-end demo on a **stock, Foundry-installed SigNoz**. No SigNoz source is modified.
Every step here has been run and verified locally.

## Prerequisites

- Docker
- `foundryctl` (`curl -fsSL https://signoz.io/foundry.sh | bash`)
- Go 1.25+ (only to run the agent/seed from source; a Docker image is also provided)

## 1. Bring up stock SigNoz (reproducible)

From the repo root (the `casting.yaml` + `casting.yaml.lock` live there):

```bash
foundryctl cast -f casting.yaml
```

This installs stock SigNoz + the OTel collector. SigNoz UI/API is on `http://localhost:8080`, OTLP on `4317/4318`.

## 2. Bootstrap access (one command)

Create the first admin, a service account with an admin role, and an API key:

```bash
cd reliability-agent
SIGNOZ_URL=http://localhost:8080 ./scripts/bootstrap.sh
# copy the printed line, e.g.:
export SIGNOZ_API_KEY=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx=
```

## 3. Seed telemetry

The example SLO reads `agent_success_total` / `agent_requests_total`. Seed them:

```bash
go run ../hackathon/seed --endpoint localhost:4318 --requests 10000 --errors 50
```

## 4. Evaluate the SLO and emit results back into SigNoz

```bash
go build -o agent ./cmd/agent   # or use the Docker image (step 7)

./agent slo --config slo.yaml --emit
```

Expected:

```text
SLO                    STATE    SLI     TARGET  BUDGET LEFT  BURN
successful-agent-runs  healthy  99.50%  99.00%  50.00%       0.50x

emitted slo.* metrics to localhost:4318
```

The `slo_compliance`, `slo_state`, `slo_error_budget_remaining`, and `slo_burn_rate`
metrics are now queryable inside SigNoz.

## 5. Generate the SLO dashboard and burn-rate alert

```bash
./agent generate --config slo.yaml
```

This creates (idempotently) an "SLOs & Error Budgets" dashboard, a notification
channel, and a fast-burn alert per SLO. Open the printed dashboard URL in SigNoz.

## 6. Show the trust states

```bash
# No data anywhere -> indeterminate (the engine refuses a false green):
./agent slo --config slo.yaml

# Failing telemetry -> unhealthy, burn 20x (> 14.4x fast threshold -> alert fires):
go run ../hackathon/seed --requests 10000 --errors 2000 && sleep 8
./agent slo --config slo.yaml
```

Verified results:

| Telemetry | state | SLI | error budget left | burn |
|---|---|---|---|---|
| none | indeterminate | - | - | - |
| 0.5% errors | healthy | 99.50% | 50.00% | 0.50x |
| 20% errors | unhealthy | 80.00% | -1900% | 20.00x |

## 7. Run the agent as a container (optional)

```bash
docker build -t reliability-agent:demo .
docker run --rm \
  -e SIGNOZ_URL=http://host.docker.internal:8080 \
  -e SIGNOZ_API_KEY=$SIGNOZ_API_KEY \
  reliability-agent:demo slo --config /slo.yaml
```

## What this proves

- Stock SigNoz, reproducible by Foundry (`casting.yaml` + `casting.yaml.lock`).
- The reliability agent reads telemetry, computes SLIs/error budgets/burn rates,
  refuses to trust SLOs computed from incomplete telemetry (`indeterminate`),
  and writes metrics, a dashboard, and a burn-rate alert back into SigNoz.
- All through public APIs with a service-account key. Zero SigNoz source changes.
