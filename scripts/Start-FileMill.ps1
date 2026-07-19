[CmdletBinding()]
param(
    [switch]$Foreground
)

$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$executable = Join-Path $repositoryRoot 'bin\filemill.exe'

if (-not (Test-Path -LiteralPath $executable -PathType Leaf)) {
    throw "FileMill executable not found at $executable. Build it with: go build -o bin/filemill.exe ./cmd/filemill"
}

if ($Foreground) {
    Set-Location -LiteralPath $repositoryRoot
    & $executable run
    exit $LASTEXITCODE
}

$alreadyRunning = Get-CimInstance Win32_Process -Filter "Name = 'filemill.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -match '(?i)filemill\.exe"?\s+run(\s|$)' }

if ($alreadyRunning) {
    Write-Host "FileMill worker is already running (PID $($alreadyRunning[0].ProcessId))."
    exit 0
}

$process = Start-Process -FilePath $executable -ArgumentList 'run' -WorkingDirectory $repositoryRoot -WindowStyle Hidden -PassThru
Write-Host "FileMill worker started in the background (PID $($process.Id))."
