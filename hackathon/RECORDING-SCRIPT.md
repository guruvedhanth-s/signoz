# Recording script

A shot-by-shot runbook for the recorded demo, with every wait measured on a live
run rather than guessed.

This version drives `demo-app/` - a browser frontend, a `checkout-api` backend,
and a `payments` dependency. The earlier version drove
`sre-sidekick/cmd/demo-agent`, a synthetic loop; that still exists and is still
used by CI and `preflight`, but it is no longer what you record. A real app gives
you a browser-to-payments trace, two SLOs that break independently, and a
diagnosis that names a downstream service.

Read `REBUILD-LOG.md` first if the environment is not already up.

## The two things that make this recordable

**Faults come from the UI, with a rate.** No restarting processes off-camera.
Drag a slider and the next request fails.

**The incident is triggered by posting an alertmanager payload at the detect
webhook**, instead of waiting for SigNoz's alertmanager to fire on its own
schedule. That is not a shortcut around the product: it is byte-for-byte what a
SigNoz notification channel sends, and everything downstream - dedup, grounding,
MCP evidence gathering, the LLM call, Block Kit rendering, session creation - is
the same code the real alert drives. It just means a take starts when you say so.

## Measured timings

All observed on 2026-07-26 against a freshly cast SigNoz.

| Step | Wait | What is happening |
|---|---|---|
| `payments_errors` at 80% to availability unhealthy | ~2-3 min | measured SLI 0.52, burn 48.3x at 2m40s |
| Webhook fired to Slack message posted | ~6 sec | RCA, MCP evidence, DeepSeek, render |
| Metric emitted to queryable in SigNoz | ~1 min | ingestion lag, see the failure table |
| Fix applied to SLOs healthy again | ~10 min | the failing period ages out of the 5m window |

The last row is the only one needing a cut in the edit.

## Setup, before recording

Four terminals; keep 1-3 off camera.

```sh
# 1: the demo app
cd demo-app
go run ./cmd/payments &
go run ./cmd/api &

# 2: the sidekick, on the demo app's SLO config
cd sre-sidekick
go run ./cmd/reliability-agent watch \
  --config configs/sidekick.yaml \
  --signoz-url "$SIGNOZ_URL" --mcp-url "$SIGNOZ_MCP_URL" \
  --signoz-internal-url "$SIGNOZ_INTERNAL_URL" \
  --slo-config examples/demo-checkout-slo.yaml \
  --window 5m \
  --webhook-listen 127.0.0.1:8082

# 3: dashboards and alert rules, plus the metrics that fill them
go run ./cmd/reliability-agent generate \
  --config examples/demo-checkout-slo.yaml --webhook-url "$SIDEKICK_WEBHOOK_URL"
go run ./cmd/reliability-agent slo \
  --config examples/demo-checkout-slo.yaml --emit-otlp
go run ./cmd/reliability-agent audit-watch \
  --profile examples/demo-checkout-api.yaml --interval 5s --emit-otlp
```

Wait for `slack socket connected` before rolling.

Then open <http://localhost:8090> and click **Start continuous traffic**. Leave it
running for the whole take.

This matters more than it sounds: **an SLO is a rate.** A window with no requests
is not healthy, it is `indeterminate` with "no data in SLO window". The first
attempt at this setup failed exactly that way, because traffic had stopped nine
minutes earlier.

Let the healthy baseline run 5+ minutes so the opening shot reads `1.000`.

## Shot list

### 1. The claim (~40s)

On camera: the app at localhost:8090, then SigNoz showing its traces.

Say what the two tracks are: telemetry quality is a separate question from
service reliability, and the agent refuses to judge the second when it cannot
trust the first.

Show the baseline:

```sh
go run ./cmd/reliability-agent slo --config examples/demo-checkout-slo.yaml --output json
```

Both SLOs `healthy`, SLI `1.000`.

### 2. One trace, three services (~40s)

In SigNoz, open a trace from a checkout click:

```
checkout-web  checkout            (root)
└─ checkout-api  POST /api/checkout
   └─ checkout-api  checkout.process
      └─ payments  POST /charge
         └─ payments  payments.authorize
```

The browser click starts the trace and it continues through both services. This
is what makes the later "read the actual failing trace tree" claim concrete
rather than illustrative.

### 3. Break it, from the UI (~40s)

Drag **`payments_errors`** to **80%**.

The log panel immediately shows red failures interleaved with green successes.
Point that out: the failures return in ~43ms, the same as the successes, because
this fault fails fast.

**Wait 2-3 minutes.** Re-run the `slo` command:

- `checkout-availability` → **unhealthy**, SLI ~0.52, burn ~48x, budget deeply negative
- `checkout-latency` → still **healthy**

That contrast is worth dwelling on. A single "is it up" SLO would miss the
distinction entirely, and it shows the numbers come from measurement rather than
from a model.

### 4. The alert fires (~20s)

```sh
curl -X POST http://127.0.0.1:8082/webhook \
  -H "X-Sidekick-Webhook-Secret: $SIDEKICK_WEBHOOK_SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{
        "alertname":"SLO fast burn - checkout-availability",
        "service":"checkout-api","environment":"local",
        "severity":"critical","slo":"checkout-availability","tier":"fast"}}]}'
```

Cut to Slack. The message arrives in about six seconds.

### 5. The diagnosis (~90s, the centrepiece)

Walk the message top to bottom:

- the grounding fields - SLO, state, error budget, burn rate, telemetry trusted,
  window - all deterministic;
- **"Possible causes (ranked, not confirmed)"**, naming the payments service
  returning 500s that propagate as Bad Gateway errors. It cites evidence ids;
- the evidence links, each opening the real trace in SigNoz. Click one;
- the recommended action, labelled advisory;
- the three buttons.

Two lines worth saying out loud:

> The model wrote the explanation. It wrote none of the numbers.

> It says *ranked, not confirmed*, because only one golden signal is supported.
> It commits to a single conclusion only when the rules say the evidence
> justifies one.

That second one is the deterministic presentation engine, not modesty: with
`golden_signals_for_conclusion: 2`, an errors-only incident gets hypotheses.

### 6. Approve (~30s)

Click **Approve**. The message rewrites: buttons retire, "Approved by @you"
replaces them, a thread reply confirms nothing was executed, and a **Verify
recovery** button appears.

Say why nothing ran: every remediation is advisory in the MVP, and a human
applies it.

### 7. Verify, honestly (~20s)

Click **Verify recovery** *before* fixing anything. It reports not recovered.

That is the point: the verify stage is deterministic and will not claim success
because someone clicked a button.

### 8. Fix it and close the loop (~40s + a cut)

In the UI, click **Clear all faults**.

**Cut here.** Wait ~10 minutes for the failing period to age out of the 5m
window. Confirm off-camera with the `slo` command before resuming.

Resume, click **Verify recovery** again: recovered, with the SLO state and burn
rate re-read from SigNoz.

### 9. The results, inside SigNoz (~30s)

Open the generated dashboard, `SLOs & Error Budgets [sre-sidekick]`: eight panels
showing the SLI dropping and recovering and the burn rate spiking, plus the six
burn-rate alert rules the agent registered. All created by the sidekick, through
public endpoints, on a stock SigNoz.

### 10. Close (~20s)

The loop that just ran: detect, ground, diagnose, communicate, act, verify. One
pass, one human decision, no autonomous action.

## Optional: make it commit to a conclusion

The presentation rules give ranked hypotheses on one golden signal and a single
conclusion on two. Enabling **`payments_errors` at 80% and `payments_latency` at
50% together** should supply both an error concentration and a latency shift, and
therefore produce a conclusion rather than hypotheses.

**Not yet verified.** Try it in rehearsal before relying on it. If it does commit,
showing both messages side by side is the strongest available evidence that the
presentation logic is rule-based rather than vibes.

## Demonstrating the refusal

Set any payments fault to **100%** rather than 80% and wait ~5 minutes.

The good-path metric stops being emitted entirely, so the completeness gate
cannot tell "everything failed" from "telemetry stopped", and it refuses to
diagnose: `indeterminate`, telemetry trusted `no`, gate reason
`1 of 2 dependencies have data`.

Most AI demos never show the system declining to answer. This one can, on demand,
and it is the differentiator the PRD names.

The `logs_missing_trace_id` slider is the other honesty beat: the service keeps
serving perfectly while the evidence about it degrades, which is exactly what
Track A exists to catch. It also honours its rate, so 50% degrades half the
records rather than all of them.

## If something goes wrong mid-take

| Symptom | Cause | Fix |
|---|---|---|
| Webhook returns 401 | wrong or unset `SIDEKICK_WEBHOOK_SECRET` | re-source the env file; must match what `watch` started with |
| Webhook 200 but no Slack message | same service fired inside the 10-minute dedup window | click **Close session** on the old message, or restart `watch` |
| SLO reads `indeterminate`, "no data in SLO window" | traffic stopped | click **Start continuous traffic** |
| SLO `indeterminate`, telemetry not trusted | a fault is at 100% | drop it to 80% |
| Diagnosis says "too few error samples" | the fault has not produced 3+ errors yet | wait a minute and re-fire |
| Dashboard panels empty | metrics only just emitted | ingestion lags ~1 min; querying immediately returns zero series even though ClickHouse already holds the samples |
| Three `telemetry_quality_*` panels empty | only `slo --emit-otlp` was run | `audit-watch --emit-otlp` feeds those three |
| Buttons do nothing | Interactivity disabled in the Slack app | enable it in app settings, no restart needed |
| Buttons say "this session was lost" | `watch` restarted since that message | use the newest message |
| Evidence links do not open | they point at `--signoz-url`, i.e. localhost | only resolves on the demo machine, fine for recording |

Two rules that save takes:

**One incident per service is one thread.** Re-firing the same service posts a
thread reply, not a new diagnosis. Changing `alertname` does not help - the
fingerprint is service-based. Close the session or restart `watch`.

**"Not recovered" and "not connected" mean opposite things.** The first is
success: the deterministic check ran and honestly reported the service is still
broken. The second means the check could not run at all. Do not record with the
second showing.

## Known rough edges, if a judge asks

Answer plainly; each has a real reason.

- **State is in memory.** Restarting `watch` loses open sessions; the bot says so
  rather than pretending. Persistence is #48.
- **Anyone in the channel can approve.** Approver groups are #47. It is
  advisory-only today, so a wrong click costs a log line.
- **The 5m SLO window is a recording concession**, not a recommendation. A real
  service wants 30d, and the sibling configs say so.
- **A Track A audit reports `indeterminate` overall** even when every rule passes,
  because filling the query row limit sets `Partial`. Reproduced on two
  independent services; recorded in `REBUILD-LOG.md`.
- **The demo app is not production code.** No auth, no persistence; it is a
  telemetry source with a UI, and `demo-app/README.md` says so.
