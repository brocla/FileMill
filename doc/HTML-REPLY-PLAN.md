# HTML reply bodies

Let a transformer return an HTML version of its message, and send the email
reply as `multipart/alternative`: the existing plain text plus that HTML.
Transformers that don't return HTML get exactly today's reply.

## Why

The first transformer that needs this is `check_temple_schedule`
(`../check_temple_schedule`). It emails back a report on a shift's schedule:
several sections, each with a status, a few problem lines, and a table of
counts per 15-minute block. As plain text it is hard to scan, and the table only
lines up in a fixed-width font, which phones don't use. The HTML version has
coloured section cards, emoji headings, a one-line grey explanation under each
heading, and a real table. It will produce `message_html` from day one, and it
won't be registered in FileMill until this change is in.

## Today

- `contract.Result` (`internal/contract/contract.go`) has one `Message` string.
- `app.run` passes `result.Message` to `finish`, then to `store.Complete`,
  which writes `jobs.message`.
- `mailgun.Service.deliver` (`internal/mailgun/outbound.go`) builds one text
  body for the submission:
  - one line per job: `"<input name> (<label>): <message>"`;
  - then `withLinks` (sheets-link mode);
  - then `withSupportFooter`.
- `send` posts it to Mailgun as the single `text` field.

## The change

### 1. Contract: optional `message_html`

```go
type Result struct {
    ...
    Message     string `json:"message"`
    MessageHTML string `json:"message_html,omitempty"`
    ...
}
```

- **Additive.** `contract_version` stays `"1"`. Go's `encoding/json` already
  ignores unknown fields, so a transformer may send `message_html` before
  FileMill reads it, and an old FileMill simply drops it. Deploy order doesn't
  matter.
- **Success only.** Only used when `success` is true. A failed job's reply
  stays plain `message`, as today. Failure messages are one sentence and gain
  nothing from HTML.
- **`message` stays required and is still sent.** It becomes the `text/plain`
  part, so a reader whose client shows plain text gets today's report.

### What `message_html` must be

Document this in the README's "Writing a transformer" section:

- **A fragment**, placed inside the reply's `<body>`. No `<!doctype>`,
  `<html>`, `<head>` or `<body>`.
- **Email-safe:**
  - inline `style` attributes only;
  - tables for layout;
  - no `<script>`, no external stylesheets, fonts or images;
  - designed for widths up to about 600px.
- **The transformer escapes its own data.** FileMill inserts the fragment
  verbatim. Transformers are operator-installed code, so the fragment is
  trusted the way the transformer's own output files are.
- **Size:** keep it well under 100 KB. Gmail clips messages over about 102 KB
  behind a "View entire message" link.

### 2. Store: keep it with the job

The reply is built later from the database (`PendingEmails`), not from the
workspace, so the HTML has to be stored.

- Add a column: `jobs.message_html TEXT NOT NULL DEFAULT ''`.
  - New databases: add it to the `CREATE TABLE jobs`.
  - Existing databases: add a migration in the style of
    `migrateJobFilesDeletedColumn`, i.e. a `PRAGMA table_info` check, then
    `ALTER TABLE jobs ADD COLUMN message_html TEXT NOT NULL DEFAULT ''`.
    SQLite allows `NOT NULL` with a constant default on `ADD COLUMN`.
- `store.Job`: add `MessageHTML string`.
- `Complete(id, status, message, messageHTML string)`:
  - its only caller is `app.finish`;
  - `failSystemic` and the other failure paths pass `""`;
  - `InterruptRunning` doesn't go through `Complete` (it updates rows in SQL),
    and needs no change: the column's default covers interrupted jobs.
- Read the column in the `PendingEmails` job query
  (`SELECT j.id, ..., j.message, ...`). That is the only read delivery uses.
  `Get` reads it too, so the app's tests (and `filemill status`) see the
  whole job.
- `app.run`: on success, call
  `a.finish(j.ID, store.StatusSucceeded, result.Message, result.MessageHTML)`.
  Every other `finish` call passes `""`.
- **Rollback stays safe.** The `DEFAULT ''` means the previous binary, which
  doesn't know the column, can still insert jobs into a migrated database.

### 2a. Log only the message's first line

`finish` logs `job=<id> status=<s> message=%q` with the whole message. This
transformer puts its entire multi-line report in `message`, so every job would
write one very long line full of `\n` escapes to `filemill.log`. Log the first
line only, with `...` appended when anything was cut. The full message is still
in the database and in the reply. The HTML is never logged.

### 3. Delivery: build an HTML body when any job has one

In `deliver`, keep building `text` exactly as today. Also build `html`, but only
if at least one succeeded job in the submission has a non-empty `MessageHTML`.
Otherwise `html` is `""` and the reply is byte-for-byte today's.

The HTML body:

```html
<!doctype html>
<html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="margin:0;padding:0;">
  <!-- per job, in attachment order: -->
  <p style="...small grey...">{input name} ({label})</p>
  {job.MessageHTML}
     or, for a job without HTML (or a failed one):
  <p style="...">{input name} ({label}): {message}</p>   <!-- escaped -->
  <!-- then, if sheets-link: the same sentence as withLinks, with each link as <a href> -->
  <!-- then, if a support address: the same footer sentence -->
</body></html>
```

- **Escaping.** Escape everything FileMill itself inserts
  (`html.EscapeString`): input names, labels, plain messages, link text and
  `href`s, the support address. Only `MessageHTML` goes in unescaped.
- **Shared wording.** `withLinks` and `withSupportFooter` get HTML siblings
  (`htmlLinks`, `htmlSupportFooter`), or a small shared helper that renders the
  sentence once and formats it two ways. Either way, the wording lives in one
  place.
- **The per-job heading line.**
  - Keep it even for a single job. It is how the sender knows which file the
    report is about.
  - Render it as a small grey caption, not glued onto the front of the
    fragment the way the text version prefixes the message.
  - **It doesn't repeat the fragment's own heading.** `check_temple_schedule`'s
    fragment opens with a navy band holding the PDF's title line and the
    shift, and its `report_html.py` says it expects FileMill to add its own
    heading and footer. FileMill's caption names the *file*; the fragment names
    the *shift*. A transformer should not put the file name in its fragment.
- **One job per reply in practice.** The webhook drops any email with more
  than one attachment it can process ("one per email",
  `internal/mailgun/webhook.go`), so every submission has exactly one job.
  `deliver` still loops over jobs, and a job without HTML (or a failed one)
  simply gets its escaped `<p>`; no extra design or test is spent on mixing
  them.

### 4. Send: add the `html` field

`send` gains an `html string` parameter:

- When it is non-empty, write it as the `html` form field next to `text`.
  Mailgun then builds the `multipart/alternative` message itself, and
  attachments still work.
- `SendAlert` passes `""`. Alerts stay plain text.

## Tests

Follow the existing style: fakes, plus the Mailgun form-capture tests in
`internal/mailgun`.

1. **Contract:**
   - a `result.json` with `message_html` decodes into `MessageHTML`;
   - one without it decodes to `""`.
2. **Store:**
   - `Complete` round-trips `MessageHTML` through `PendingEmails`;
   - opening a database created without the column adds it and keeps existing
     rows, which read as `""`;
   - opening twice is a no-op.
3. **App:**
   - a succeeded job's `message_html` is stored;
   - a failed job with `message_html` in its result stores `""`.
4. **Delivery:**
   - **No job has HTML:** the form has no `html` field, and `text` is unchanged
     (a regression guard).
   - **One job with HTML:** the `html` field contains the fragment verbatim, the
     escaped input name and label, and the support footer. `text` still has the
     plain message.
   - **Failed job:** a failed job's `message_html` is ignored, so its reply has
     no `html` field.
   - **Escaping:** an input name like `a<b>&c.pdf` appears as
     `a&lt;b&gt;&amp;c.pdf` in `html`.
   - **sheets-link mode:** the links appear as `<a href="...">` in `html`, and
     as bare URLs in `text`.
   - **Alerts:** `SendAlert` sends no `html` field.
5. **README:**
   - document `result.json`'s fields, including `message_html` and the fragment
     rules above. The README documents `job.json` in detail but not
     `result.json`;
   - optionally, give `examples/transformers/uppercase` a one-line
     `message_html` to show the shape.
6. **App logging:** a multi-line message is logged as its first line plus
   `...`; a one-line message is logged whole.

## Routing the new transformer

Route `check_temple_schedule` in **plain email** delivery mode, not
sheets-link. It produces no output files, so there is nothing to publish, and
in sheets-link mode `publish` fails outright when no Google publisher is
configured, even with zero files (`internal/mailgun/outbound.go`). This is a
`config/email.yaml` setting, made when the transformer is registered after this
change is deployed.

## Process

- Build it in a separate `git worktree`. The main working copy is the live
  deployment: the scheduled task runs `bin\filemill.exe` from it, so a rebuild
  there after a branch switch would deploy unfinished code.
- After deploying, send a real schedule PDF and look at the reply in Gmail on a
  phone and on desktop, in light and dark mode. How mail clients render the
  HTML is the main risk; the FileMill code is not.

## Out of scope

- HTML for failure replies and operator alerts.
- Sanitizing transformer HTML. The transformer is trusted.
- A `contract_version` bump. The field is optional and additive.
