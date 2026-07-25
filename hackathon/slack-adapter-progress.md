# Slack Adapter (Track D) — Progress & Working Context

**Purpose of this file.** This is the handover document for the SRE Sidekick
Slack adapter. It is written so that another agent (or human) can pick the work
up cold: what has been built, why each decision was made, what the invariants
are, what is still missing, and where the landmines are. Read this before
touching `sre-sidekick/internal/notify/slack/`.

Keep it updated as each phase lands.

**Source documents (read these for the "why"):**

- `hackathon/telemetry-health-auditor-prd.md` — the product contract
- `hackathon/slack-notifier-roadmap.md` — the one-way notifier plan (phases 0-8)
- `hackathon/slack-session-design.md` — the interactive/session layer, and the
  20 edge cases E1-E20 referenced throughout the code comments

---

## 1. What this adapter is

The sidekick runs the loop **Detect → Ground → Diagnose → Communicate → Act →
Verify** (PRD section 4). Track D owns **Communicate**: taking a finished
`notify.Diagnosis` and putting it in front of an on-call engineer in Slack,
then holding a conversation about it.

Two shapes are being built, in order:

1. **Outbound notifier (PRD MVP).** Diagnosis → Block Kit message → posted to
   the on-call channel. One-way.
2. **Interactive session layer (session design doc).** The posted message
   becomes the root of a Slack thread; that thread *is* the session. Humans ask
   follow-ups, and approve or decline, inside the thread.

### The three invariants (do not break these)

1. **The adapter never computes reliability facts.** SLO state, burn rate,
   error budget and telemetry trust are computed by the SLO engine and the
   completeness gate. This layer *displays* them. It never recomputes,
   re-rounds into a different meaning, or infers them (PRD section 7).
2. **Indeterminate means silent about cause.** When telemetry is not trusted,
   the message states no root cause, proposes no fix and offers nothing to
   approve. Inventing a cause is the failure mode the PRD exists to prevent.
3. **Approval records intent; it never executes.** The MVP is advisory (PRD
   sections 5.6, 15). A `Decision` is logged with the Slack user id, and a
   human acts by hand. Design the record so a future executing adapter can
   consume it unchanged.

---

## 2. Phase plan and status

| Phase | Scope | Status | Branch |
|---|---|---|---|
| 0 | `Notifier` interface + `Diagnosis` types + fake | done before this work | — |
| 1 | Typed `sidekick.yaml` config loader | **done** | `feat/slack-config` |
| 2 | Block Kit rendering (pure functions) | **done** | `feat/slack-blocks` |
| 3 | Slack client + `notify.Notifier` implementation | **done** | `feat/slack-client` |
| 4 | Session store (`internal/session`) | **done** | `feat/slack-sessions` |
| 5 | Inbound door: Socket Mode receiver | **done** | `feat/slack-inbound` |
| 6 | Handlers: coordinator, decisions, follow-ups | **done** | `feat/slack-handlers` |
| 7 | `watch` subcommand: dial Slack and supervise | **done** | `feat/slack-watch` |
| 8 | Integration tests + `sidekick_incidents` metrics | not started | — |

Branches stack: each phase branches from the previous phase's branch, since
none are merged to `main` yet.

---

## 3. Phase 1 — config loader (done)

**Files:** `sre-sidekick/internal/config/config.go`, `config_test.go`,
`sre-sidekick/configs/sidekick.yaml`

```go
cfg, err := config.Load("configs/sidekick.yaml")
token, err := cfg.Notify.Slack.BotToken()      // reads $SLACK_BOT_TOKEN
secret, err := cfg.Notify.Slack.SigningSecret() // reads $SLACK_SIGNING_SECRET
ttl, err := cfg.Notify.Slack.SessionTTLDuration()
```

Shape (`notify.slack.*` in YAML): `bot_token_env`, `app_token_env`,
`default_channel`, `default_environment`, `session_ttl`,
`max_concurrent_rca`.

`default_environment` (phase 6) is what `/diagnose support-agent` resolves to.
An SLO is always scoped to an environment, so guessing would report facts
about the wrong system.

`app_token_env` was added in phase 5 and **`signing_secret_env` was removed**:
request signatures authenticate an inbound HTTP endpoint, and Socket Mode
means there isn't one. Both tokens have their prefix checked (`xoxb-` for the
bot token, `xapp-` for the app-level token) because swapping them is the most
common setup mistake and Slack's own error for it is unhelpful. The check
never echoes the token value.

### Decisions

- **Secrets are never in the YAML and never on the struct.** The file names
  environment *variables*; values are read from the process environment. A
  `Config` value is therefore safe to log. Errors name the variable, never the
  value.
- **Validation is strict about credentials.** `Load` fails if a named variable
  is empty, so a misconfigured deployment dies at startup rather than at the
  first Slack call during an incident. Consequence: **any test that calls
  `config.Load`/`Parse` must set both env vars** — use the `setCredentials(t)`
  helper pattern with `t.Setenv`.
- **`session_ttl` is a string, not `time.Duration`.** yaml.v3 cannot decode
  `30m` into a `time.Duration` (it expects an integer nanosecond count). It is
  parsed by `SessionTTLDuration()`, mirroring `slo.WindowDuration`.
- **Unknown YAML keys are rejected** (`decoder.KnownFields(true)`), so a typo'd
  key fails loudly instead of being silently ignored.

---

## 4. Phase 2 — Block Kit rendering (done)

**Files:** `sre-sidekick/internal/notify/slack/blocks.go`, `blocks_test.go`

Pure functions, zero I/O — the entire message contract is testable without a
Slack workspace:

```go
func DiagnosisBlocks(d notify.Diagnosis) []slack.Block
func IndeterminateBlocks(r notify.IndeterminateReason) []slack.Block
```

### Message order (PRD section 14)

1. Header — `Diagnosis: <service> (<env>)`, or `Indeterminate: ...`
2. Grounding fields — SLO, state, error budget left, burn rate, telemetry
   trusted, window
3. Root cause — or ranked `Candidates`, or (indeterminate) the reason plus a
   missing-evidence list
4. Evidence — SigNoz deep links
5. Recommended action — explicitly advisory
6. Action buttons
7. Context footer — correlation id and UTC timestamp

### Buttons — wire contract for phase 6

| Constant | `action_id` | Rendered when |
|---|---|---|
| `ActionApprove` | `sidekick_approve` | status `diagnosed` **and** a fix exists |
| `ActionDecline` | `sidekick_decline` | same as approve |
| `ActionClose` | `sidekick_close` | always |

There is deliberately **no Acknowledge button** — Approve/Decline/Close cover
the decisions, and a fourth button was judged noise. Every button carries the
`CorrelationID` in its `value`, so a click stays auditable even if the session
lookup fails (PRD section 20).

`ActionClose` is the answer to edge case **E1**: a `/end` slash command cannot
carry `thread_ts`, so closing a session must be a button (or a threaded
keyword), never a global slash command.

### Formatting rules

- Burn rate: `14.2x`. Error budget: `-3.4%`, sign preserved (negative means the
  budget is spent). `NaN`/`Inf` render as `n/a`.
- Timestamps are UTC, so two engineers in two timezones read the same incident
  clock.
- Missing values get explicit placeholders (`unknown service`, `no correlation
  id recorded`) rather than blanks.

### Security posture (E12, E13)

Root causes, evidence notes and proposed fixes originate from an LLM reading
attacker-influenceable telemetry. They are untrusted:

- `escape()` replaces `&`, `<`, `>` so text cannot forge a Slack link or a
  `<!channel>` broadcast mention.
- `safeLink()` allows only `http(s)://` URLs, and rejects any URL containing
  `<`, `>` or `|` (which would terminate Slack's `<url|label>` syntax early).
  `javascript:`, `slack://` and relative paths are dropped, not rendered.
- Headers use `plain_text`, which Slack never interprets as markup.
- `truncate()` caps every text object at 2900 bytes on a rune boundary, under
  Slack's 3000 limit.

### Other guards

- `MaxEvidenceItems = 5` (**E20**: Slack rejects messages over 50 blocks). The
  omitted count is reported as `Showing 5 of 12 evidence items`. There is
  deliberately **no "view all" link** — no such URL exists in the data, and
  fabricating one would violate invariant 1. *If a stable SigNoz search URL
  shape becomes available, this is the place to add it.*
- An irreversible fix (`Reversible == false`) is labeled and its Approve button
  carries a Slack confirmation dialog (**E6**, PRD section 15).

### Testing note

`blocks_test.go` flattens blocks by walking the structs, **not** by
marshalling to JSON: `encoding/json` escapes `<`, `>` and `&` into
`\u003c`-style sequences and would hide exactly the characters the escaping
tests care about.

---

## 4a. Phase 3 — Slack client and Notifier implementation (done)

**Files:** `sre-sidekick/internal/notify/slack/client.go`, `client_test.go`

```go
client, err := slack.New(cfg.Notify.Slack, slack.WithLogger(logger))
err = client.NotifyDiagnosis(ctx, d)      // notify.Notifier
ref, err := client.PostDiagnosis(ctx, d)  // same, plus the posted message ref
```

`*Client` satisfies `notify.Notifier`, asserted at compile time.

### The transport seam

```go
type poster interface {
    PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error)
}
```

`*slack.Client` satisfies this, and so does a test fake. Injected with
`WithPoster`. This is the seam that keeps the whole adapter testable without a
workspace, and it is also how the session layer will get the returned message
timestamp.

### `PostRef` — why it exists before it is used

`PostDiagnosis`/`PostIndeterminate` return `PostRef{Channel, Timestamp}`.
The `Timestamp` is the Slack message `ts` of the root message, which becomes
the `thread_ts` session key in phase 4. Nothing consumes it yet; it is returned
now so the session layer does not have to reshape this API later.

### Retry policy

`DefaultRetryPolicy()`: 4 attempts, backoff 250ms doubling to a 2s cap, ±20%
jitter, 10s total wall-clock budget, `ctx` honoured throughout.

- **Why retry:** Slack rate limits and 5xx blips are routine, and a diagnosis
  that silently never arrives is the worst failure this adapter has.
- **Why bound it:** an unbounded retry is itself an outage. When Slack's
  `Retry-After` exceeds the remaining budget (it sometimes says 60s) the client
  gives up and reports, rather than stalling the diagnose loop.
- **Why jitter:** during an alert storm, retries that fire in lockstep re-spike
  the API they are waiting on.
- **Retryable:** `RateLimitedError` (honouring its exact `Retry-After`), HTTP
  429/5xx, and unclassified errors (assumed network).
- **Not retryable:** context cancellation, and the permanent Slack errors
  listed in `permanentSlackErrors` (`invalid_auth`, `channel_not_found`,
  `not_in_channel`, `msg_too_long`, `invalid_blocks`, ...). Retrying those only
  wastes the budget and delays the failure report.

### Failure semantics (decided deliberately)

A failed post is **logged at `Error` with the correlation id, then returned**.

- Returning it keeps the `Notifier` interface honest: an undelivered diagnosis
  is a real event. Swallowing it would mean nobody is told, and nobody knows
  nobody was told.
- The log record exists so the failure is in the audit trail regardless of what
  the caller does with the error (PRD section 20).
- **The rule that a Slack outage must not take down the engine (E17, PRD
  section 25) is enforced at the call site**, in phase 7: the loop treats a
  notify error as non-fatal, logs it, marks the incident undelivered and
  continues. Do not "fix" this by making the client return `nil`.
- A panic in rendering or transport is recovered, logged with its stack, and
  converted to an error - a malformed diagnosis must not crash the process
  that is trying to report an incident.

### Fallback text

Every message carries plain-text fallback (`diagnosisFallback` /
`indeterminateFallback`). Block Kit messages without it arrive blank in push
notifications and for screen readers. The fallback carries deterministic facts
only - service, SLO, state, burn rate, error budget.

### Logging

`log/slog`, matching the rest of the sidekick, injectable via `WithLogger`
(defaults to `slog.Default()`). One record per outcome: `slack message posted`
(info), `slack post failed, retrying` (warn), `slack message not delivered`
(error), `slack adapter panicked` (error). All carry `correlation_id`.

### Testing approach - read this before adding tests

Two different fakes, on purpose:

1. **`fakePoster`** - implements `poster`, scripts a sequence of results. Used
   for retry, backoff, cancellation and failure-logging tests. Combined with an
   injected clock (`withClock`, `withoutJitter`), so "waited 2s" is asserted in
   microseconds and the suite has no real sleeps.
2. **`wireRecorder`** - an `httptest` server plus a *real* `*slack.Client`
   pointed at it. Used for payload assertions. This is necessary because
   `MsgOption`s only become a payload inside the Slack library's request
   builder: `slack.UnsafeApplyMsgOptions` does **not** expose blocks, since
   `MsgOptionBlocks` writes to an unexported `sendConfig.blocks` field that is
   only serialised in `formSender.BuildRequestContext`. Asserting on the real
   form body avoids testing a reimplementation of the library.

Note: `go test -race` does not run on a Windows box without a C toolchain
(`cgo.exe: exit status 2`). Run the race detector in CI or on Linux.

---

## 4b. Phase 4 — session store (done)

**Files:** `sre-sidekick/internal/session/session.go`, `manager.go`,
`manager_test.go`

A pure in-memory state machine. **This package must never import the Slack
package** — phase 6's Slack handlers import *it*, so the dependency runs one
way only. It also never posts, logs or reasons: anything that needs to *say*
something returns to the caller instead.

### Types

```go
type Status string   // open | resolved | expired
type Session struct { ... }   // always passed by pointer; contains mutexes
type View struct { ... }      // immutable snapshot, safe to copy and log
type Turn struct { Actor Actor; Text string; At time.Time }
type Decision struct { Kind DecisionKind; UserID, Note string; At time.Time }
```

`Fingerprint(service, environment, slo)` is the dedup key, lowercased and
trimmed. **The window is deliberately excluded**: a window that shifts between
evaluations would make every re-fire look like a new incident and silently
defeat deduplication — the exact failure the key exists to prevent.

### Manager API

```go
m := session.NewManager(session.WithTTL(ttl), session.WithClock(now))

s, existing, err := m.Open(session.OpenRequest{ChannelID, ThreadTS, Diagnosis})
s, ok := m.ByThread(channelID, threadTS)     // routes every inbound event
s, ok := m.ByFingerprint(fp)                 // live sessions only
err := m.AppendTurn(s, turn)
err := m.AddEvidence(s, ev...)               // budgeted (E9)
ok, existing, err := m.Decide(s, decision)   // single-writer
err := m.Close(s, reason)
m.Touch(s)
expired := m.ReapIdle()                      // pure function; ticker lives in phase 7
views := m.Snapshot()
```

### Behaviour decisions (settled, do not silently reopen)

- **`Open` is passive on a re-fire.** When the fingerprint already has a live
  session it returns it with `existing == true` and touches nothing: no post,
  no turn appended, frozen diagnosis untouched. Whether a re-fire deserves a
  thread update depends on how long ago the last one was, which is handler
  policy, not store policy. `History` is LLM context; filling it with "alert
  re-fired" system turns burns context window for no reasoning value. The
  dedup still does its real job: the caller skips a second paid RCA run and a
  duplicate thread (**E2**).
- **A re-fire after resolution opens a *new* session.** Resolved and expired
  sessions leave `byFingerprint`, so the same alert firing later is a new
  incident with its own thread. A closed thread is a closed record.
- **No `awaiting_decision` status.** Every open session with a fix implicitly
  awaits one; a separate state would be a second source of truth.
- **Single-writer decisions (E5).** The first terminal decision wins.
  Later ones return `accepted == false` plus the decision already on record,
  so the handler can reply "already resolved by @X at HH:MM". Verified by a
  32-goroutine race test asserting exactly one acceptance.
- **Closed sessions stay addressable** in `byThread` for `ClosedRetention`
  (default 24h), so a late reply gets "this session is closed" rather than
  "unknown thread" (**E7**). After that `ReapIdle` forgets them, which is the
  memory backstop — there is no session-count cap by design.
- **`ReapIdle` returns the sessions it expired** instead of announcing them.
  Keeping the package Slack-free is what makes it testable and reusable for
  another chat adapter (**E4**).
- **Participants are a set of Slack user ids.** Nothing reads it yet. It
  exists because "who was in the room" cannot be backfilled, and a stricter
  approver policy (**E18**) or an `@`-mention on auto-close would need it.

### Locking rules — read before editing

There are two locks per session, for two different jobs:

- **`Session.mu`** guards the struct's fields. Held only for short,
  non-blocking updates, **never across a network or LLM call**.
- **The turn lock** (`BeginTurn`/`EndTurn`) serialises a *whole* human turn
  including the slow RCA call it triggers, so two people typing at once in one
  thread are handled one after another and the history cannot be corrupted by
  two half-finished updates (**E5**, **E15**). Handlers must take it for the
  duration of the turn.

Lock ordering is always **`Manager.mu` → `Session.mu`**, never the reverse.
`Decide` and `Close` therefore release the session lock before touching the
manager's indexes.

### Not in this phase

No HTTP, no Slack calls, no LLM, and no reaper *goroutine* — `ReapIdle` is a
pure function; the ticker that calls it belongs to the server lifecycle in
phase 7. The concurrency cap (`max_concurrent_rca`, **E8**) is a semaphore
around the RCA run in phase 6, not a session-count limit here: the cost being
capped is the paid analysis, not the map entry.

---

## 4c. Phase 5 — the inbound door, Socket Mode (done)

**Files:** `sre-sidekick/internal/notify/slack/socket.go`, `events.go`,
`socket_test.go`

### Why Socket Mode, and what it removes

The adapter receives events over **Slack Socket Mode**: the sidekick dials
*out* to Slack over a WebSocket and Slack pushes envelopes down it.

This was a deliberate choice over HTTP request URLs:

- **No public endpoint.** Nothing to expose, no TLS to terminate, no ngrok or
  tunnel for a laptop demo, and nothing inbound to attack.
- **No signature verification, and none needed.** Signature checks exist to
  authenticate an inbound endpoint; the socket is authenticated once, when it
  is dialled, by the app-level token. That is why `verify.go`, the replay
  window, body caps and `url_verification` do not exist in this codebase.
- **Trade-off:** Socket Mode cannot be used by an app distributed in Slack's
  public directory. If this ever ships publicly, HTTP routes plus signature
  verification come back — and the `Handler` seam below means only the
  transport file changes.

### What Socket Mode does *not* change

- **Ack within three seconds.** Slack redelivers anything unacked, so every
  envelope is acked immediately and the work is dispatched asynchronously
  (**E11**).
- **Deduplication.** Because Slack redelivers, and a reconnect can replay,
  deliveries are deduped by `event_id`, falling back to `envelope_id`. Without
  it the same human turn is processed twice.
- **Bounded work.** A burst must not spawn unbounded goroutines or unbounded
  paid analysis (**E8**).

### The seam

```go
type Handler interface {
    OnMessage(ctx context.Context, msg Message)
    OnInteraction(ctx context.Context, interaction Interaction)
    OnCommand(ctx context.Context, cmd Command)
}

receiver, err := slack.NewReceiver(events, acker, handler, opts...)
err = receiver.Run(ctx)   // blocks until ctx is cancelled or the stream closes
```

The event stream and the `acker` are **injected**, not constructed inside the
receiver. `*socketmode.Client` satisfies both in production; a plain channel
and a recorder satisfy them in tests. That is why the entire inbound path is
tested with no WebSocket and no network.

`Message`, `Interaction` and `Command` (`events.go`) are the adapter's own
narrow structs, so phase 6's logic is not coupled to slack-go's JSON shapes
and can be tested by constructing a three-field value.

### Behaviour worth knowing before editing

- **Ack happens before dispatch, always** — including for envelopes the
  receiver decides to ignore. An unacked envelope is simply redelivered, so
  "ignore" must still mean "ack".
- **Bot messages are dropped at the transport.** The sidekick's own posts come
  back as message events; acting on them would make it answer itself forever.
  Detected via `bot_id` or the `bot_message` subtype.
- **Work runs on a detached context.** The envelope's context dies at ack, so
  work hangs off `context.WithoutCancel(runCtx)` with its own 5 minute
  timeout (`DefaultWorkTimeout`). Passing the envelope context through would
  cancel every LLM call the instant it was acked — this is the single easiest
  mistake to reintroduce here.
- **Worker pool: 8 workers, queue 64** (`DefaultWorkers`,
  `DefaultQueueSize`). A full queue **sheds load and logs at error level**
  rather than growing without bound: a visible drop beats an invisible
  backlog.
- **Shutdown drains.** Accepted work has already been acked to Slack, so
  abandoning it would silently lose a human's message. `Run` closes the job
  channel and waits for workers before returning.
- **A handler panic is contained** and logged with its stack; the receiver
  keeps serving.
- **Connection lifecycle events** (connecting, connected, disconnect,
  invalid auth, errors) are logged, never dispatched, never acked.
  Reconnection itself is `socketmode.Client`'s job.
- **Dedup cache** is TTL-bounded (10 minutes) *and* capacity-bounded (4096),
  evicting expired entries first and then the oldest. An empty id is treated
  as new, because collapsing every unidentified delivery into one entry would
  silently drop unrelated events.

### Not in this phase

No session lookup, no RCA, no replies. Phase 6 implements `Handler`; phase 5
ships only a recording test double.

---

## 4d. Phase 6 — the coordinator (done)

**Files:** `sre-sidekick/internal/notify/slack/coordinator.go`, `rca.go`,
`coordinator_test.go`, plus additions to `client.go` and `blocks.go`

`Coordinator` implements the phase 5 `Handler`, so the receiver hands it work
directly. It is where sessions, the poster and the analysis engine meet.

```go
coordinator, err := slack.NewCoordinator(client, sessions, rca,
    slack.WithDefaultEnvironment(cfg.DefaultEnvironment),
    slack.WithMaxConcurrentAnalysis(cfg.MaxConcurrentRCA),
)

// Alert-driven entry point: post, then open the session on the new thread.
s, err := coordinator.Announce(ctx, diagnosis)

// Handler (called by the receiver)
coordinator.OnMessage(ctx, msg)
coordinator.OnInteraction(ctx, interaction)
coordinator.OnCommand(ctx, cmd)

// Called by the phase 7 ticker
coordinator.ReapIdle(ctx)
```

### The RCA seam — the integration point for the analysis branch

`rca.go` declares the interface **on the consumer side**:

```go
type RCA interface {
    Diagnose(ctx context.Context, req DiagnoseRequest) (notify.Diagnosis, error)
    AnswerFollowup(ctx context.Context, req FollowupRequest) (string, error)
}
```

`FollowupRequest` is a **flat value**, not a `*session.Session`, specifically so
the analysis engine never has to import the session package. The coupling runs
one way: the Slack adapter depends on a small interface, and the engine depends
on nothing of ours. Attaching a real engine is a ~10-line adapter struct.

Shipped with `UnavailableRCA`, which **refuses rather than improvises**. A
fabricated root cause would be indistinguishable from a real one in the thread,
so the stub returns `ErrRCAUnavailable` and the adapter says plainly that the
engine is not connected — while noting the grounded facts still stand, because
they came from the SLO engine, not a model.

### Flows

**Alert → thread.** `Announce` checks the fingerprint first. A live session for
the same incident means a short "fired again" note in the *existing* thread and
no second analysis (**E2**); otherwise it posts the diagnosis and opens the
session keyed on the message it just created.

**Button → decision.** `ByThread` → `Decide`/`Close` → rewrite the original
message so the buttons are replaced by the outcome → reply in the thread. A
second click gets "Already approved by @U1 at 14:32" (**E5**). A click with no
session (restart) is answered, because the button proves the thread was ours.

**Threaded message → answer.** Only threaded replies are considered; top-level
channel messages belong to no incident. Then: turn lock → build the follow-up
request → append the human turn → analysis → append the answer → reply.

**`/diagnose <service> [env]`.** Acknowledge in channel, run the analysis under
the concurrency cap, then `Announce`.

### Decisions worth not re-litigating

- **Buttons decide; text never does.** Typing "approve" triggers a nudge to the
  button and records nothing. `isTerminalWord` matches only short, unqualified
  phrases — "approve if you think the timeout theory holds" is a question, and
  treating it as consent is the precise misread this design exists to avoid.
  There is no intent classifier and no LLM in this path.
- **The buttons are retired after a decision** via `chat.update` and
  `ResolvedBlocks`. A live-looking Approve button on a decided incident is
  actively misleading mid-incident. Update failure is logged, never
  propagated: the decision is already in the session and the audit log.
- **History excludes the current question.** `FollowupRequest.History` is the
  conversation *so far*; the new turn travels in `Question`. Carrying it in
  both would waste context and read as if it were asked twice.
- **Unknown thread → one `conversations.replies` lookup.** The bot answers
  only if it posted the thread root; otherwise it stays silent, and it stays
  silent on lookup error too. Speaking wrongly in someone else's thread is
  worse than saying nothing (**E3**).
- **Closed sessions are answered once** per thread, tracked in the
  coordinator's `notified` set (**E7**). Expired sessions say "expired", not
  just "closed".
- **Closing is not silencing.** Both the close reply and the expiry notice say
  explicitly that ending the conversation does not stop a still-firing alert
  (**E16**).
- **The concurrency cap gates analysis only** (**E8**). Button clicks and
  lookups are cheap and stay responsive when the system is saturated — a
  decision must be possible even when busy. Saturation is stated plainly
  ("too many analyses are running"), never silently queued.

### Client additions

`PostThreadReply`, `UpdateMessage`, `ThreadRootPostedByUs`, `Channel()`. The
`poster` interface widened to three methods (`PostMessageContext`,
`UpdateMessageContext`, `GetConversationRepliesContext`), all still satisfied
by `*slack.Client` and by the test fake.

### Testing note

`slack.UnsafeApplyMsgOptions` exposes `text` and `thread_ts` but **not**
`blocks` (see the phase 3 note). So coordinator tests assert on *which* message
was updated, and `TestResolvedBlocks` asserts on *what* the rewrite contains.
Log buffers in tests are wrapped in `syncBuffer`, because handler goroutines
write to them while the test reads.

---

## 4e. Phase 7 — the `watch` subcommand (done)

**Files:** `sre-sidekick/cmd/reliability-agent/watch.go`, `watch_test.go`, plus
one case in `main.go`

```bash
export SLACK_BOT_TOKEN=xoxb-...
export SLACK_APP_TOKEN=xapp-...
go run ./cmd/reliability-agent watch --config configs/sidekick.yaml
```

`--config` defaults to `configs/sidekick.yaml`, resolved relative to the
working directory.

### What it wires

```
config.Load  →  slackapi.New(bot, OptionAppLevelToken(app))
             →  socketmode.New(api)
             →  slack.New(cfg, WithPoster(api))
             →  session.NewManager(WithTTL(cfg.SessionTTL))
             →  slack.NewCoordinator(client, sessions, UnavailableRCA{})
             →  slack.NewReceiver(socket.Events, socket, coordinator)
             →  supervise(ctx, socket, receiver, coordinator)
```

`supervise` runs three things and stops all of them when any one finishes:
the socket (`RunContext`), the receiver (`Run`), and a one-minute ticker
calling `ReapIdle`. Shutdown order matters: the socket stops first so no new
envelopes arrive, then the receiver drains work it has already acknowledged to
Slack. Cancellation is a clean exit; a component failing is reported.

The sweep interval is one minute against a 30 minute TTL, because the sweep is
just a map scan over live sessions — running it often only bounds how late an
expiry notice arrives.

Each runner is behind a tiny interface (`socketRunner`, `receiverRunner`,
`reaper`), so the supervisor's lifecycle is tested without dialling Slack.

### Deliberately not included

`watch` **does not serve HTTP**. The alert webhook that starts an alert-driven
diagnosis belongs to the detection track; when it exists it calls
`Coordinator.Announce` and nothing here changes. Building an HTTP server for
someone else's route would mean deciding its auth and port before the route
exists.

`UnavailableRCA{}` is attached until the analysis branch lands — one line to
swap. The binary is honest at runtime: `/diagnose` replies that the engine is
not connected.

### Startup behaviour worth relying on

- Missing or **swapped** tokens fail at startup with the variable named and
  the expected prefix stated, never the value:
  `environment variable SLACK_BOT_TOKEN does not hold a Slack bot token:
  expected a value starting with "xoxb-"`.
- The startup log records channel, default environment, TTL, concurrency cap
  and the credential **variable names**.
- `SIGINT`/`SIGTERM` cancel the context via `signal.NotifyContext`.

### One correctness fix to phase 5

`*socketmode.Client.Ack` returns an `error`, which the phase 5 `acker`
interface did not declare — so the real client never actually satisfied it.
That only surfaced when this phase wired the two together. The interface now
matches, and a failed ack is logged rather than propagated: Slack simply
redelivers, and the dedup cache absorbs it.

---

## 5. Design decisions that shape everything after this

These were settled in discussion and should not be silently reopened.

### 5.1 One incident = one Slack thread = one session

`thread_ts` is the session key. Slack tells us which thread a reply belongs to,
so incidents never need to be disambiguated by guessing. Two incidents are two
threads and therefore two independent sessions. A user is never a session; an
incident is. One user can be in five sessions, five users can be in one.

### 5.2 Buttons carry decisions; free text is only questions

Approve/Decline/Close are **buttons only** (option A of the discussion). A
button click arrives with a verified identity, `thread_ts` and `action_id` — no
parsing, no ambiguity, no LLM.

Consequence: **there is no intent classifier.** Every free-text threaded reply
is treated as a question and routed to the follow-up path. The only concession
is a small nudge: if a reply is an obvious terminal word ("approve", "done"),
the bot replies "tap the **Approve** button above" instead of acting on it.

This deliberately removes the LLM-classification layer and with it the risk of
a misread "sure, but…" being recorded as an approval. If a future channel
without buttons appears (voice, SMS), a classifier can be reintroduced behind
an interface — but not before.

### 5.3 Advisory only

Approve records who decided, what, and when. Nothing executes. Verify does not
belong to Track D.

---

## 6. Landmines for whoever works on this next

- **Slack redelivers anything not acked within 3 seconds** (**E11**). The
  receiver acks first and works asynchronously, and dedupes on `event_id`.
  Phase 6 handlers must not reintroduce slow work before the ack.
- **Never pass the envelope's context into async work.** It is cancelled at
  ack. Use the detached, timeout-bounded context the receiver supplies.
- **Socket Mode means no signature verification exists.** That is correct, not
  an oversight: there is no inbound endpoint to authenticate. If HTTP routes
  are ever added back, signature verification becomes mandatory again.
- **The same alert fires repeatedly** (**E2**). Dedupe by fingerprint
  (`service + env + slo + window`) and update the existing thread rather than
  opening a second session and paying for a second RCA run.
- **A Slack API failure must never take down the deterministic engine**
  (**E17**, PRD section 25). Retry with backoff, log, carry on.
- **Sessions are in-memory for now** (**E3**). A restart loses them; a reply to
  an old thread must fail gracefully with "this session was lost, run
  `/diagnose` to reopen", not a panic or silence. SQLite persistence is a
  later phase. Note the distinct case: `ByThread` returning a session whose
  status is terminal means "closed", while `ByThread` returning nothing means
  "forgotten or never existed" — those deserve different replies.
- **Do not put grounding numbers through the LLM.** They are pinned into the
  prompt as frozen facts and echoed verbatim; the model writes phrasing only.

---

## 7. Commands

```bash
cd sre-sidekick
go run ./cmd/reliability-agent watch      # run the Slack adapter
go build ./...
go test ./... -count=1
go test ./internal/notify/slack/ -count=1 -v
gofmt -l internal && go vet ./...
```

Slack work needs these in the environment (values from the Slack app config):

```bash
export SLACK_BOT_TOKEN=xoxb-...   # bot token, for posting
export SLACK_APP_TOKEN=xapp-...   # app-level token, for the Socket Mode dial
```

Slack app setup for Socket Mode: enable Socket Mode, create an app-level token
with `connections:write`, subscribe to `message.channels` (and
`message.groups` for private channels), enable interactivity, and register the
`/diagnose` slash command. No Request URL is needed for any of them.

---

## 8. Changelog

| Date | Phase | Commit | Summary |
|---|---|---|---|
| — | 1 | `c084916` | Typed `sidekick.yaml` loader; env-var-named secrets, strict presence validation, unknown keys rejected |
| — | 2 | `4909ef8` | Block Kit rendering for diagnosis and indeterminate messages; approve/decline/close buttons; escaping, link allowlist, evidence cap |
| — | 2 | `53bfbb0` | This progress and handover document |
| — | 4 | `2d01cc7` | Session store: thread-keyed sessions, fingerprint dedup, single-writer decisions, budgeted follow-up evidence, TTL reaper |
| — | 7 | pending | `watch` subcommand: dials Socket Mode, supervises the receiver and the idle sweep, drains on shutdown; fixes the `acker` signature so the real client satisfies it |
| — | 6 | `ad414b6` | Coordinator: alert-to-thread announcements with dedup, button decisions with retired buttons, threaded follow-ups behind the RCA seam, `/diagnose`, idle expiry notices, analysis concurrency cap |
| — | 5 | `9435ddc` | Socket Mode receiver: ack-then-dispatch, dedup, bounded worker pool, draining shutdown; config gains `app_token_env` and drops `signing_secret_env` |
| — | 3 | `a7daef5` | Slack client implementing `notify.Notifier`; bounded retry with jitter and rate-limit awareness, fallback text, correlation-id audit logging, panic containment |
