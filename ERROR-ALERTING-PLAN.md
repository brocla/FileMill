# FileMill Error Alerting — Implementation Plan (issue #7)

**Status:** Phase 1 (`internal/alert` core and the store `Ledger`) and phase 2
(job taxonomy split) implemented 2026-09-11; phases 3–5 not started. First drafted 2026-07-19; **revised
2026-09-11** against the current code. Since the first draft, FileMill gained the
supervisor loop (#4), boot start, the two retention sweeps, sheets-link delivery,
and non-blocking delivery. Each adds alert sites, and the supervisor changes how a
crash can be reported at all.

Goal: when FileMill fails *systemically*, email the operator (the address in
`config/email.yaml`, which is gitignored; examples here use `support@example.com`),
so an unattended worker doesn't fail silently.

Sending is the easy part: `Service.send` in `internal/mailgun/outbound.go`
already works. The real work is deciding **which** failures alert, **throttling**
them, and being honest about what email **cannot** report.

---

## 1. Principles

1. **Alert on systemic failures, stay silent on expected ones.** A PDF that a
   transformer cleanly rejected is a normal outcome the sender already hears about.
   A transformer that crashed, timed out, or broke the contract is systemic.
2. **Throttle everything, and keep the throttle state across restarts.** The
   delivery loop ticks every second, and the supervisor restarts a crashing worker
   every ≤120s. An in-memory throttle resets on every restart, so a crash-loop
   would still send an alert per restart. Throttle state lives in SQLite.
3. **Alerting never blocks, recurses, or crashes.** `Report` enqueues and returns.
   A failed alert send is logged and dropped, never re-reported.
4. **A process can't report its own death.** A crash is reported by the *next*
   process, the one the supervisor restarts. "Never came back" or "machine offline"
   is the heartbeat's job (#5), not this issue's.

---

## 2. What alerts

| Failure | Code site | Alert? | Category | Notes |
|---|---|---|---|---|
| Forged/stale webhook (401), malformed/oversize body (400) | `webhook.go` `receive` | No | — | Bot/client noise |
| Unrouted recipient, disallowed sender, no attachments, rejected file type | `receive` | No | — | Benign or sender's fault |
| **Intake failure (500)** — storage/filesystem/Submit | `receive` → `intake` | **Yes** | `intake` | Mailgun retries ~8h; the throttle absorbs the retry burst |
| `store(notify=)` route misconfiguration warning | `receive` | **Yes** | `route-config` | Today it's a log WARNING only; every real submission is being lost |
| **Transformer missing from config** | `app.go` `execute` | **Yes** | `job-systemic` | Misconfiguration |
| **Transformer timed out** | `execute` | **Yes** | `job-systemic` | |
| **Missing/corrupt/invalid `result.json`** | `execute` → `readResult` | **Yes** | `job-systemic` | Contract violation |
| **Nonzero exit without a valid `success:false` result** | `execute` | **Yes** | `job-systemic` | Crash |
| Valid `result.json` with `success:false` | `execute` | No | — | Sender told in the reply |
| **Panic while running a job** | `execute` (new `recover`) | **Yes** | `panic` | Job marked failed; worker continues |
| **Reply send failing** (non-2xx, timeout) for ≥5 min | `outbound.go` `deliver` | **Yes** | `delivery` | Time-based, not per tick, to ride out a blip (§3.5) |
| **`MarkEmailDelivered` failing after a successful send** | `deliver` | **Yes, no grace period** | `delivery-mark` | The database is failing. Only the mark is retried, not the send (§3.5), but every restart until it recovers sends one duplicate reply |
| **Sheets-link publish failing** (token expired, quota, Drive outage) | `deliver` → `publish` | **Yes** | `publish` | Can persist for hours |
| **Drive file orphaned** (`PutDelivery` failed after upload) | `publish` | **Yes** | `publish-orphan` | Names the file ID for manual cleanup |
| **Job claim error** (`store.Next`) persisting ≥1 min | `app.go` `Run` | **Yes** | `worker-claim` | Today it retries silently forever; a stuck DB halts all work |
| Retention sweep: Drive delete failures | `mailgun/retention.go` | **Yes** | `sweep-drive` | World-editable files outliving the 30-day promise |
| Retention sweep: workspace delete failures | `app/retention.go` | **Yes** | `sweep-jobs` | Low urgency; throttled daily anyway |
| **Worker restarted after a crash** | startup (via supervisor, §3.6) | **Yes** | `restart` | Includes exit code and rapid-restart count |
| **Jobs interrupted** (running at the last shutdown) | `store.Open` marks them `interrupted` | **Yes, if N > 0** | `restart` | Second crash signal that needs no supervisor |
| Startup `fatal()` (bad config, DB, incomplete env, bind failure) | `main.go` | **No** | — | No reporter exists yet; covered by heartbeat #5 |
| Mailgun send itself failing | `send` | **Can't** | — | The alert channel is the broken thing; heartbeat #5 |

**The job-failure split** (`execute`), decided by whether the transformer honored
the contract:

- `result.json` valid and `success:false` → handled. No alert, whatever the exit code.
- `result.json` valid, `success:true`, but nonzero exit → systemic (the result contradicts the exit code).
  The job's message says so rather than repeating the transformer's success text,
  which would tell the sender their report is ready in a reply with nothing attached.
- Anything else that fails (timeout, missing/invalid result, missing transformer,
  a command that won't start) → systemic.
- Killed because the worker itself is shutting down (Ctrl+C, a failed webhook
  listener: the job's context is cancelled) → not the transformer's failure, so
  no alert. Otherwise every restart that lands mid-job would read as a crash.
  The job is still marked failed, as before.

---

## 3. Design

### 3.1 New package `internal/alert`

Neither `app` nor `mailgun` can own this: `mailgun` imports `app`, and both need to
report. A leaf package both import:

```go
type Alert struct {
    Category string // throttle key, e.g. "job-systemic"
    Summary  string // one line; becomes the subject
    Detail   string // job id, operation, input name, error, truncated output
}

// Reporter records a systemic failure. Report must not block or panic.
type Reporter interface{ Report(Alert) }

type Nop struct{}          // the default: alerting disabled

// Mailer sends one plain-text message. *mailgun.Service satisfies it.
type Mailer interface {
    SendAlert(ctx context.Context, to, subject, text string) error
}

// Ledger is the persisted throttle state. *store.Store satisfies it.
type Ledger interface {
    LastAlert(category string) (sentAt time.Time, suppressed int, err error) // zero sentAt: never sent
    RecordAlertSent(category string, at time.Time) error // resets suppressed
    RecordAlertSuppressed(category string) error         // suppressed++
    AlertSendsSince(t time.Time) ([]time.Time, error)    // sends after t, oldest first
}

// Emailer is the real Reporter: a throttle in front of a Mailer, draining a
// buffered channel on its own goroutine so Report never blocks a hot path.
func NewEmailer(m Mailer, l Ledger, to string, cfg Config, now func() time.Time, log *log.Logger) *Emailer
func (e *Emailer) Run(ctx context.Context) // started by main; drains the queue
```

- `Report` does a non-blocking send on a buffered channel (e.g. 64). If the buffer
  is full, it logs and drops the alert. A flood that fills it would be throttled
  anyway.
- The throttle is **per-category cooldown** (default 15 min), plus two **global
  caps**: 10 emails/hour and **20 emails/day** (a rolling 24h window). A
  suppressed alert increments the count, and the next email for that category
  says "N more since the last alert".
- **The daily cap is the binding one.** The Mailgun Free plan allows 100 sends a
  day, *shared with replies*, and hard-rejects past that until the next day. The
  hourly cap alone would allow 240/day, enough to lock out every reply to senders.
  20/day keeps alerts to at most a fifth of the day's budget. When the daily cap
  is hit, one last alert says so ("daily alert cap reached; further alerts are
  logged only until <time>"); it counts within the 20, so the cap stays exact.
- **A failing Ledger doesn't silence alerts.** A sick database is the likeliest
  cause of `worker-claim` and `intake`, so dropping alerts when the Ledger fails
  would silence exactly those. The `Emailer` keeps an in-memory copy of the
  throttle state, synced from the Ledger on every good read. After the first
  Ledger error, it throttles from that copy for the rest of the process, and
  each email says so. It never switches back: if writes failed but reads still
  worked, the Ledger would read back too few sends. The risk that remains is a
  worker that crash-loops while its database opens but can't hold the ledger,
  since the in-memory copy resets on every restart. A database that can't be
  written usually fails `store.Open` instead, which is a startup `fatal` with no
  reporter at all.
- Clock, Mailer and Ledger are all injected, so the throttle is tested without
  sleeping, SQLite or the network.

### 3.2 Persistence (`internal/store`)

Two small tables, created like the existing ones in `store.Open`:

```sql
CREATE TABLE IF NOT EXISTS alert_state (
  category TEXT PRIMARY KEY, last_sent_at TEXT, suppressed INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS alert_sends (sent_at TEXT NOT NULL); -- pruned to 24h on write
```

`alert_sends` exists only for the global caps. `alert_state` alone can't count sends
per hour or per day. Keeping 24h of rows covers both windows. `AlertSendsSince`
returns the send times, not just a count, so one read serves both caps and gives
the cap notice its "until <time>" (the oldest send in the window, plus 24h).

`last_sent_at` is nullable because a global cap can suppress a category that has
never sent, and its count still has to be stored. Times are stored in a
fixed-width UTC format, not `RFC3339Nano`: that trims trailing zeros, so
`10:00:00Z` sorts after `10:00:00.5Z`, and these windows are compared as strings
in SQL. Persisting matters
most for the daily cap: a crash-looping worker that reset it on every restart
could spend the whole Free-plan budget.

### 3.3 Mailgun changes

- `send` currently applies `replySubject`, which adds a "Re:" prefix, inside the
  function. Move that to the caller (`deliver`) so `send` takes its subject
  verbatim. Then add `SendAlert(ctx, to, subject, text)` as a thin call to `send`
  with no attachments and no threading headers.
- Alerts come from `REPLY_FROM`. Subject: `[FileMill] <summary>`.
- `Service` gets a `reporter alert.Reporter` field that defaults to `alert.Nop{}`,
  plus `SetReporter`.
- `fileConfig` gets `alert_recipient` (empty means disabled),
  `alert_cooldown_minutes`, `alert_max_per_hour` (default 10) and
  `alert_max_per_day` (default 20).

### 3.4 App changes

- `App` gets `reporter alert.Reporter` (default `Nop`) and `SetReporter`. It's a
  setter, not a constructor argument, because `app.Open` runs before the Mailgun
  service that the real reporter needs exists.
- `execute` is reshaped around the split in §2. Systemic branches call `Report` with
  the job ID, operation, input name, message and the last 2 KB of transformer
  output (already captured by `CombinedOutput`).
- A `defer`/`recover` around each job marks the job failed, reports `panic` with
  the stack, and lets `Run` continue.
- `Run` tracks the time of the first consecutive claim error and reports
  `worker-claim` once it has persisted for ≥1 min.

### 3.5 Delivery failure sensitivity

A 3-tick grace period is 3 seconds, which isn't a blip ride-out. Instead
`Service` keeps an in-memory `firstFailure map[int64]time.Time` keyed by
submission ID. It is set on the first failure, cleared on success, and alerts
(`delivery` or `publish`) once a submission has failed for ≥5 min.
`MarkEmailDelivered` failures skip the grace period: the database is failing, and
every restart until it recovers sends the sender one more duplicate.

The resend storm is fixed ahead of phase 3. It used to happen when the send
succeeded but the mark failed, so the reply went out again on every 1-second
tick. Now `Service.sentUnmarked` remembers a submission whose reply Mailgun
accepted, and later ticks retry only the mark. #6's crash window (a crash between
send and mark, or a send that timed out on our side after Mailgun accepted it) is
accepted and documented on `deliver`: each costs one duplicate. A per-submission
retry cap and dead-letter state is a separate future issue, and this map is its
natural first step.

### 3.6 Reporting crashes across a restart

The supervisor can't send email, and a crashed process can't report itself. So:

1. `Supervise-FileMill.ps1` sets `FILEMILL_PREVIOUS_EXIT` (the last exit code;
   unset on the first launch) and `FILEMILL_RAPID_RESTARTS` in the child's
   environment before each launch.
2. At startup, once the reporter is wired, `run` reports `restart` if
   `FILEMILL_PREVIOUS_EXIT` is set. The summary is "restarted after exit code N",
   plus "(crash-loop: K rapid restarts)" when K ≥ 4, the supervisor's existing
   threshold.
3. `store.Open` already marks any `running` job `interrupted`. Return the count
   from `Open` (or expose it) and fold it into the same `restart` alert.

Because the throttle is persisted (§3.2), a crash-loop sends **one** `restart`
alert per cooldown with a suppressed count, not one per restart. The case this
can't cover is a worker that dies before the reporter is wired (startup `fatal`),
which is left to heartbeat #5.

### 3.7 Wiring in `main.go` (`run`, continuous mode only)

```
app.Open → mailgun.Load → if alert_recipient set:
    emailer := alert.NewEmailer(mail, store, …); go emailer.Run(ctx)
    application.SetReporter(emailer); mail.SetReporter(emailer)
    report restart / interrupted jobs (§3.6)
```

`--once` mode and a Mailgun-less configuration keep `Nop`. `emailer.Run` should
drain the queue on shutdown within the existing 10s window, so an alert raised
during shutdown still goes out.

---

## 4. Phases (one PR each; tests with fakes written first, per project convention)

**Phase 1 — `internal/alert` core.**
- Write `fakeMailer`, `fakeLedger` and a fake clock, then the tests, then
  `Emailer`.
- Tests:
  - N identical reports inside the cooldown → 1 email; the next email after the cooldown carries a suppressed count of N-1.
  - Different categories don't suppress each other.
  - The hourly cap holds across categories.
  - The daily cap holds across categories and across hours: the 21st alert in a
    rolling 24h is suppressed even when the hourly cap would allow it. The 20th
    is the "daily cap reached" notice. The cap reopens as the oldest send leaves
    the window.
  - The daily cap survives a restart (a new `Emailer` over the same ledger).
  - A failed send is logged, not retried or re-reported.
  - `Report` never blocks when the queue is full.
  - Throttle state survives a new `Emailer` built over the same ledger, simulating a restart.
- Real `Ledger` in `store`, with its own tests against a temp DB.
- **Nothing is wired yet**, so there is no behavior change.

**Phase 2 — job taxonomy split.**
- `App.SetReporter` plus a `fakeReporter` in the `app` tests.
- Table-driven `execute` tests use tiny fake transformer scripts: clean
  `success:false`, `success:false` with nonzero exit, timeout, missing
  `result.json`, invalid contract version, `success:true` with nonzero exit,
  missing transformer, and panic recovery.
- The assertion is that *only* the systemic cases report, and job statuses and
  messages are unchanged from today.

**Phase 3 — Mailgun sites and wiring.**
- Move `replySubject` to the caller and add `SendAlert`.
- Config fields, `Service.SetReporter`, and the 5-minute delivery and publish
  grace period.
- Immediate `delivery-mark`, `publish-orphan`, `intake`, `route-config`, and both
  sweeps.
- Wire everything in `main.go`, and add `alert_recipient` to
  `email.yaml.example`.
- Tests go through the existing `fakeEngine` plus a `fakeReporter`.
- **Alerting goes live here.**

**Phase 4 — crash reporting across restarts.**
- Supervisor env vars, the startup `restart` report, and the interrupted-job count
  from `store.Open`.
- Add `recover` to the `Deliver` tick and both sweep loops; each reports `panic`
  and keeps looping.
- Remove the "log-only; alerting tracked in issue #7" wording from the supervisor
  script and README.
- Verify by hand: kill the worker from an elevated shell and confirm that one
  `restart` email arrives, and that a forced crash-loop still produces one.

**Phase 5 — live verification.**
- Set the real `alert_recipient` in the gitignored `config/email.yaml`.
- Trigger one alert per sink: a transformer that exits 1 with no result, a bad
  Mailgun domain for delivery, and a restart.
- Confirm the throttle by forcing a repeated failure for 20 minutes and expecting
  two emails, the second carrying a suppressed count.

**Estimate:** about 3 days. Phase 1 (the throttle and persistence) is the bulk.
Phases 2 and 3 are about 1 day together. Phase 4 is half a day.

---

## 5. Decisions (recommended defaults; change before Phase 3 if needed)

1. **Throttle style:** per-category cooldown (15 min) with suppressed counts, plus
   global caps of 10/hour and **20/day**. No digest emails. The daily cap is
   set by the Mailgun **Free plan** (100 sends/day shared with replies, hard
   stop at the limit). Revisit it if the account moves to a paid plan, where
   alert volume is a rounding error in the bill.
2. **Transformer output in alerts:** include the last 2 KB. The recipient is the
   operator, who already holds the input files under `data/jobs/`, so this reveals
   nothing new to them. It is also the most useful diagnostic.
3. **From address:** reuse `REPLY_FROM`. No new Mailgun sender to set up.
4. **Delivery sensitivity:** 5 min of continuous failure per submission; immediate
   for `delivery-mark`.
5. **Heartbeat:** out of scope here, tracked as #5. It is the only cover for
   startup fatals, a Mailgun outage, and a machine that is off.
6. **Ordering with #6:** done before Phase 3. A failed mark no longer resends the
   reply every second (§3.5). #6's crash window is accepted and documented. The
   retry cap and dead-letter state for stuck submissions will be their own issue.
