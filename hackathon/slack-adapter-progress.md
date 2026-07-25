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
| 3 | Slack client + `notify.Notifier` implementation | not started | `feat/slack-client` |
| 4 | Session store (`internal/session`) | not started | — |
| 5 | Inbound HTTP (signature verify, 3s ack, dedup) | not started | — |
| 6 | Handlers (interactivity, events, `/diagnose`) | not started | — |
| 7 | Wire into the API server + `watch` subcommand | not started | — |
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

Shape (`notify.slack.*` in YAML): `bot_token_env`, `signing_secret_env`,
`default_channel`, `session_ttl`, `max_concurrent_rca`.

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

- **Slack retries anything slower than 3 seconds** (**E11**). Every inbound
  handler must ack `200` immediately and do LLM/MCP work asynchronously, and
  must dedupe on the Slack `event_id` / retry header, or the same human turn
  gets processed twice.
- **Signature verification is non-negotiable** on all three inbound routes.
  Without it anyone on the internet can puppet the bot.
- **The same alert fires repeatedly** (**E2**). Dedupe by fingerprint
  (`service + env + slo + window`) and update the existing thread rather than
  opening a second session and paying for a second RCA run.
- **A Slack API failure must never take down the deterministic engine**
  (**E17**, PRD section 25). Retry with backoff, log, carry on.
- **Sessions are in-memory for now** (**E3**). A restart loses them; a reply to
  an old thread must fail gracefully with "this session was lost, run
  `/diagnose` to reopen", not a panic or silence. SQLite persistence is a
  later phase.
- **Do not put grounding numbers through the LLM.** They are pinned into the
  prompt as frozen facts and echoed verbatim; the model writes phrasing only.

---

## 7. Commands

```bash
cd sre-sidekick
go build ./...
go test ./... -count=1
go test ./internal/notify/slack/ -count=1 -v
gofmt -l internal && go vet ./...
```

Slack work needs these in the environment (values from the Slack app config):

```bash
export SLACK_BOT_TOKEN=xoxb-...
export SLACK_SIGNING_SECRET=...
```

---

## 8. Changelog

| Date | Phase | Commit | Summary |
|---|---|---|---|
| — | 1 | `c084916` | Typed `sidekick.yaml` loader; env-var-named secrets, strict presence validation, unknown keys rejected |
| — | 2 | `4909ef8` | Block Kit rendering for diagnosis and indeterminate messages; approve/decline/close buttons; escaping, link allowlist, evidence cap |
