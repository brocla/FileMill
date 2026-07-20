<#
.SYNOPSIS
    Supervises the FileMill worker: runs `filemill run` and restarts it if it
    exits unexpectedly. Intended to be launched by the logon scheduled task
    (see Install-FileMillScheduledTask.ps1).

.DESCRIPTION
    Restart policy: an immediate first retry after a crash, then escalating
    backoff (5s, 15s, 30s, 60s, 120s) for repeated rapid failures. A run that
    lasts long enough to be "healthy" resets the backoff. A clean exit (code 0,
    i.e. an intentional Ctrl+C / OS shutdown signal) stops the supervisor.

    A crash-loop is logged only, for now; alerting is tracked in issue #7.
    Supervisor events are written to data\logs\supervisor.log; the worker's own
    output continues to go to data\logs\filemill.log.
#>
[CmdletBinding()]
param(
    # Overridable for testing; defaults to the built worker.
    [string]$Executable
)

$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent $PSScriptRoot
if (-not $Executable) {
    $Executable = Join-Path $repositoryRoot 'bin\filemill.exe'
}
$logDir = Join-Path $repositoryRoot 'data\logs'
$supervisorLog = Join-Path $logDir 'supervisor.log'

if (-not (Test-Path -LiteralPath $Executable -PathType Leaf)) {
    throw "FileMill executable not found at $Executable. Build it with: go build -o bin/filemill.exe ./cmd/filemill"
}
New-Item -ItemType Directory -Force -Path $logDir | Out-Null
Set-Location -LiteralPath $repositoryRoot

function Write-SupervisorLog {
    param([string]$Message)
    $timestamp = (Get-Date).ToUniversalTime().ToString('yyyy/MM/dd HH:mm:ss')
    $line = "$timestamp supervisor $Message"
    Add-Content -LiteralPath $supervisorLog -Value $line
    Write-Host $line
}

# Restart policy.
$backoffSeconds    = @(0, 5, 15, 30, 60, 120) # immediate first retry, then escalate (capped)
$healthyRunSeconds = 60                        # a run at least this long resets the backoff
$crashLoopAt       = 4                         # rapid restarts before logging a crash-loop warning

$rapidFailures = 0
Write-SupervisorLog "starting; watching $Executable"

while ($true) {
    $startedAt = Get-Date
    Write-SupervisorLog 'launching: filemill run'
    & $Executable run
    $exitCode = $LASTEXITCODE
    $ranSeconds = [int]((Get-Date) - $startedAt).TotalSeconds
    Write-SupervisorLog "filemill exited code=$exitCode after ${ranSeconds}s"

    if ($exitCode -eq 0) {
        # `filemill run` returns 0 only on an intentional signal (Ctrl+C / OS
        # shutdown); don't fight it.
        Write-SupervisorLog 'clean exit (intentional stop); supervisor stopping'
        break
    }

    if ($ranSeconds -ge $healthyRunSeconds) {
        $rapidFailures = 0
        $delay = 0
    } else {
        $delay = $backoffSeconds[[Math]::Min($rapidFailures, $backoffSeconds.Count - 1)]
        $rapidFailures++
        if ($rapidFailures -ge $crashLoopAt) {
            Write-SupervisorLog "WARNING crash-loop: $rapidFailures rapid restarts (log-only; alerting tracked in issue #7)"
        }
    }

    if ($delay -gt 0) {
        Write-SupervisorLog "restarting in ${delay}s"
        Start-Sleep -Seconds $delay
    } else {
        Write-SupervisorLog 'restarting immediately'
    }
}
