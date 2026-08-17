# FileMill Email Pipeline — Architecture & Handoff Notes

This document describes the inbound/outbound email path that lets a user
submit a file to FileMill by email and receive the transformed result back
by email (Phase 3 of `FileMill Project Definition.md`). It's written for
whoever picks up implementation next — human or AI — so the infrastructure
decisions and the reasoning behind them don't have to be re-derived.

**Status: COMPLETE. The Go handler (`internal/mailgun`, wired into
`cmd/filemill run`) is built and the full pipeline was verified with a live
email on 2026-07-20 — a real PDF to `report@mill.example.com` came back as
a `report.xlsx` reply to the sender.**

> **CORRECTION (2026-07-20), read this first — it overrides several claims
> below.** The Mailgun route action must be **`forward("<webhook URL>")`**,
> NOT `store(notify="<webhook URL>")`. `store(notify=)` POSTs only ~7 KB of
> form-urlencoded *metadata* with attachments referenced by a storage URL —
> the file bytes are NOT inline, so the handler (which reads inline multipart
> `attachment-N` parts) sees no attachment and silently returns 200 with no
> job. `forward(<url>)` POSTs the full message as `multipart/form-data` with
> attachments inline, which is what the handler expects. The multipart payload
> documented in section 5 is the **forward-to-URL** shape, not store-notify.
> Working route: expression `catch_all()` (safe — `mill.example.com` is
> dedicated to this pipeline), action `forward("https://notify.example.com/mailgun-webhook")`.
> Also observed: Mailgun route processing lagged up to ~10 minutes during
> testing, so a delayed result is not a failure. A more robust alternative for
> large attachments is to keep `store()` and have FileMill fetch the file from
> the storage URL with the API key — noted as future work, not built.

---

## 1. Why this architecture

Target: reply to an inbound email within 1–2 minutes (ideally under 1),
where the actual transform step itself takes under 0.1s — so essentially
all the latency budget is network/transport, not compute.

Three inbound patterns were evaluated:

1. **Webhook push** (chosen) — a provider parses inbound mail and POSTs it
   to a URL we control.
2. **Mailbox + IMAP polling** (rejected) — forward mail to a real inbox
   (iCloud was the candidate, specifically to avoid Gmail/Outlook's
   OAuth requirement) and have FileMill poll or IDLE it. Rejected because
   **iCloud does not support standard IMAP IDLE for third-party clients**
   (Apple uses a proprietary `XAPPLEPUSHSERVICE` extension instead), so a
   Go IMAP client would have to poll every 15–30s continuously to hit the
   latency target — fragile and heavier than the alternative.
3. **Store-and-pull hybrid** (e.g. Mailgun's `store()` + a separate pull
   API call) — considered and initially assumed unnecessary, but see the
   CORRECTION above: `store(notify=)` does **not** deliver attachments
   inline (only metadata + a storage URL). The chosen `forward(<url>)`
   action posts the full message inline, so no second API round-trip is
   needed — but that is a property of `forward`, not `store(notify=)`.

Provider choice landed on **Mailgun** over iCloud/Gmail/Outlook because it
requires no OAuth app registration (API key only), and over a raw
Cloudflare Worker because Workers would require writing JavaScript/
TypeScript (or a painful TinyGo+WASM+Node-toolchain workaround) — FileMill
is a Go project and the goal was zero non-Go code in the pipeline.

Cloudflare Tunnel was added because FileMill runs on a personal Windows
laptop (not an always-on server, no static IP, behind NAT) — Mailgun's
`notify()` needs a real public HTTPS URL to POST to, and the tunnel
provides that without port-forwarding.

---

## 2. The pipeline, step by step

```
Sender
  |  sends email to e.g. someone@mill.example.com
  v
Mailgun MX (mxa.mailgun.org / mxb.mailgun.org)
  |  receives via SMTP
  |  evaluates Routes
  v
Mailgun Route match: catch_all()   [or match_recipient(".*@mill.example.com")]
  action: forward("https://notify.example.com/mailgun-webhook")
  (NOT store(notify=...) — see CORRECTION at top; forward posts attachments inline)
  |
  |  stores the message (~3 day retention, backup only — not the primary path)
  |  AND immediately POSTs the fully parsed message as multipart/form-data
  v
https://notify.example.com/mailgun-webhook
  |  DNS: CNAME to <tunnel-id>.cfargotunnel.com (Cloudflare-managed,
  |  auto-created when the tunnel's public hostname route was configured)
  v
Cloudflare edge -> Cloudflare Tunnel (cloudflared, Windows service on the laptop)
  |  persistent outbound connection from the laptop to Cloudflare;
  |  no inbound ports opened on the laptop
  v
cloudflared forwards to the configured local service
  |  Service type: HTTP (not HTTPS — Type=HTTPS was the cause of an
  |  earlier 502; the local Go server does not terminate TLS)
  |  URL: localhost:8080
  v
FileMill Go process, HTTP server on :8080, handler at /mailgun-webhook
  |  <-- this is the piece that needs to be written
  v
  1. Verify Mailgun's HMAC signature (mandatory, see section 4)
  2. Parse the multipart payload into sender/subject/body/attachments
  3. Decide which transformer operation to run (OPEN QUESTION, see section 5)
  4. app.Submit(operation, attachmentPath) — reuse the existing job
     pipeline, do not reimplement job creation
  5. Wait for the job to complete (see section 5)
  6. Send the reply via Mailgun's Send API, with the job's output file(s)
     attached and threaded into the original conversation
```

---

## 3. Infrastructure already configured (do not redo)

- **Domain**: `example.com`, registered and DNS-hosted on Cloudflare. This
  is a test/sandbox domain, intentionally decoupled from the operator's real
  production domain (hosted elsewhere) so mistakes here don't risk
  production mail. If this pipeline proves out, the real domain can be
  migrated later — not yet.
- **Subdomain**: `mill.example.com`, dedicated to this pipeline.
- **DNS records for mill.example.com** (all verified green in Mailgun):
  - 2x TXT (SPF, DKIM)
  - 2x MX -> Mailgun's inbound mail servers
  - 1x CNAME (open/click tracking) — must be "DNS only" (grey cloud), not
    proxied through Cloudflare
  - 1x TXT DMARC at `_dmarc.mill.example.com` (`p=none`, monitor-only —
    matters for deliverability of the *reply*, not for receiving)
  - Gotcha encountered: Cloudflare's own **DMARC Management** feature will
    silently generate and reassert a competing DMARC record with a
    `dmarc-reports.cloudflare.net` address unless disabled per-zone
    (Email -> DMARC Management -> disable). If DMARC ever reverts
    unexpectedly, check there first before assuming a Mailgun-side issue.
  - Gotcha encountered: Cloudflare's DNS form auto-appends the zone to
    whatever you type in the Name field. A record must be entered as
    `_dmarc.mill` (not `_dmarc`, and not the full `_dmarc.mill.example.com`)
    to resolve to the correct host.
- **Mailgun**: domain `mill.example.com` added and fully verified. Working
  route: expression `catch_all()`, action
  `forward("https://notify.example.com/mailgun-webhook")` (see CORRECTION at
  top — `forward` delivers attachments inline; `store(notify=)` does not).
  Confirmed working with a real inbound email producing a reply on 2026-07-20.
- **Cloudflare Tunnel**: `cloudflared` installed via winget, running as a
  Windows service (`cloudflared.exe service install <token>`; if it shows
  installed-but-stopped, `Start-Service Cloudflared` from an elevated
  PowerShell fixes it). Public hostname route: `notify.example.com` ->
  HTTP -> `localhost:8080`. Confirmed working end-to-end.
- **Reboot/logon persistence**: `scripts/Install-FileMillScheduledTask.ps1`
  already registers a scheduled task that runs `filemill.exe run` at user
  logon (see `scripts/Start-FileMill.ps1`). Decision: the webhook HTTP
  server should be started from *within* the `run` subcommand (same
  process, same worker loop) rather than as a separate service, so this
  existing scheduled task covers the whole pipeline's persistence with no
  new infrastructure. `cloudflared` has its own independent persistence
  via its Windows service.

---

## 4. Signature verification (mandatory, not optional)

`notify.example.com` is a public URL. Anyone who discovers it could POST
forged data pretending to be an inbound email. Mailgun signs every notify
POST with HMAC-SHA256 over `timestamp + token`, keyed with the Mailgun API
key. The three fields (`token`, `timestamp`, `signature`) arrive as
regular form fields in the same multipart body. Recompute and compare
before trusting anything else in the request:

```go
func verifyMailgunSignature(webhookSigningKey, timestamp, token, signature string) bool {
    mac := hmac.New(sha256.New, []byte(webhookSigningKey))
    mac.Write([]byte(timestamp + token))
    expected := hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(expected), []byte(signature))
}
```

Reject with 401 on any mismatch, before parsing anything else in the body.

**Important: this uses a different key than the outbound API key.** Mailgun
has a dedicated **HTTP webhook signing key** (dashboard: Sending -> API
Keys -> "HTTP webhook signing key"), separate from the Private API Key
used for the Send API in section 6. Using the Private API Key here will
silently fail verification. Config needs both:

- `MAILGUN_API_KEY` — Private API key, used for the outbound Send API only.
- `MAILGUN_WEBHOOK_SIGNING_KEY` — used only for `verifyMailgunSignature`.

---

## 5. The payload shape

This is `multipart/form-data`, **not JSON**. Confirmed from a real captured
payload (both Mailgun's synthetic "sample POST" and a genuine test email
produced the same shape). Relevant fields:

| Field | Notes |
|---|---|
| `sender` | Envelope sender — reply to this address |
| `from` | Display "From" header, informational |
| `subject` | Original subject; prefix with "Re: " for the reply if not already present |
| `stripped-text` | Reply-only body content, quoted history already removed — prefer this |
| `body-plain` | Full plain-text body including quoted history — fallback if `stripped-text` is empty |
| `Message-Id` | Original message ID — set as both `In-Reply-To` and `References` on the reply for proper threading |
| `attachment-1`, `attachment-2`, ... | Actual file parts (not base64-in-JSON) — use `r.MultipartForm.File`, not `r.FormValue` |
| `attachment-count` | How many attachment-N fields to expect (or just iterate `r.MultipartForm.File`, which only contains what's actually present) |
| `token`, `timestamp`, `signature` | Signature verification, see section 4 |

Go's `mime/multipart` package via `r.ParseMultipartForm(maxSize)` handles
the split between value fields (`r.MultipartForm.Value`) and file fields
(`r.MultipartForm.File`) automatically — file fields are anything with a
`filename=` in its `Content-Disposition`.

A trimmed real example of the multipart body (attachment binary content
omitted for brevity):

```
Content-Disposition: form-data; name="subject"
Re: Sample POST request
--boundary
Content-Disposition: form-data; name="sender"
ross@mill.example.com
--boundary
Content-Disposition: form-data; name="stripped-text"
Hi Alice,
This is Bob.
I also attached a file.
--boundary
Content-Disposition: form-data; name="Message-Id"
<517ACC75.5010709@mill.example.com>
--boundary
Content-Disposition: form-data; name="attachment-1"; filename="crabby.gif"
Content-Type: image/gif
<binary data>
--boundary
Content-Disposition: form-data; name="attachment-count"
2
--boundary
Content-Disposition: form-data; name="token"
095e0417f50b0fc609d51ae6a8140452888572d4fe829594cc
--boundary
Content-Disposition: form-data; name="timestamp"
1784430981
--boundary
Content-Disposition: form-data; name="signature"
d2b7af2849575ef0ba009e38d7fb3d3922b2ce793f10ccea58b549266d6788d1
--boundary--
```

---

## 6. Sending the reply

Mailgun's Send API, not a separate provider:

```
POST https://api.mailgun.net/v3/mill.example.com/messages
Authorization: Basic base64("api:<MAILGUN_API_KEY>")
Content-Type: application/x-www-form-urlencoded

from=<REPLY_FROM>
to=<original sender>
subject=Re: <original subject>
text=<result message>
h:In-Reply-To=<original Message-Id>
h:References=<original Message-Id>
attachment=<job output file(s), multipart>
```

Attach the transformer's actual output file(s) from the completed job's
`output/` directory (see `contract.Result.OutputFiles`), not just a text
summary.

---

## 7. Fitting into the existing FileMill architecture

Do **not** reimplement job creation, file copying, or transformer
dispatch — all of that already exists and works:

- `internal/app.App.Submit(operation, sourcePath string) (jobID string, err error)`
  — creates the job workspace, copies the file, writes `job.json`, queues
  it in the store. The webhook handler should call this directly.
- `internal/app.App.Run(ctx, once)` — the existing worker loop that pulls
  queued jobs and executes the matching transformer subprocess.
- `internal/config` — loads `config/transformers.yaml`
  (operation -> command + accepted extensions). `Transformer.Accepts(filename)`
  already does extension matching — relevant to the open question below.
- `internal/contract` — the JSON contract shared with transformer
  subprocesses (`Job`, `Result`).
- `internal/store` — SQLite-backed job status tracking
  (`queued` -> `running` -> `succeeded`/`failed`).

Per the project's own design philosophy ("Email should be an adapter, not
the core" — see `FileMill Project Definition.md`), the new code belongs in
a new `internal/mailgun` package: payload parsing, signature verification,
and reply-sending. It should depend on `internal/app`, not the other way
around.

Suggested shape:

```
internal/mailgun/
  webhook.go   — HTTP handler, signature verification, payload parsing
  send.go      — reply-sending via Mailgun's API
  config.go    — MAILGUN_API_KEY / MAILGUN_WEBHOOK_SIGNING_KEY / MAILGUN_DOMAIN / REPLY_FROM / LISTEN_ADDR
```

Wired into `cmd/filemill/main.go`'s existing `run` case: start the HTTP
server (in a goroutine) alongside the existing `application.Run(ctx, once)`
call, so one process and one scheduled task covers both.

---

## 8. Open decisions — resolve before/while implementing

1. **How does an inbound email select which transformer/operation runs?**
   Not yet decided. Options discussed:
   - Per-operation address (`pdf-compress@mill.example.com`) — parse from
     the `recipient` field.
   - Subject-line keyword/syntax.
   - Auto-detect from the attachment's file extension via the existing
     `Transformer.Accepts()` — least new code since the matching logic
     already exists, but only works if extensions map unambiguously to a
     single operation.
2. **How does the webhook handler know when the submitted job completes,**
   so it knows when to send the reply? `App.Run()` processes jobs
   asynchronously via the queue. Since the transform itself is expected to
   take under 0.1s and the worker loop's idle poll interval is ~1s,
   polling `app.Job(id)` in a short loop after `Submit()` should resolve
   well within budget — but this hasn't been implemented or timed yet. An
   event-driven alternative (a completion channel/callback added to `App`)
   is possible if polling proves too crude, but isn't necessary to start.
3. Confirm `MAILGUN_API_KEY` / `MAILGUN_DOMAIN` / `REPLY_FROM` should be
   plain environment variables (consistent with secrets not living in
   `config/transformers.yaml`, which is presumably version-controlled).

---

## 9. End-to-end test checklist once built

- [ ] Real email with attachment to `mill.example.com` (or a per-operation
      address, depending on decision #1) triggers a job via `app.Submit`
- [ ] Job appears in `data/jobs/<id>/` with correct input file
- [ ] Job transitions queued -> running -> succeeded in the store
- [ ] Reply email arrives with the correct output file attached
- [ ] Reply threads correctly (shows as a reply, not a new message) in a
      real mail client
- [ ] Round-trip time from send to reply is comfortably under 1–2 minutes
- [ ] Unsigned/forged POSTs to `/mailgun-webhook` are rejected with 401
