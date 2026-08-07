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

## Run the worker in the background

FileMill should run under your Windows account so it can use locally installed transformers and their dependencies. Start it immediately in the background with:

```powershell
.\scripts\Start-FileMill.ps1
```

To start it automatically whenever you sign in, run this once from your normal PowerShell session after building `bin\filemill.exe`:

```powershell
.\scripts\Install-FileMillScheduledTask.ps1 -StartNow
```

The task is named `FileMill Worker` and runs only while you are signed in. It launches a **supervisor** (`Supervise-FileMill.ps1`) that runs `filemill run` and **restarts it automatically if it crashes** — an immediate first retry, then escalating backoff (5s, 15s, 30s, 60s, 120s) for repeated rapid failures; a persistent crash-loop is logged (alerting is tracked in issue #7). A clean exit (Ctrl+C / shutdown) stops the supervisor. The worker logs to `data\logs\filemill.log`; supervisor events go to `data\logs\supervisor.log`. To remove the automatic start later (this also stops the running supervisor and worker):

```powershell
.\scripts\Uninstall-FileMillScheduledTask.ps1
```

### Restart the worker (to pick up config changes)

`config\email.yaml` and `config\transformers.yaml` are read once at startup, so restart the worker after editing them.

**Under the supervisor** (started via the scheduled task above), just stop the worker — the supervisor relaunches it immediately with the new config:

```powershell
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

```powershell
Stop-ScheduledTask -TaskName 'FileMill Worker'
go build -o bin\filemill.exe ./cmd/filemill
Start-ScheduledTask -TaskName 'FileMill Worker'
```

`Stop-ScheduledTask` ends the supervisor and worker cleanly and releases the exe lock; the build overwrites the binary; `Start-ScheduledTask` launches a fresh supervised chain on the new code. Confirm it came up by checking `data\logs\filemill.log` for a new `FileMill … — webhook listening on :8080` line. Because a fresh start also re-reads the YAML, this one sequence covers any change that touches code, with or without config.

If `go build` fails with a file-lock or permission error, the worker didn't actually stop and is still holding `bin\filemill.exe`. Force it down, then rebuild and start:

```powershell
Get-Process filemill | Stop-Process -Force
```

## Transformer contract

FileMill runs the configured `command` in a job workspace and appends `job.json`. A transformer reads `job.json`, treats `input/` as read-only, writes artifacts only in `output/`, then writes `result.json` in the workspace. Both files carry `contract_version: "1"`.
