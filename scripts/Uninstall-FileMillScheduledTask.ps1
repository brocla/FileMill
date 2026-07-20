[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

Unregister-ScheduledTask -TaskName 'FileMill Worker' -Confirm:$false -ErrorAction Stop
Write-Host "Removed scheduled task 'FileMill Worker'."

# Stop the supervisor first so it doesn't relaunch the worker, then the worker.
Get-CimInstance Win32_Process -Filter "Name = 'powershell.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -match '(?i)Supervise-FileMill\.ps1' } |
    ForEach-Object {
        Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
        Write-Host "Stopped supervisor (PID $($_.ProcessId))."
    }

Get-Process filemill -ErrorAction SilentlyContinue | ForEach-Object {
    Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue
    Write-Host "Stopped worker (PID $($_.Id))."
}
