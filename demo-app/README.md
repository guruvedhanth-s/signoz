# demo-app

The application SRE Sidekick observes. A browser frontend, an HTTP API, and a
downstream payments dependency, instrumented with OpenTelemetry so its traces,
logs and metrics arrive in SigNoz.

It exists because `sre-sidekick/cmd/demo-agent` - a synthetic loop emitting one
signal triple every two seconds - proved the pipeline but limited what could be
demonstrated or validated. `demo-agent` remains: it is a zero-dependency
workload for CI and `preflight`.

## What it is for

Three services, because two is the minimum that makes root-cause analysis
interesting and three lets a fault propagate:

| Service | Role |
|---|---|
| `checkout-web` | the browser. Starts the trace on a click. |
| `checkout-api` | the backend. Owns the SLOs. |
| `payments` | the dependency. Where faults are injected. |

With one service, a diagnosis can only say "checkout is failing". With a
dependency, the useful answer is "checkout is failing **because payments is**",
which is what an on-call engineer needs. That is verified: with
`payments_errors` injected, the RCA agent reported *"the payments service
returning 500 Internal Server Error responses, which propagate as Bad Gateway
errors to the client"*.

## Run it

Locally, against a SigNoz already running on `localhost:4318`:

```sh
go run ./cmd/payments &
go run ./cmd/api &
open http://localhost:8090
```

Or as containers on the Foundry network:

```sh
docker compose -f demo-app/compose.yaml up --build
```

Then click **Start continuous traffic**. This matters: an SLO is a rate, so a
burn rate over a window with no requests is not "healthy", it is `indeterminate`
with "no data in SLO window". The first SLO evaluation here failed exactly that
way because traffic had stopped nine minutes earlier.

## Faults

Injected from the UI, each with a **rate** rather than on/off:

| Mode | Breaks | Effect |
|---|---|---|
| `payments_errors` | availability | payments returns 500, api returns 502 |
| `payments_latency` | latency only | payments takes 1.4s and succeeds |
| `payments_timeout` | availability | payments hangs 6s, api gives up at 3s |
| `logs_missing_trace_id` | *telemetry quality* | logs lose trace correlation |

Two of these deserve explanation.

**`payments_latency` breaks the latency SLO and leaves availability healthy**,
because every request still succeeds. That separation is the point: a single "is
it up" SLO would call a service that answers every request in 1.4 seconds
perfectly healthy. Verified: availability `healthy` at SLI 1.0 while latency
degrades.

**`logs_missing_trace_id` breaks nothing about the service** - it degrades the
evidence *about* the service. That is Track A's whole thesis, and it is what the
demo's "honesty beat" needs: telemetry quality is not service health.

### Rates are not decoration

Default 0.8, and the reason is load-bearing. At **1.0**, the good-path metric
stops being emitted entirely, so an SLO's "good" counter has no samples in the
window, the completeness gate cannot distinguish "everything failed" from
"telemetry stopped", and it **correctly refuses to diagnose**.

So: use ~0.8 for a diagnosable incident, and 1.0 deliberately when you want to
demonstrate the agent refusing rather than guessing.

## Telemetry contracts

Profiles and SLO configs live with the sidekick:

- `sre-sidekick/examples/demo-checkout-api.yaml` - Track A profile. All six
  rules pass against live data.
- `sre-sidekick/examples/demo-checkout-slo.yaml` - availability (`ratio`) and
  latency (`latency_threshold`) SLOs, 5m windows to match the demo config used
  elsewhere.

One trace spans all three services, verified in ClickHouse:

```
checkout-web  checkout            (root)
└─ checkout-api  POST /api/checkout
   └─ checkout-api  checkout.process
      └─ payments  POST /charge
         └─ payments  payments.authorize
```

## Three things that were only discoverable live

Recorded here because each looked like working code and produced empty results.

**Browser spans are relayed, not posted directly.** OTLP/HTTP accepts JSON, so
the browser could export straight to the collector - but the collector would
need CORS headers, and configuring that means editing the Foundry-generated
deployment. `POST /api/traces` relays instead: same-origin, no collector change,
still real OTLP.

**`deployment.environment` is emitted three ways on purpose.** semconv v1.34
renamed it to `deployment.environment.name`, but every existing SLO config here
and SigNoz's own spanmetrics filter on `deployment.environment`, and some
queries use bare `environment`. Emitting one spelling means a filter written
against another silently matches nothing - which is precisely how the first SLO
config reported "0 of 2 dependencies have data" while the metrics were plainly
in ClickHouse.

**The histogram needs explicit bucket boundaries.** The SDK's defaults are the
millisecond-shaped set (5, 10, 25 … 10000), but
`http.server.request.duration` is defined in *seconds*. With those defaults
every real request lands in the first bucket, the next boundary is at 5 seconds,
and a 1s latency threshold has no `le=1` bucket to compare against - so the SLI
cannot be computed and the SLO reports `indeterminate` while the data looks
present. The semconv-recommended boundaries are set explicitly for that reason.

## Deploy markers

Every service emits a `deploy` span on startup carrying `service.version`,
`deploy.marker=true`, and the environment. It is the cheapest possible change
event: no CI integration, and it lands in the same trace store the RCA agent
already reads, so deploy correlation (#49) can query it.

Stamp a real version with `--build-arg VERSION=$(git rev-parse --short HEAD)`.
