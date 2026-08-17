<p align="center">
  <img src="logo2.png" alt="FileMill" width="420">
</p>

<p align="center">
  <a href="https://github.com/brocla/FileMill/releases"><img src="https://img.shields.io/github/v/tag/brocla/FileMill?label=release" alt="Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/brocla/FileMill" alt="License"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/brocla/FileMill" alt="Go version">
  <img src="https://img.shields.io/badge/platform-Windows-0078D6?logo=windows" alt="Platform: Windows">
</p>

# FileMill

FileMill turns a single Windows machine into a small file-processing service
that people use entirely by email. An operator registers a handful of
**transformers** — independent programs, each good at one job, like
converting a PDF to a spreadsheet or normalizing a text file — and maps each
one to an email address. Anyone who emails an attachment to that address gets
the transformed result back as a threaded reply a couple of minutes later,
either as a file attachment or, for spreadsheet output, a shareable Google
Sheets link. There's nothing to install on the sender's side and no account
to create — the entire interface is "send an email, get an email back."

FileMill itself is a single Go binary that runs continuously in the
background. It receives inbound mail through a webhook rather than polling a
mailbox, so no IMAP client and no port-forwarding are required — the included
setup uses Mailgun to parse inbound mail and a Cloudflare Tunnel to expose the
webhook, which works from an ordinary laptop with no static IP. Each inbound
email becomes a job: FileMill runs the matching transformer as a subprocess in
an isolated workspace, then sends the result back over the same channel.

## Setup

### Prerequisites

- **Go 1.26+** to build FileMill and its example transformers.
- **Windows 10/11** — the background-worker tooling (scheduled task,
  supervised restarts, log layout) is Windows-specific. The core Go code
  itself is portable.
- **PowerShell** for the setup and deploy scripts below.
- Optional, only if you want email delivery: a **Mailgun** account and a
  domain you control, and something to expose a local port publicly (this
  project's own deployment uses a **Cloudflare Tunnel** — see
  [EMAIL-PIPELINE.md](EMAIL-PIPELINE.md) for the full infrastructure
  walkthrough).

### Build FileMill

```powershell
git clone https://github.com/brocla/FileMill.git
cd FileMill
go build -o bin/filemill.exe ./cmd/filemill
go build -o bin/copy-rename.exe ./examples/transformers/copy-rename
go build -o bin/uppercase.exe ./examples/transformers/uppercase
```

`copy-rename` and `uppercase` are two minimal example transformers included
in the repo — see [Writing a transformer](#writing-a-transformer) below.

### Register your transformers

Transformers are registered in `config/transformers.yaml`. That file is
gitignored — it typically names machine-specific paths, so it's never
committed — start from the template:

```powershell
copy config\transformers.yaml.example config\transformers.yaml
```

The template already registers the two transformers built above
(`copy_rename`, `uppercase`) plus a `pdf_report` entry that's a placeholder
showing how to point at an external interpreter and script instead of a Go
binary. Edit the copy to match whatever transformers you actually have.

### Run your first job

```powershell
.\bin\filemill.exe submit uppercase .\example.txt
.\bin\filemill.exe run --once
.\bin\filemill.exe jobs get <job-id>
```

The result lands in `data\jobs\<job-id>\output\`. `filemill run` without
`--once` runs the single-worker queue continuously instead of processing one
job and exiting. `data/` holds the SQLite database, job workspaces, and logs;
it's intentionally excluded from Git.

### Enable email delivery (optional)

`config/email.yaml` is gitignored for the same reason as the transformers
file — start from its template too:

```powershell
copy config\email.yaml.example config\email.yaml
```

Edit it to map your own addresses to operations, then set these as
environment variables (never in the YAML — see
[Email delivery modes](#email-delivery-modes) for why):

- `MAILGUN_API_KEY`, `MAILGUN_WEBHOOK_SIGNING_KEY`, `MAILGUN_DOMAIN`,
  `REPLY_FROM` — all four are required together, or leave all four unset to
  run FileMill without the email adapter at all.
- `LISTEN_ADDR` — optional, defaults to `:8080`.

Getting a real inbound webhook requires Mailgun route configuration, DNS
records, and (for a laptop with no public IP) a tunnel — that infrastructure
setup is documented separately in [EMAIL-PIPELINE.md](EMAIL-PIPELINE.md) and
diagrammed in [email-pipeline-diagram.html](email-pipeline-diagram.html).

### Run continuously, in the background

FileMill should run under your Windows account so it can use locally
installed transformers and their dependencies. Start it immediately in the
background with:

```powershell
.\scripts\Start-FileMill.ps1
```

To start it automatically, run this once from an **elevated** PowerShell
after building `bin\filemill.exe`:

```powershell
.\scripts\Install-FileMillScheduledTask.ps1 -StartNow
```

Elevation is required because the task runs with nobody logged on (see
below); the script checks and tells you if you forgot.

The task is named `FileMill Worker` and starts **at boot, before anyone signs
in**, and again at logon as a backstop. It launches a **supervisor**
(`Supervise-FileMill.ps1`) that runs `filemill run` and **restarts it
automatically if it crashes** — an immediate first retry, then escalating
backoff (5s, 15s, 30s, 60s, 120s) for repeated rapid failures; a persistent
crash-loop is logged (alerting is tracked in issue #7). A clean exit (Ctrl+C
/ shutdown) stops the supervisor.

Two consequences of running before logon are worth knowing:

- **Secrets must be machine-scope environment variables.** A task that runs
  before anyone signs in has no user registry hive, so *user*-scope
  variables are invisible to it — and a `sheets-link` route with missing
  Google credentials refuses to start. Set `MAILGUN_*` and `GOOGLE_OAUTH_*`
  at machine scope (elevated:
  `[Environment]::SetEnvironmentVariable('NAME','value','Machine')`).
- **No network identity.** The task cannot reach SMB shares as you. FileMill
  does not need that — it makes outbound HTTPS calls authenticated by API
  keys and listens on a local port — but a transformer that reads from a
  mapped drive would fail here.

The task also overrides two Task Scheduler defaults that are wrong for a
laptop: without them Windows refuses to start the task on battery and stops
it the moment you unplug.

To remove the automatic start later (this also stops the running supervisor
and worker):

```powershell
.\scripts\Uninstall-FileMillScheduledTask.ps1
```

## Logs

The worker logs to `data\logs\filemill.log`. When it's running under the
supervisor (the scheduled task above), supervisor events — restarts,
backoff, crash loops — go to `data\logs\supervisor.log` instead. A fresh,
healthy start shows a `FileMill … — webhook listening on :8080` line in
`filemill.log` and a matching `supervisor starting` in `supervisor.log`.

## Verify the pipeline end to end

Once the worker is running and an address in `config/email.yaml` is live,
the real test is the real thing: email an attachment to that address and
watch `data\logs\filemill.log` for a `webhook received` line, followed by
the reply landing in your inbox, threaded as a reply — typically within a
couple of minutes. Mailgun's route processing has been observed to lag up to
~10 minutes under load, so give it a few minutes before assuming failure.

If you're using `sheets-link` delivery, the Google Drive half can be
verified in isolation, without waiting on email at all — it publishes one
small test file and deletes it again:

```powershell
$env:FILEMILL_GOOGLE_E2E = '1'; go test ./internal/gsheets -run Live -v
```

## Restarting after a change

### Config changes only

`config\email.yaml` and `config\transformers.yaml` are read once at startup,
so restart the worker after editing them.

**Under the supervisor** (started via the scheduled task above), just stop
the worker — the supervisor relaunches it immediately with the new config.
This needs an **elevated** PowerShell, because the worker runs in session 0
under the task's S4U principal and an unelevated `Stop-Process` on it fails
with *Access is denied*:

```powershell
# Elevated.
Get-Process filemill | Stop-Process -Force
```

Then check the logs: `data\logs\supervisor.log` should show `restarting
immediately`, and `data\logs\filemill.log` should show the startup line
again. Do **not** stop the scheduled task or the supervisor to reload
config — a clean stop tells the supervisor you're done; killing just the
`filemill` process is what triggers a reload-and-restart.

**Running by hand** (a foreground `.\bin\filemill.exe run` window): press
`Ctrl+C`, then run `.\bin\filemill.exe run` again.

To restart the whole chain (supervisor + worker) cleanly instead:

```powershell
Stop-ScheduledTask -TaskName 'FileMill Worker'; Start-ScheduledTask -TaskName 'FileMill Worker'
```

### After a code change (rebuild the binary)

Editing the YAML above only needs a config reload because the *same* binary
re-reads the files at startup. Changing Go code is different: the new
behavior lives in a rebuilt `bin\filemill.exe`, and that makes the
config-reload trick the **wrong** tool. Windows won't let you overwrite the
executable while the worker holds it open, and if you kill just the worker
the supervisor immediately relaunches the **old** binary before you can
rebuild. So stop the whole chain, rebuild, then start it again.

Run the whole sequence from an **elevated** PowerShell — the worker and
supervisor both run in session 0, where an unelevated `Stop-Process` fails
with *Access is denied*:

```powershell
# Elevated.
Stop-ScheduledTask -TaskName 'FileMill Worker'
Get-CimInstance Win32_Process -Filter "Name='powershell.exe'" |
  Where-Object { $_.CommandLine -match 'Supervise-FileMill' } |
  ForEach-Object { Stop-Process -Id $_.ProcessId -Force }
Get-Process filemill -ErrorAction SilentlyContinue | Stop-Process -Force
go build -o bin\filemill.exe ./cmd/filemill
Start-ScheduledTask -TaskName 'FileMill Worker'
```

**Order matters: supervisors first, then the worker.** Killing the worker
while any supervisor is alive is the config-reload recipe above — the
supervisor immediately relaunches it, reclaiming the port and the file lock
you were trying to free.

`Stop-ScheduledTask` alone is not enough for either half. It leaves the
`filemill` child running and orphaned (the task reports `Ready` while the
worker is still up, holding `bin\filemill.exe`), and if the task was
re-registered since the running instance started, it will not stop that
instance's supervisor either — which is why the sequence hunts down
supervisor processes explicitly rather than trusting the task to have
stopped them.

Verify with `Get-Process filemill` returning nothing before you build. If
the build still fails with a file-lock error, something survived.

If the new code carries a schema change, back the database up while
everything is stopped — after the worker exits and before
`Start-ScheduledTask`, since a copy taken with a writer running can be torn.
Copy all three files; the `-wal` holds committed data the `.db` does not
yet:

```powershell
Copy-Item data\filemill.db, data\filemill.db-wal, data\filemill.db-shm data\backup\
```

Migrations run automatically on the first `Open` of the new binary and are
one-way, so that copy is the rollback path — and rolling back means
restoring **both** the old binary and the backed-up database. An old binary
against a migrated database is not a rollback: it runs constraints the
schema no longer enforces, which is how duplicate rows get written rather
than rejected.

Then `go build` overwrites the binary and `Start-ScheduledTask` launches a
fresh supervised chain on the new code. Confirm it came up the same way
described in [Logs](#logs) above. Because a fresh start also re-reads the
YAML, this one sequence covers any change that touches code, with or without
config.

## Email delivery modes

A reply normally carries the output file as an attachment. An address can
instead be set to **`sheets-link`**, which uploads the output to Google
Drive, converts it to a native Google Sheet shared with anyone holding the
link, and replies with the link. Set it per address in `config\email.yaml`:

```yaml
delivery:
  report@mill.example.com: sheets-link
```

Return mode is a property of the *address*, not the transformer — two
addresses can share one operation and reply differently. A `sheets-link`
address should be routed to an operation whose `layout` option is `sheets`;
a mismatch logs one warning at startup rather than failing, since Drive
converts either layout.

**One email, one attachment.** A message carrying more than one attachment
the transformer can process is dropped with a log line and no reply — one
email produces one reply and one upload, so several attachments have no
unambiguous answer. Attachments the transformer does not accept (an inline
signature logo, say) are ignored rather than counted.

### Google setup for `sheets-link`

1. **Enable the Drive API** for the Cloud project that owns the OAuth client
   (console → APIs & Services → Library → Google Drive API → Enable). OAuth
   credentials alone are not enough: without this, uploads fail with
   `SERVICE_DISABLED` even though the token refreshes correctly.
2. Set these in the environment, alongside the `MAILGUN_*` variables — never
   in `email.yaml`:
   - `GOOGLE_OAUTH_CLIENT_ID`
   - `GOOGLE_OAUTH_CLIENT_SECRET`
   - `GOOGLE_OAUTH_REFRESH_TOKEN`

The scope is `drive.file`, so FileMill can only see files it created itself.
The worker **refuses to start** if an address asks for `sheets-link` and any
of these is missing — it will not quietly fall back to attaching the file. A
running worker must be restarted to see newly-set variables.

Published files are deleted from Drive after 30 days by a sweep that runs a
minute after startup and daily thereafter.

## Writing a transformer

FileMill runs the configured `command` as a subprocess in a per-job
workspace and appends `job.json` to the argument list. A transformer reads
`job.json`, treats `input/` as read-only, writes artifacts only in
`output/`, then writes `result.json` in the workspace. Both files carry
`contract_version: "1"`. `examples/transformers/copy-rename` and
`examples/transformers/uppercase` are complete, minimal reference
implementations — start from whichever one is closer to what you're
building: `copy-rename` shows the shape of the contract with almost no
logic, `uppercase` adds the smallest possible real transformation on top.

### The job.json contract

Running `.\bin\filemill.exe submit uppercase .\example.txt` (see
[Run your first job](#run-your-first-job)) writes this into the job's
workspace before the transformer is even started:

```json
{
  "contract_version": "1",
  "job_id": "ac542a3a-3fd2-4cd9-b564-99629a113f27",
  "operation": "uppercase",
  "input_files": [
    {
      "path": "input/example.txt",
      "name": "example.txt"
    }
  ],
  "output_directory": "output",
  "options": {}
}
```

- **`contract_version`** — the version of this JSON shape, not of FileMill
  itself. A transformer should refuse to run against a version it doesn't
  recognize rather than guess.
- **`job_id`** — the workspace's directory name
  (`data\jobs\<job_id>\`), handy for log correlation; the transformer
  itself has no need to parse it.
- **`operation`** — the operation name from `transformers.yaml` that
  selected this transformer. Only useful if one binary is registered under
  more than one operation and branches on it.
- **`input_files`** — every input file for the job, as paths *relative to
  the workspace* (the transformer is launched with the workspace as its
  working directory, so `input/example.txt` opens directly). `name` is the
  original filename, which matters when `path` has been sanitized or
  de-duplicated but the transformer wants to preserve the sender's name in
  its output.
- **`output_directory`** — always `"output"` today; a transformer writes
  every artifact it produces there rather than assuming a filename FileMill
  hasn't told it.
- **`options`** — the transformer's own `options:` block from
  `transformers.yaml`, passed through verbatim so one binary can behave
  differently per registration. `uppercase` and `copy_rename` don't declare
  any, hence `{}`; the `pdf_report` template in
  `config/transformers.yaml.example` declares `layout: sheets`, so its
  `job.json` would carry `"options": {"layout": "sheets"}` instead — that's
  also the value [Email delivery modes](#email-delivery-modes) checks
  before allowing `sheets-link` on an address.

When the transformer exits, FileMill looks for `result.json` in the same
workspace — `examples/transformers/uppercase/main.go` shows the matching
write side of that contract.
