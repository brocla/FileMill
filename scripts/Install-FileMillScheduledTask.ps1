[CmdletBinding()]
param(
    [switch]$StartNow
)

$ErrorActionPreference = 'Stop'

$taskName = 'FileMill Worker'
$repositoryRoot = Split-Path -Parent $PSScriptRoot
$launcher = Join-Path $PSScriptRoot 'Start-FileMill.ps1'
$powershell = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
$user = "$env:USERDOMAIN\$env:USERNAME"

if (-not (Test-Path -LiteralPath $launcher -PathType Leaf)) {
    throw "Launcher script not found: $launcher"
}

$action = New-ScheduledTaskAction -Execute $powershell -Argument "-NoProfile -ExecutionPolicy Bypass -File `"$launcher`""
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $user
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -MultipleInstances IgnoreNew -ExecutionTimeLimit (New-TimeSpan -Seconds 0)

Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Description 'Starts the FileMill local job worker at user logon.' -Force | Out-Null
Write-Host "Installed scheduled task '$taskName' for $user."

if ($StartNow) {
    Start-ScheduledTask -TaskName $taskName
    Write-Host 'Started the FileMill worker task.'
}
