<#
.SYNOPSIS
    Deploys a fresh build of the FileMill worker and verifies it is serving,
    rolling back if it is not.

.DESCRIPTION
    The supervisor is left alone. Windows will not let a running executable be
    overwritten, but it will let one be renamed, so a deploy is:

        rename bin\filemill.exe -> bin\filemill.<old version>.exe  (the rollback copy)
        move   the new build    -> bin\filemill.exe
        kill   the worker, and the supervisor relaunches it into the new binary

    That is one worker restart of downtime, no Task Scheduler interaction (and
    so no way to trip over MultipleInstances IgnoreNew, which makes
    Start-ScheduledTask a silent no-op while a task is already running), and the
    previous binary is left beside the new one.

    Nothing is assumed from an exit code alone: the new binary must run and
    report its version, the old worker must be gone, a new one must appear, and
    the worker's own log must show the new version serving. If any of that
    fails, the previous binary goes back and the script exits non-zero.

    Requires elevation: the worker runs in session 0 (see
    Install-FileMillScheduledTask.ps1) and an unelevated Stop-Process on it
    fails with "Access is denied".

    Killing the worker interrupts a job it is running. The restarted worker
    marks that job interrupted, and the sender's reply asks them to send the
    file again.

.EXAMPLE
    .\scripts\Deploy-FileMill.ps1
    Builds from the current checkout, runs the tests, and deploys.

.EXAMPLE
    .\scripts\Deploy-FileMill.ps1 -NewBinary .\bin\filemill.new.exe -Yes
    Deploys a binary you built beforehand (handy to keep `go build` unelevated).
#>
[CmdletBinding()]
param(
    # The FileMill checkout to deploy. Defaults to this script's repository.
    [string]$RepositoryRoot,
    # Deploy this build instead of building one.
    [string]$NewBinary,
    [switch]$SkipBuild,
    [switch]$SkipTests,
    # Deploy even though the working tree has uncommitted changes.
    [switch]$AllowDirty,
    # Don't wait for the new version to appear in filemill.log. That line is
    # written by the Mailgun adapter, so a worker with no Mailgun configuration
    # never prints it.
    [switch]$SkipLogCheck,
    # Don't ask before restarting the worker.
    [switch]$Yes,
    [int]$TimeoutSeconds = 90,
    # Testing only: the harness drives this against a fake worker in its own
    # session, which needs no elevation.
    [switch]$NoElevationCheck
)

$ErrorActionPreference = 'Stop'

function Write-Step { param([string]$Message) Write-Host "==> $Message" }

function Fail {
    param([string]$Message)
    Write-Host "DEPLOY FAILED: $Message" -ForegroundColor Red
    exit 1
}

# Wait-Until polls Condition until it returns true or Seconds elapse. Every
# check in this script is a wait-for-a-fact, never a fixed sleep: on this
# machine a command returning cleanly has more than once meant nothing happened.
function Wait-Until {
    param([scriptblock]$Condition, [int]$Seconds)
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        if (& $Condition) { return $true }
        Start-Sleep -Milliseconds 500
    }
    return $false
}

# Get-Workers finds worker processes running one specific binary. Matching on
# the path, not just the name, keeps a second checkout's worker (or a test
# harness's fake) out of it.
function Get-Workers {
    param([string]$Path)
    # Not $matches: that is a PowerShell automatic variable, written by -match.
    $found = Get-CimInstance Win32_Process -Filter "Name='filemill.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.ExecutablePath -and ($_.ExecutablePath -ieq $Path) }
    return @($found)
}

# Get-Version runs a binary's --version. It is also the proof that the file is
# a working executable before anything is swapped.
function Get-Version {
    param([string]$Path)
    $output = & $Path --version 2>&1
    if ($LASTEXITCODE -ne 0) { return $null }
    $text = ($output | Select-Object -First 1)
    if ([string]::IsNullOrWhiteSpace($text)) { return $null }
    return ($text.Trim() -split '\s+')[-1]
}

# Read-LogSince reads what a log file has gained since an offset, sharing the
# file with the worker that is writing it.
function Read-LogSince {
    param([string]$Path, [long]$Offset)
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
    try {
        if ($Offset -gt $stream.Length) { $Offset = 0 }  # the log was rotated or truncated
        $stream.Seek($Offset, [System.IO.SeekOrigin]::Begin) | Out-Null
        $reader = New-Object System.IO.StreamReader($stream)
        return $reader.ReadToEnd()
    } finally {
        $stream.Dispose()
    }
}

function Get-LogLength {
    param([string]$Path)
    if (Test-Path -LiteralPath $Path) { return (Get-Item -LiteralPath $Path).Length }
    return 0
}

if (-not $NoElevationCheck) {
    $identity = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
    if (-not $identity.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        Fail 'Run this from an elevated PowerShell (Run as administrator). The worker runs in session 0, and stopping it needs elevation.'
    }
}

if (-not $RepositoryRoot) { $RepositoryRoot = Split-Path -Parent $PSScriptRoot }
$RepositoryRoot = (Resolve-Path -LiteralPath $RepositoryRoot).Path
$binary = Join-Path $RepositoryRoot 'bin\filemill.exe'
$logPath = Join-Path $RepositoryRoot 'data\logs\filemill.log'

# --- preflight: everything that can fail before anything is touched ---------

if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
    Fail "No worker binary at $binary. Build one first with .\scripts\Build-FileMill.ps1"
}
$oldVersion = Get-Version $binary
if (-not $oldVersion) { Fail "The current binary at $binary did not report a version." }

if ($NewBinary) {
    $SkipBuild = $true
} else {
    $NewBinary = Join-Path $RepositoryRoot 'bin\filemill.new.exe'
}

if (-not $SkipBuild) {
    Push-Location -LiteralPath $RepositoryRoot
    try {
        $dirty = git status --porcelain
        if ($LASTEXITCODE -ne 0) { Fail 'git status failed; is this a checkout?' }
        if ($dirty -and -not $AllowDirty) {
            Fail "The working tree has uncommitted changes, so the build would not match any commit. Commit them, or pass -AllowDirty.`n$($dirty -join "`n")"
        }
        if (-not $SkipTests) {
            Write-Step 'Running go test ./...'
            go test ./...
            if ($LASTEXITCODE -ne 0) { Fail 'Tests failed; nothing was deployed.' }
        }
        Write-Step 'Building'
        & (Join-Path $PSScriptRoot 'Build-FileMill.ps1') -Output $NewBinary
    } finally {
        Pop-Location
    }
}

if (-not (Test-Path -LiteralPath $NewBinary -PathType Leaf)) {
    Fail "No new build at $NewBinary."
}
$NewBinary = (Resolve-Path -LiteralPath $NewBinary).Path
$newVersion = Get-Version $NewBinary
if (-not $newVersion) { Fail "The new build at $NewBinary did not report a version, so it is not a working executable." }

$workers = Get-Workers $binary
Write-Host ''
Write-Host "  repository: $RepositoryRoot"
Write-Host "  running:    $oldVersion  (worker PID(s): $(if ($workers.Count) { ($workers | ForEach-Object { $_.ProcessId }) -join ', ' } else { 'none' }))"
Write-Host "  deploying:  $newVersion"
Write-Host ''

if (-not $Yes) {
    $answer = Read-Host 'Restart the worker into the new build? Any job it is running will be interrupted. [y/N]'
    if ($answer -notmatch '^(y|yes)$') { Write-Host 'Nothing was changed.'; exit 0 }
}

# --- swap: rename the running binary aside, move the new one in -------------

$backup = Join-Path $RepositoryRoot ('bin\filemill.{0}.exe' -f ($oldVersion -replace '[^\w\.\-]', '_'))
Write-Step "Keeping the current binary as $(Split-Path -Leaf $backup)"
Move-Item -LiteralPath $binary -Destination $backup -Force
try {
    Move-Item -LiteralPath $NewBinary -Destination $binary -Force
} catch {
    Move-Item -LiteralPath $backup -Destination $binary -Force
    Fail "Could not put the new build in place, so the old one stayed: $_"
}

# Restore-Previous puts the old binary back and restarts the worker into it.
# Called when the new build does not come up.
function Restore-Previous {
    param([string]$Reason)
    Write-Host "Rolling back: $Reason" -ForegroundColor Yellow
    # What the worker managed to say before it failed is the most useful thing
    # on the screen at this point.
    $tail = Read-LogSince $logPath $logOffset
    if ($tail) {
        Write-Host 'filemill.log since the restart:' -ForegroundColor Yellow
        $tail -split "`r?`n" | Where-Object { $_ } | Select-Object -Last 8 | ForEach-Object { Write-Host "    $_" }
    }
    $failed = Join-Path $RepositoryRoot 'bin\filemill.failed.exe'
    Move-Item -LiteralPath $binary -Destination $failed -Force
    Move-Item -LiteralPath $backup -Destination $binary -Force
    foreach ($worker in (Get-Workers $binary)) {
        Stop-Process -Id $worker.ProcessId -Force -ErrorAction SilentlyContinue
    }
    $back = Wait-Until -Seconds $TimeoutSeconds -Condition {
        $running = Get-Workers $binary
        ($running.Count -gt 0) -and ((Get-Version $binary) -eq $oldVersion)
    }
    if ($back) {
        Fail "$Reason. Rolled back to $oldVersion; the build that failed is at $failed."
    }
    Fail "$Reason. Rolled back to $oldVersion, but no worker came back — start it with: Start-ScheduledTask -TaskName 'FileMill Worker'. The build that failed is at $failed."
}

# --- restart: kill the worker and let the supervisor relaunch it ------------

if ($workers.Count -eq 0) {
    Write-Host ''
    Write-Host "Deployed $newVersion. No worker was running, so nothing was restarted." -ForegroundColor Green
    Write-Host "Start it with: Start-ScheduledTask -TaskName 'FileMill Worker'"
    exit 0
}

$logOffset = Get-LogLength $logPath
$oldPids = @($workers | ForEach-Object { $_.ProcessId })
Write-Step "Stopping the worker (PID $($oldPids -join ', ')); the supervisor will relaunch it"
foreach ($worker in $workers) {
    Stop-Process -Id $worker.ProcessId -Force
}
if (-not (Wait-Until -Seconds $TimeoutSeconds -Condition { (Get-Workers $binary | Where-Object { $oldPids -contains $_.ProcessId }).Count -eq 0 })) {
    Restore-Previous 'The old worker did not stop'
}

Write-Step 'Waiting for the supervisor to start the new worker'
if (-not (Wait-Until -Seconds $TimeoutSeconds -Condition { (Get-Workers $binary | Where-Object { $oldPids -notcontains $_.ProcessId }).Count -gt 0 })) {
    # Two causes look identical from here, and a build that crashes at startup
    # can exit before it is ever seen in the process list, so name both rather
    # than guessing: the log tail printed below usually settles it.
    Restore-Previous 'No new worker stayed up. Either the new build exits at startup, or no supervisor is running to relaunch it (Start-ScheduledTask -TaskName ''FileMill Worker'')'
}
$newWorkers = Get-Workers $binary | Where-Object { $oldPids -notcontains $_.ProcessId }

# --- verify: the new version is the one serving -----------------------------

if (-not $SkipLogCheck) {
    Write-Step "Waiting for $newVersion to appear in filemill.log"
    if (-not (Wait-Until -Seconds $TimeoutSeconds -Condition { (Read-LogSince $logPath $logOffset) -match [regex]::Escape($newVersion) })) {
        Restore-Previous "The new worker did not report $newVersion in $logPath within $TimeoutSeconds seconds"
    }
}

# A worker that started and then died leaves a PID that no longer exists, so
# this is checked after the log line, not instead of it.
if ((Get-Workers $binary).Count -eq 0) {
    Restore-Previous 'The new worker started and then exited'
}

Write-Host ''
Write-Host "Deployed $oldVersion -> $newVersion" -ForegroundColor Green
Write-Host "  worker PID(s): $(($newWorkers | ForEach-Object { $_.ProcessId }) -join ', ')"
Write-Host "  previous binary kept at: $backup"
$startup = (Read-LogSince $logPath $logOffset) -split "`r?`n" | Where-Object { $_ -match [regex]::Escape($newVersion) } | Select-Object -First 1
if ($startup) { Write-Host "  $($startup.Trim())" }
exit 0
