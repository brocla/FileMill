[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent $PSScriptRoot

Push-Location -LiteralPath $repositoryRoot
try {
    $describe = git describe --tags --dirty --always 2>$null
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($describe)) {
        Write-Warning 'git describe failed (no commits or tags reachable?) - building without a stamped version'
        go build -o bin\filemill.exe .\cmd\filemill
    } else {
        Write-Host "Building filemill $describe"
        go build -ldflags "-X main.version=$describe" -o bin\filemill.exe .\cmd\filemill
    }
} finally {
    Pop-Location
}
