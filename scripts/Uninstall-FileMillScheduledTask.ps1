[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

Unregister-ScheduledTask -TaskName 'FileMill Worker' -Confirm:$false -ErrorAction Stop
Write-Host "Removed scheduled task 'FileMill Worker'. Existing worker processes, if any, are not stopped."
