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

## Transformer contract

FileMill runs the configured `command` in a job workspace and appends `job.json`. A transformer reads `job.json`, treats `input/` as read-only, writes artifacts only in `output/`, then writes `result.json` in the workspace. Both files carry `contract_version: "1"`.
