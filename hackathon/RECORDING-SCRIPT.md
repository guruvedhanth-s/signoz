# Recording script

A shot-by-shot runbook for the recorded demo, with the waits measured on a live
run rather than guessed.

Read `REBUILD-LOG.md` first if the environment is not already up.
Every timing below was observed on 2026-07-26 against a freshly cast SigNoz with
the demo-agent emitting into it.

## The one thing that makes this recordable

The incident is triggered by posting an alertmanager payload straight at the
detect webhook, instead of waiting for SigNoz's alertmanager to fire on its own
schedule.
That is not a shortcut around the product: it is byte-for-byte what a SigNoz
notification channel sends, and everything downstream of it - dedup, grounding,
MCP evidence gathering, the LLM call, Block Kit rendering, session creation - is
the same code the real alert drives.
It just means a take starts when you say so.

## Two setup rules the first rehearsal established

Both of these were found the hard way, and either one silently ruins a take.

### Run the buggy agent at 80% failure, never 100%

```sh
go run ./cmd/demo-agent --buggy --error-rate 0.8
```

`--buggy` alone means a 100% error rate, and in that mode
`agent_grounded_answers_total` never increments at all (see the comment in
`cmd/demo-agent/main.go`).
Once the 5m SLO window contains nothing but failing traffic, the good-path
metric has zero samples, the completeness gate cannot tell "everything failed"
apart from "telemetry stopped", and it refuses to diagnose.

Observed live: the gate reported `1 of 2 dependencies have data` and the alert
came back `indeterminate`, telemetry trusted `no`.
That is the product upholding its own invariant, not a bug - but it gave the
demo a hidden expiry, where firing 1-4 minutes after the bad deploy produced a
diagnosis and firing at 5+ minutes produced nothing.

At 80% the good counter keeps incrementing, the gate stays trusted
indefinitely, and there is no timing cliff at all.
The numbers are also more dramatic: 18.7x burn versus 4.0x.

### One incident per service is one thread

Re-firing the same service does not produce a second message.
It deduplicates into the existing session and posts a `thread_reply` into that
thread instead, which is correct behaviour and exactly what you do not want
between takes.
Changing `alertname` does **not** help; the fingerprint is service-based.

To get a fresh top-level diagnosis, either click **Close session** on the old
message in Slack, or restart `watch` (sessions are in memory, so a restart
clears them).

Restarting `watch` also kills the buttons on any message already in the
channel: clicking them replies "this session was lost". Always click on the
newest message.

## Measured timings

| Step | Wait | What is happening |
|---|---|---|
| Bad deploy to grounded-answers unhealthy | ~1 min | measured at 55 sec, not the 3 min first assumed |
| Bad deploy to latency SLO unhealthy | ~2 min | the 5m window fills with failing spans |
| Webhook fired to Slack message posted | ~7 sec | RCA, MCP evidence, DeepSeek, render |
| Fix applied to SLOs healthy again | ~10 min | the failing period ages out of the 5m window |

The last row is the only one that needs a cut in the edit.

Burn rate varies with how long the failure has been running - 4.0x, 8.6x, and
18.7x across three rehearsal takes.
Fire at a consistent interval after the bad deploy if a specific number matters
on screen.

## Reading the buttons correctly

"Not recovered" and "not connected" look similar at a glance and mean opposite
things.

- **"not recovered"** is success: the deterministic check ran and honestly
  reported that the service is still broken.
- **"The verify engine is not connected yet"** is failure: the check could not
  run at all. Do not record with this showing.

Server-side proof, which Slack cannot fake: `advisory action recorded` in the
`watch` log for an approval, and `verify checked` for a verify.
If a click produces no log line at all, the event never left Slack - Interactivity
is disabled in the app settings.

Use `examples/support-agent-slo-demo.yaml` (5m window), not
`examples/support-agent-slo.yaml` (1h).
With the 1h config the service is still unhealthy an hour after the fix, so the
verify beat never lands.
The demo config's header comment explains the trade and says plainly that 5m is
too twitchy for a real service.

## Setup, before recording

Four terminals. Keep 1 and 2 off camera.

```sh
# 1: healthy baseline
cd sre-sidekick && go run ./cmd/demo-agent

# 2: the sidekick, on the demo SLO config
go run ./cmd/reliability-agent watch \
  --config configs/sidekick.yaml \
  --signoz-url "$SIGNOZ_URL" --mcp-url "$SIGNOZ_MCP_URL" \
  --signoz-internal-url "$SIGNOZ_INTERNAL_URL" \
  --slo-config examples/support-agent-slo-demo.yaml \
  --webhook-listen 127.0.0.1:8082
```

Wait for `slack socket connected` before rolling.

Let the healthy baseline run at least 5 minutes first, so the opening SLO shot
reads `healthy / 1.000` rather than something still warming up.

## Shot list

### 1. The claim (~40s)

On camera: the SigNoz UI with the support-agent's traces and the SLO dashboard.

Say what the two tracks are: telemetry quality is a separate question from
service reliability, and the agent refuses to judge the second when it cannot
trust the first.

Show the baseline:

```sh
go run ./cmd/reliability-agent slo --config examples/support-agent-slo-demo.yaml --output json
```

Both SLOs `healthy`, SLI `1.000`.

### 2. The bad deploy (~30s)

```sh
# terminal 1: Ctrl-C, then
go run ./cmd/demo-agent --buggy
```

Narrate while it warms up: `tool.search_kb` now times out at 5s and the agent
run fails behind it.

**Wait 3 minutes.** Re-run the `slo` command to show `unhealthy`, and point at
the burn rate and the negative error budget. These are computed by the
deterministic engine; no model is involved.

### 3. The alert fires (~20s)

```sh
curl -X POST http://127.0.0.1:8082/webhook \
  -H "X-Sidekick-Webhook-Secret: $SIDEKICK_WEBHOOK_SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{
        "alertname":"SLO fast burn - grounded-answers",
        "service":"support-agent","environment":"local",
        "severity":"critical","slo":"grounded-answers","tier":"fast"}}]}'
```

Cut to Slack. The message arrives in about seven seconds.

### 4. The diagnosis (~90s, the centrepiece)

Walk the message top to bottom:

- the grounding fields - SLO, state, error budget, burn rate, telemetry trusted,
  window - all deterministic;
- the root cause, which cites evidence IDs rather than asserting;
- the evidence links, each opening the actual trace in SigNoz. Click one;
- the recommended action, labelled advisory;
- the three buttons.

The line worth saying out loud: the model wrote the explanation, and it wrote
none of the numbers.

### 5. Approve (~30s)

Click **Approve**.

The message rewrites itself: buttons retire, "Approved by @you" replaces them, a
thread reply confirms nothing was executed, and a **Verify recovery** button
appears.

Say why nothing ran: every remediation is advisory in the MVP, and a human
applies it.

### 6. Verify, honestly (~20s)

Click **Verify recovery** *before* fixing anything.

It reports not recovered. That is the point: the verify stage is deterministic
and will not claim success because someone clicked a button.

### 7. Apply the fix, and close the loop (~40s + a cut)

```sh
# terminal 1: Ctrl-C, then
go run ./cmd/demo-agent
```

**Cut here.** Wait ~10 minutes for the failing period to age out of the 5m
window. Confirm off-camera with the `slo` command before resuming.

Resume and click **Verify recovery** again: recovered, with the SLO state and
burn rate re-read from SigNoz.

### 8. Close (~20s)

The loop that just ran: detect, ground, diagnose, communicate, act, verify.
One pass, one human decision, no autonomous action.

## If something goes wrong mid-take

| Symptom | Cause | Fix |
|---|---|---|
| Webhook returns 401 | wrong or unset `SIDEKICK_WEBHOOK_SECRET` | re-source the env file; the value must match the one `watch` started with |
| Webhook 200 but no Slack message | the same alert fingerprint fired inside the 10-minute dedup window | change `alertname` or wait it out |
| Message says indeterminate, "too few error samples" | the bad deploy has not produced 3+ error samples yet | wait another minute and re-fire |
| Message says indeterminate, "telemetry not trusted" | the SLO completeness gate has no metrics yet | the demo-agent has not been up long enough; give it 2 minutes |
| Buttons do nothing | Interactivity disabled in the Slack app | enable it in app settings, no restart needed |
| Evidence links do not open | they point at `--signoz-url`, i.e. `localhost:8080` | only resolves on the demo machine, which is fine for recording |

## Known rough edges, if a judge asks

Answer these plainly rather than dodging; each has a real reason.

- **State is in memory.** Restarting `watch` loses open sessions; the bot says so
  rather than pretending. Persistence is Phase 3 in the design doc.
- **Anyone in the channel can approve.** Approver groups are designed, not built.
  It is advisory-only today, so the blast radius of a wrong click is a log line.
- **The 5m SLO window is a recording concession**, not a recommendation. The
  sibling config and its comments say so.
- **A Track A audit of `examples/checkout-api.yaml` still fails** its metrics
  query: the v5 API wants a metric aggregation and the source sends a raw
  `count()`. Found during this rebuild, documented in `REBUILD-LOG.md`, not on
  the demo path.

## Showing the results inside SigNoz

The strongest shot for "best use of SigNoz" is the dashboard the sidekick
created, populated by metrics the sidekick emitted.
Validated live; run these once before recording.

```sh
# Dashboard + burn-rate alert rules + notification channel
go run ./cmd/reliability-agent generate \
  --config examples/support-agent-slo-demo.yaml \
  --webhook-url "$SIDEKICK_WEBHOOK_URL"

# SLO metrics: slo_compliance, slo_state, slo_error_budget_remaining,
# slo_burn_rate, slo_mwmb_firing
go run ./cmd/reliability-agent slo \
  --config examples/support-agent-slo-demo.yaml --emit-otlp

# Track A metrics: telemetry_quality_score / _coverage / _findings
go run ./cmd/reliability-agent audit-watch \
  --profile examples/demo-agent.yaml --interval 5s --emit-otlp
```

`generate` creates `SLOs & Error Budgets [sre-sidekick]` with eight panels and
six alert rules (fast/medium/slow burn per SLO), all enabled and evaluating.

The three `telemetry_quality_*` panels are fed only by `audit-watch --emit-otlp`.
Run `slo --emit-otlp` alone and those three panels stay empty on camera.

**Ingestion lags about a minute.** Querying immediately after emitting returns
zero series even though the samples are already in ClickHouse - the series has
to be registered before the query path finds it. Emit, wait, then open the
dashboard. This cost real debugging time; it is not a defect.

For the incident shot, run the emitters while the service is failing so the
panels show the SLI dropping, the burn rate climbing, and
`slo_mwmb_firing` going to 1 - then the alert rules on the same page explain
what would have paged.
