# Sheets-Link Delivery — Implementation Plan

**Status:** planned, not started. Written after the design discussion of 2026-08-07.

## Goal

Add a new **delivery mode** to FileMill's Mailgun adapter. For selected
recipient addresses, instead of emailing the transformer's `.xlsx` back as an
attachment, FileMill uploads the file to Google Drive, converts it to a native
Google Sheet, sets link-sharing to *anyone-with-the-link-can-edit*, and emails
the **link** back to the sender (threaded, via the existing Mailgun reply).

This is **not a new adapter.** Intake stays email. Only the outbound half
changes, on a per-route basis. See [DATA-FLOW.md](DATA-FLOW.md) for the current
round trip.

## Decisions locked (2026-08-07)

1. **Sharing:** link-sharing, `type=anyone, role=writer` (world-editable by
   link). Accepted: the output is a reformat of data the sender already emailed
   in. Nuance on record: an anyone-with-link URL is a *bearer* capability
   (broader than the directed inbound email), but the underlying data exposure
   is unchanged.
2. **Auth:** OAuth **user** refresh token; files land in the operator's
   **personal Gmail** Drive (decided 2026-08-07, not a Workspace account).
   Scope: **`drive.file` only** (per-file, app-created files) — non-sensitive,
   so a published app avoids verification. Never use full `drive` (restricted →
   security assessment). App configured External + published to production for a
   long-lived refresh token. See "OAuth setup" below.
3. **Scope:** `iwk@mill.keywind.cc` gets this mode. It changes from
   attachment-Sheets to link-Sheets. `workerlist@` stays attachment-Sheets.
4. **Retention:** delete uploaded Drive files after 30 days.
5. **Senders:** `allowed_senders` stays open (`[]`). Anyone emails their own
   data in, gets their own data back.
6. **Link delivery:** FileMill emails the link via the existing Mailgun reply —
   *not* Google's "someone shared a file" notification. Keeps threading, keeps
   Mailgun as the single outbound channel, works for non-Google senders.

## Architecture

The delivery loop ([outbound.go](../internal/mailgun/outbound.go)) `deliverPending`
already: finds submissions whose jobs are all terminal, builds `text` + a list
of output file paths, calls `send(...)`, then `MarkEmailDelivered`. The feature
adds a per-recipient branch before `send`.

`send()` needs **no changes**: a link-only reply is just `send` called with an
empty `outputs` slice (the attachment loop becomes a no-op) and a `text` body
that includes the link.

### Delivery-mode config — parallel flat map in email.yaml

Leave the existing `routes` map (address → operation) untouched. Add a second
flat map, address → delivery mode. Absent/`email` = today's attachment reply;
`sheets-link` = the new mode. Illustrative:

```yaml
delivery:
  iwk@mill.keywind.cc: sheets-link
```

Return mode is a *route* concern, not a transformer property — this is the
asymmetry with the `layout` option (which is operation-carried). So it lives in
email.yaml, parsed in [mailgun/config.go](../internal/mailgun/config.go) into a
`map[address]mode` on the Service, looked up by recipient in `deliverPending`.

### The Google client behind an interface (+ fake)

Mirror the `Engine` pattern ([service.go](../internal/mailgun/service.go)). Define
a small interface, e.g.:

```
type Publisher interface {
    // Publish uploads path as a Google Sheet, sets anyone-with-link edit
    // sharing, and returns the file id and shareable link.
    Publish(ctx, path, name string) (fileID, link string, err error)
    Delete(ctx, fileID string) error   // for the retention sweep
}
```

Real impl: a new `internal/gsheets` package wrapping the Google Drive API
(upload xlsx with conversion to `application/vnd.google-apps.spreadsheet`, then
`permissions.create` type=anyone role=writer, then read `webViewLink`). Tests
drive `deliverPending` with a fake Publisher — no network. Per the DI + isolation
-test preference, **write the fake and the delivery-loop tests before the real
Google impl.**

## Idempotency (the load-bearing detail)

Delivery is at-least-once: `send` then `MarkEmailDelivered`, retried on the next
tick if `send` fails. A duplicate *email* is harmless; a duplicate *upload* is
not — it creates a second world-editable Sheet of someone's PII and doubles the
cleanup burden.

Fix — make the upload idempotent by recording its result **before** the fallible
email step (the commit-point pattern [intake.go](../internal/mailgun/intake.go)
already uses):

1. For a `sheets-link` submission, first check for a stored `fileId`/link.
2. If none: `Publish(...)`, then **immediately persist** `fileId` + `link` +
   `created_at`, keyed on the submission. Commit point.
3. If present: skip the upload, reuse the stored link.
4. Compose the reply body with the link, `send` with empty `outputs`,
   `MarkEmailDelivered`.

Net: **at-most-once upload, at-least-once email.**

### Schema addition

A record per submission holding `fileId`, `link`, `created_at`. Either a nullable
set of columns on `email_submissions` or a small side table. This record does
triple duty: idempotency (skip re-upload), the link to email, and the handle +
timestamp the 30-day sweep needs.

## Head-of-line blocking (must fix as part of this)

[outbound.go:77-79](../internal/mailgun/outbound.go#L77) returns on the first
`send` error, so one stuck submission stalls **every** later delivery — including
plain-Excel ones. Today that's a brief Mailgun blip; with Google in the path
(expired token, quota, 5xx) a persistent per-submission failure is plausible and
would silently halt all replies. Change the loop to **skip-and-continue** (log
the failure, move to the next submission) so one bad submission can't block the
queue. Consider per-submission failure counting / a dead-letter status as a
follow-up.

## Config cross-validation (log-warning only, no noise)

At config load, for each `sheets-link` delivery route, check that the address's
operation produces `layout: sheets`. **Emit a single warning log line only when
the pairing is inconsistent; log nothing when all pairings are fine.** This is a
quality coupling, not a hard error (Drive will still convert an Excel-layout
xlsx), so do **not** reject the config — just make a future slip visible. Also
document the pairing expectation in an `email.yaml` comment.

## Retention — 30-day cleanup sweep

A periodic sweep (own goroutine, like `Deliver`, but a slow interval — hourly or
daily) that finds stored records older than 30 days, calls `Publisher.Delete`,
and removes/flags the record. Idempotent: a delete that 404s (already gone) is
success. Runs only in continuous worker mode.

## OAuth setup (out-of-band, operator action)

- One-time **interactive browser consent** by the operator to mint the refresh
  token; store it locally (Drive secrets belong in the environment, like the
  existing Mailgun keys in [mailgun/config.go](../internal/mailgun/config.go) —
  do **not** commit tokens).
- **Decided:** personal Gmail, OAuth app **External + published to production**
  for a long-lived refresh token, **`drive.file` scope only** (non-sensitive →
  no verification). Console setup: create project → enable Drive API → consent
  screen (External, add `drive.file`, add self as test user) → Publish App →
  create **Desktop app** OAuth client → download client id/secret.
- Secrets via env, never committed: `GOOGLE_OAUTH_CLIENT_ID`,
  `GOOGLE_OAUTH_CLIENT_SECRET`, `GOOGLE_OAUTH_REFRESH_TOKEN` (matching the
  `MAILGUN_*` pattern in [mailgun/config.go](../internal/mailgun/config.go)).
- Refresh-token bootstrap: quick via OAuth 2.0 Playground (use own credentials),
  or the proper `filemill auth google` loopback helper (a build step).
- **Verify against current Google docs at build time:** that `drive.file` is
  still non-sensitive, that publishing removes the Testing 7-day token expiry,
  and whether native Sheets count against the 15 GB quota (affects retention).

## Testing

- Fake `Publisher`; isolation tests for `deliverPending`:
  - `sheets-link` route → uploads once, emails a link, no attachment.
  - retry after a `send` failure → **no** second upload (reuses stored link).
  - `email` route → unchanged (attachment path).
  - skip-and-continue → a failing submission doesn't block a following good one.
- Config test: `sheets-link` on a non-`sheets` operation logs exactly one
  warning; a correct pairing logs nothing.
- Keep the Google impl itself out of unit tests (network); gate any live check
  behind an env var like the existing `FILEMILL_E2E`.

## Sequencing / go-live

1. Schema record + `Publisher` interface + fake + delivery-loop branch +
   skip-and-continue + config warning — all behind config that defaults off.
2. Real `gsheets` package; bootstrap the refresh token out-of-band.
3. Retention sweep.
4. Flip `iwk@` to `sheets-link` in `email.yaml`; config-reload; verify
   end-to-end over real email (link arrives, opens, editable; Excel path for
   `workerlist@`/`excel@` unaffected).
5. Confirm a real upload appears in the operator's Drive and the 30-day sweep
   deletes a back-dated test record.

## Risks / notes

- **New external dependency in the delivery path** — a shift from the tool's
  local-first character. Opt-in per route; email delivery stays the default.
- **Token expiry** is the most likely unattended-failure mode; ties into the
  error-alerting work ([ERROR-ALERTING-PLAN.md](../ERROR-ALERTING-PLAN.md)).
- **World-editable PII links** — accepted, but note it interacts with the still-
  open `allowed_senders`.
