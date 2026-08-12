[CmdletBinding()]
param(
    [switch]$StartNow
)

$ErrorActionPreference = 'Stop'

# Registering an S4U principal (see below) is a privileged operation: without
# elevation Task Scheduler answers a bare "Access is denied". Say so plainly
# rather than letting that surface as the failure.
$identity = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $identity.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this from an elevated PowerShell (Run as administrator). Registering a task that starts at boot requires it.'
}

$taskName = 'FileMill Worker'
$repositoryRoot = Split-Path -Parent $PSScriptRoot
$launcher = Join-Path $PSScriptRoot 'Supervise-FileMill.ps1'
$powershell = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
$user = "$env:USERDOMAIN\$env:USERNAME"

if (-not (Test-Path -LiteralPath $launcher -PathType Leaf)) {
    throw "Launcher script not found: $launcher"
}

# The task launches the supervisor, which runs `filemill run` and restarts it on
# crash. -WindowStyle Hidden keeps it off-screen; RestartCount/-Interval is a
# backstop in case the supervisor process itself dies.
$action = New-ScheduledTaskAction -Execute $powershell -Argument "-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$launcher`""

# Two triggers. At boot is the one that matters: a machine that reboots
# overnight for updates used to sit idle until someone signed in, with the
# Cloudflare tunnel (a real service) up and nothing listening behind it. The
# logon trigger stays as a backstop — MultipleInstances IgnoreNew makes a second
# fire while the task is already running a no-op.
#
# The boot trigger waits 30 seconds. Nothing is waiting on FileMill at second
# zero, and starting into the busiest moment of boot has no upside.
$atStartup = New-ScheduledTaskTrigger -AtStartup
$atStartup.Delay = 'PT30S'
$atLogon = New-ScheduledTaskTrigger -AtLogOn -User $user

# S4U ("service for user") runs the task as this account with nobody logged on,
# and without storing a password. It is what makes the boot trigger useful, and
# it is also why this script needs elevation.
#
# It costs the task a network identity — an S4U process cannot reach SMB shares
# as the user — which FileMill does not need: it makes outbound HTTPS calls
# authenticated by API keys and listens on a local port. Its secrets must be
# machine-scope environment variables, though, because a task running before
# logon has no user registry hive to read user-scope ones from.
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType S4U -RunLevel Limited

# AllowStartIfOnBatteries/DontStopIfGoingOnBatteries override defaults that are
# actively wrong for a laptop: out of the box Task Scheduler refuses to start
# the task on battery and stops it when the machine is unplugged.
$settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -MultipleInstances IgnoreNew -ExecutionTimeLimit (New-TimeSpan -Seconds 0) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries

Register-ScheduledTask -TaskName $taskName -Action $action -Trigger @($atStartup, $atLogon) -Principal $principal -Settings $settings -Description 'Supervises the FileMill worker (auto-restart on crash). Starts at boot, before any user signs in.' -Force | Out-Null
Write-Host "Installed scheduled task '$taskName' for $user (starts at boot and at logon)."

if ($StartNow) {
    Start-ScheduledTask -TaskName $taskName
    Write-Host 'Started the FileMill worker task.'
}
