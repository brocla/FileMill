# FileMill Error Alerting — Plan

Goal: when something goes **wrong** in FileMill, email an operator alert to
`support@keywind.cc`, so an unattended background service (it runs at logon on
a personal laptop) doesn't fail silently.

Status: **plan only, not implemented.** Written 2026-07-20.

The *sending* is trivial — FileMill already has a working Mailgun Send path
(`Service.send` in `internal/mailgun/outbound.go`). The work is deciding
**which** failures alert, **not** drowning `support@` in noise, and handling the
cases email structurally can't cover.

---

## 1. Guiding principles

1. **Alert on systemic failures, stay silent on expected ones.** A malformed
   PDF that a transformer cleanly rejected is a *normal outcome the sender
   already hears about* — it must not page an operator. A transformer that
   crashed, timed out, or produced no valid result is *systemic* — alert.
2. **Throttle everything.** Loops and bots can generate thousands of identical
   errors. Deduplication + rate limiting is mandatory, not optional.
3. **Never let alerting recurse or crash the app.** An alert-send that fails is
   logged and dropped; it must never trigger another alert or panic.
4. **Email can't report that the app is gone.** Total-crash / machine-asleep
   detection needs an external heartbeat, not self-sent email. In scope as a
   complementary Phase 4, called out honestly.

---

## 2. What alerts, and what doesn't

| Failure | Code site | Alert `support@`? | Why |
|---|---|---|---|
| Forged / replayed webhook (401) | `webhook.go` `receive` | **No** | Bots scan public URLs; would flood |
| Malformed / oversize body (400) | `receive` | **No** | Client noise |
| Unrouted recipient / disallowed sender (silent 200) | `receive` | **No** | Benign |
| **Intake failure (500)** — storage / filesystem / Submit | `receive` → `intake` | **Yes** | Systemic |
| **Job: transformer missing from config** | `app.go` `execute` | **Yes** | Systemic (misconfig) |
| **Job: transformer timed out** | `execute` | **Yes** | Systemic |
| **Job: missing / corrupt `result.json`** | `execute` → `readResult` | **Yes** | Systemic (contract violation) |
| **Job: nonzero exit with no usable result** | `execute` | **Yes** | Systemic (crash) |
| Job: transformer cleanly reported `success:false` (bad input) | `execute` | **No** | Expected; sender already told in the reply |
| **Delivery failure** — Mailgun send non-2xx | `outbound.go` `deliverPending`/`send` | **Yes (throttled)** | Replies aren't going out |
| **Worker loop dies** (`store.Next` error → `fatal`) | `app.go` `Run` → `main` | **Yes** | Catastrophic |
| **Panic** in worker or delivery goroutine | `Run`, `Deliver` | **Yes** | Would otherwise crash silently |

The subtle one is the two kinds of "job failed." Today `a.finish(id,"failed",msg)`
lumps them. The split hinges on **whether the transformer honored the contract**:
a valid `result.json` with `success:false` = *handled* (no alert); a crash /
timeout / missing / corrupt result = *systemic* (alert).

---

## 3. Design

### 3.1 A `Reporter` dependency (DI, testable)

Introduce an interface, following the `Engine` pattern already in the codebase:

```go
type Reporter interface {
    // Report records a systemic failure. Implementations must be non-blocking
    // enough for hot paths, must throttle, and must never panic or recurse.
    Report(category string, detail string, err error)
}
```

- A **no-op reporter** is the default (alerting disabled).
- A **Mailgun-backed reporter** emails `support@keywind.cc` via the existing
  send path, with throttling in front.
- Both `*app.App` and the mailgun `Service` take a `Reporter` (constructor
  injection), so job-side and email-side systemic failures funnel to one place.
- Tests use a `fakeReporter` that records calls — per the project's
  isolation-testing preference.

### 3.2 Throttle / dedupe (the mandatory middle layer)

A small stateful wrapper in front of the Mailgun reporter:

- **Per-category cooldown:** at most one email per `category` per window
  (default ~15 min). The next email for a suppressed category includes a
  "N more occurrences since last alert" count.
- **Global cap:** a hard ceiling (e.g. ≤ N alert emails/hour) as a backstop.
- Rationale: the delivery loop ticks every second, so an unthrottled delivery
  failure = one email/second; a bot spraying 401s (already excluded) would be
  worse still.

### 3.3 The feedback loop

- The Mailgun reporter's own send failures are **log-only** — never re-reported.
- "Mailgun send is failing" therefore may not reach `support@` at all (it needs
  the very channel that's down). This is the structural gap that Phase 4's
  heartbeat covers.

### 3.4 Configuration

Add to `config/email.yaml` (non-secret; secrets stay in env):

```yaml
alert_recipient: support@keywind.cc     # empty/absent => alerting disabled
# alert_from: filemill@mill.keywind.cc   # optional; defaults to REPLY_FROM
# alert_cooldown_minutes: 15
```

Alerting is enabled only when `alert_recipient` is set and Mailgun is configured.

### 3.5 Alert content

Subject: `[FileMill] <category>`. Body: timestamp, category, job ID / operation /
input filename where relevant, the error text, and the transformer's captured
stderr for job crashes. **Open decision:** stderr can contain fragments of the
input file — decide whether to include it or a truncated/redacted form.

---

## 4. Phasing

Each phase builds and tests independently.

- **Phase 0 — `Reporter` interface + no-op + `fakeReporter` + isolation tests.**
  Inject into `App` and `Service`; default no-op; nothing emails yet.
- **Phase 1 — split systemic vs handled in `execute`.** Call `reporter.Report`
  at the systemic branches only (transformer-missing, timeout, missing/corrupt
  result, crash-without-result). No email yet — verify via `fakeReporter` that
  *only* systemic paths report and handled `success:false` does not. This is
  good design independent of alerting.
- **Phase 2 — Mailgun-backed reporter + config + throttle/dedupe.** Reuse the
  `send` path (with the `sendBase` injection already present for testing).
  Isolation-test the throttle: N rapid identical reports → one email with a
  suppressed-count.
- **Phase 3 — wire the remaining sites:** delivery failures (throttled),
  `recover()` in the `Run` and `Deliver` goroutines (report then continue/exit
  cleanly instead of crashing), and the worker-loop fatal in `main`.
- **Phase 4 (adjacent, optional) — external heartbeat.** FileMill pings a
  dead-man's-switch (e.g. healthchecks.io) every few minutes; if the ping stops,
  *that* service emails you. This is the only thing that catches "the process is
  dead / the laptop slept," which self-sent email cannot. Mostly external setup
  plus a small ticker in `run`.

Partial coverage to accept: **startup `fatal()`s** (bad config, can't open the
DB, incomplete Mailgun env) happen *before* the reporter exists, so they won't
email unless we special-case loading just enough config early to send one
message before exit. Deferred; the heartbeat covers "never started" anyway.

---

## 5. Scope estimate

- Phase 0–1: ~0.5–1 day (interface, split, tests).
- Phase 2: ~1 day (reporter, config, throttle — the throttle is the bulk).
- Phase 3: ~0.5 day (wiring + panic recovery).
- Phase 4: mostly external config + a small ticker.

Roughly 2–3 days for something trustworthy. The sending is minutes; the
taxonomy split, throttle, and panic-recovery are the real work.

---

## 6. Open decisions (resolve before Phase 2)

1. **Cooldown/digest style:** per-category cooldown with suppressed-count
   (recommended) vs a periodic digest email vs alert-on-every-occurrence
   (rejected — spam).
2. **Transformer stderr in alert bodies:** include (best for diagnosis) vs
   omit/redact (may echo input-file content).
3. **From address:** reuse `REPLY_FROM` (`filemill@`) vs a dedicated `alerts@`.
4. **Heartbeat (Phase 4):** in scope now, or a separate later effort?
5. **Delivery-failure sensitivity:** how many consecutive failures before the
   first alert (immediate vs after 2–3 ticks, to ride out a blip)?
