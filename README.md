<p align="center">
  <img src="logo2.png" alt="FileMill" width="420">
</p>

# FileMill

Local-first file transformation harness. Transformers are independent programs registered in `config/transformers.yaml`.

## First proof

Build the harness and reference transformer:

```powershell
go build -o bin/filemill.exe ./cmd/filemill
go build -o bin/copy-rename.exe ./examples/transformers/copy-rename
```

Submit a text file, then process one queued job:

```powershell
.\bin\filemill.exe submit copy_rename .\example.txt
.\bin\filemill.exe run --once
.\bin\filemill.exe jobs get <job-id>
```

`filemill run` runs the single-worker queue continuously. `data/` holds the SQLite database, job workspaces, and logs; it is intentionally excluded from Git.

## Workerlist

The `workerlist` transformer is registered through the Windows Python launcher. Submit a PDF and process one queued job:

```powershell
.\bin\filemill.exe submit workerlist C:\Users\micro\Downloads\schedule.pdf
.\bin\filemill.exe run --once
```

The resulting spreadsheet is retained in `data\jobs\<job-id>\output\schedule.xlsx`.

## Email delivery modes

A reply normally carries the output file as an attachment. An address can
instead be set to **`sheets-link`**, which uploads the output to Google Drive,
converts it to a native Google Sheet shared with anyone holding the link, and
replies with the link. Set it per address in `config\email.yaml`:

```yaml
delivery:
  iwk@mill.keywind.cc: sheets-link
```

Return mode is a property of the *address*, not the transformer — `iwk@` and
`workerlist@` can share one operation and reply differently. A `sheets-link`
address should be routed to an operation whose `layout` is `sheets`; a mismatch
logs one warning at startup rather than failing, since Drive converts either
layout.

**One email, one attachment.** A message carrying more than one attachment the
transformer can process is dropped with a log line and no reply — one email
produces one reply and one upload, so several attachments have no unambiguous
answer. Attachments the transformer does not accept (an inline signature logo)
are ignored rather than counted.

### Google setup for `sheets-link`

1. **Enable the Drive API** for the Cloud project that owns the OAuth client
   (console → APIs & Services → Library → Google Drive API → Enable). OAuth
   credentials alone are not enough: without this, uploads fail with
   `SERVICE_DISABLED` even though the token refreshes correctly.
2. Set these in the environment, alongside the `MAILGUN_*` variables — never in
   `email.yaml`:
   - `GOOGLE_OAUTH_CLIENT_ID`
   - `GOOGLE_OAUTH_CLIENT_SECRET`
   - `GOOGLE_OAUTH_REFRESH_TOKEN`

The scope is `drive.file`, so FileMill can only see files it created itself.
The worker **refuses to start** if an address asks for `sheets-link` and any of
these is missing — it will not quietly fall back to attaching the file. A
running worker must be restarted to see newly-set variables.

Published files are deleted from Drive after 30 days by a sweep that runs a
minute after startup and daily thereafter.

To check the Google path end to end against real Drive (creates one file and
deletes it):

```powershell
$env:FILEMILL_GOOGLE_E2E = '1'; go test ./internal/gsheets -run Live -v
```

## Run the worker in the background

FileMill should run under your Windows account so it can use locally installed transformers and their dependencies. Start it immediately in the background with:

```powershell
.\scripts\Start-FileMill.ps1
```

To start it automatically, run this once from an **elevated** PowerShell after building `bin\filemill.exe`:

```powershell
.\scripts\Install-FileMillScheduledTask.ps1 -StartNow
```

Elevation is required because the task runs with nobody logged on (see below); the script checks and tells you if you forgot.

The task is named `FileMill Worker` and starts **at boot, before anyone signs in**, and again at logon as a backstop. It launches a **supervisor** (`Supervise-FileMill.ps1`) that runs `filemill run` and **restarts it automatically if it crashes** — an immediate first retry, then escalating backoff (5s, 15s, 30s, 60s, 120s) for repeated rapid failures; a persistent crash-loop is logged (alerting is tracked in issue #7). A clean exit (Ctrl+C / shutdown) stops the supervisor. The worker logs to `data\logs\filemill.log`; supervisor events go to `data\logs\supervisor.log`.

Two consequences of running before logon are worth knowing:

- **Secrets must be machine-scope environment variables.** A task that runs before anyone signs in has no user registry hive, so *user*-scope variables are invisible to it — and a `sheets-link` route with missing Google credentials refuses to start. Set `MAILGUN_*` and `GOOGLE_OAUTH_*` at machine scope (elevated: `[Environment]::SetEnvironmentVariable('NAME','value','Machine')`).
- **No network identity.** The task cannot reach SMB shares as you. FileMill does not need that — it makes outbound HTTPS calls authenticated by API keys and listens on a local port — but a transformer that reads from a mapped drive would fail here.

The task also overrides two Task Scheduler defaults that are wrong for a laptop: without them Windows refuses to start the task on battery and stops it the moment you unplug.

To remove the automatic start later (this also stops the running supervisor and worker):

```powershell
.\scripts\Uninstall-FileMillScheduledTask.ps1
```

### Restart the worker (to pick up config changes)

`config\email.yaml` and `config\transformers.yaml` are read once at startup, so restart the worker after editing them.

**Under the supervisor** (started via the scheduled task above), just stop the worker — the supervisor relaunches it immediately with the new config. This needs an **elevated** PowerShell, because the worker runs in session 0 under the task's S4U principal and an unelevated `Stop-Process` on it fails with *Access is denied*:

```powershell
# Elevated.
Get-Process filemill | Stop-Process -Force
```

Then check the logs: `data\logs\supervisor.log` should show `restarting immediately`, and `data\logs\filemill.log` should show the `FileMill … — webhook listening on :8080` startup line. Do **not** stop the scheduled task or the supervisor to reload config — a clean stop tells the supervisor you're done; killing just the `filemill` process is what triggers a reload-and-restart.

**Running by hand** (a foreground `.\bin\filemill.exe run` window): press `Ctrl+C`, then run `.\bin\filemill.exe run` again.

To restart the whole chain (supervisor + worker) cleanly instead:

```powershell
Stop-ScheduledTask -TaskName 'FileMill Worker'; Start-ScheduledTask -TaskName 'FileMill Worker'
```

### Deploy a code change (rebuild the binary)

Editing the YAML above only needs a config reload because the *same* binary re-reads the files at startup. Changing Go code is different: the new behavior lives in a rebuilt `bin\filemill.exe`, and that makes the config-reload trick the **wrong** tool. Windows won't let you overwrite the executable while the worker holds it open, and if you kill just the worker the supervisor immediately relaunches the **old** binary before you can rebuild. So stop the whole chain, rebuild, then start it again:

Run the whole sequence from an **elevated** PowerShell — the worker and supervisor both run in session 0, where an unelevated `Stop-Process` fails with *Access is denied*:

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

**Order matters: supervisors first, then the worker.** Killing the worker while any supervisor is alive is the config-reload recipe above — the supervisor immediately relaunches it, reclaiming the port and the file lock you were trying to free.

`Stop-ScheduledTask` alone is not enough for either half. It leaves the `filemill` child running and orphaned (the task reports `Ready` while the worker is still up, holding `bin\filemill.exe`), and if the task was re-registered since the running instance started, it will not stop that instance's supervisor either — which is why the sequence hunts down supervisor processes explicitly rather than trusting the task to have stopped them.

Verify with `Get-Process filemill` returning nothing before you build. If the build still fails with a file-lock error, something survived.

Then `go build` overwrites the binary and `Start-ScheduledTask` launches a fresh supervised chain on the new code. Confirm it came up by checking `data\logs\filemill.log` for a new `FileMill … — webhook listening on :8080` line, and that `data\logs\supervisor.log` shows a matching `supervisor starting`. Because a fresh start also re-reads the YAML, this one sequence covers any change that touches code, with or without config.

## Transformer contract

FileMill runs the configured `command` in a job workspace and appends `job.json`. A transformer reads `job.json`, treats `input/` as read-only, writes artifacts only in `output/`, then writes `result.json` in the workspace. Both files carry `contract_version: "1"`.
