[CmdletBinding()]
param(
    # Where to write the binary. Defaults to bin\filemill.exe. Deploy-FileMill.ps1
    # passes a staging name instead, because Windows will not let a running
    # executable be overwritten.
    [string]$Output
)

$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent $PSScriptRoot
if (-not $Output) {
    $Output = 'bin\filemill.exe'
}

Push-Location -LiteralPath $repositoryRoot
try {
    $describe = git describe --tags --dirty --always 2>$null
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($describe)) {
        Write-Warning 'git describe failed (no commits or tags reachable?) - building without a stamped version'
        go build -o $Output .\cmd\filemill
    } else {
        Write-Host "Building filemill $describe -> $Output"
        go build -ldflags "-X main.version=$describe" -o $Output .\cmd\filemill
    }
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed with exit code $LASTEXITCODE"
    }
} finally {
    Pop-Location
}
