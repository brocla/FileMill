[CmdletBinding()]
param(
    [switch]$StartNow
)

$ErrorActionPreference = 'Stop'

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
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $user
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -MultipleInstances IgnoreNew -ExecutionTimeLimit (New-TimeSpan -Seconds 0) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)

Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Description 'Supervises the FileMill worker (auto-restart on crash) at user logon.' -Force | Out-Null
Write-Host "Installed scheduled task '$taskName' for $user."

if ($StartNow) {
    Start-ScheduledTask -TaskName $taskName
    Write-Host 'Started the FileMill worker task.'
}
