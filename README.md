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

The task is named `FileMill Worker`, runs only while you are signed in, and starts the same background worker. FileMill logs to `data\logs\filemill.log`. To remove the automatic start later:

```powershell
.\scripts\Uninstall-FileMillScheduledTask.ps1
```

## Transformer contract

FileMill runs the configured `command` in a job workspace and appends `job.json`. A transformer reads `job.json`, treats `input/` as read-only, writes artifacts only in `output/`, then writes `result.json` in the workspace. Both files carry `contract_version: "1"`.
