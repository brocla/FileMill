<#
.SYNOPSIS
    Shows the most recent inbound-mail log lines with their timestamps converted
    from UTC to Mountain time.

.DESCRIPTION
    Scans data\logs\filemill.log for lines containing "sender=", keeps the last
    N of them, and rewrites each UTC timestamp as Mountain time. Daylight saving
    is handled by the Windows "Mountain Standard Time" zone, which observes DST,
    so the offset is -6 in summer and -7 in winter automatically.

.PARAMETER Tail
    How many of the most recent matching lines to show. Defaults to 20.

.PARAMETER LogPath
    Log file to read. Defaults to data\logs\filemill.log in the repository.

.PARAMETER AsObject
    Emit a structured object per record (LocalTime, Sender, Recipient,
    Operation, Line) instead of a single line of text, for filtering or export.

.EXAMPLE
    .\scripts\Get-FileMillSenders.ps1

.EXAMPLE
    .\scripts\Get-FileMillSenders.ps1 -Tail 50

.EXAMPLE
    .\scripts\Get-FileMillSenders.ps1 -Tail 50 -AsObject |
        Where-Object Sender -like '*proton*'
#>
[CmdletBinding()]
param(
    [ValidateRange(1, 100000)]
    [int]$Tail = 20,

    [string]$LogPath,

    [switch]$AsObject
)

$ErrorActionPreference = 'Stop'

if (-not $LogPath) {
    $repositoryRoot = Split-Path -Parent $PSScriptRoot
    $LogPath = Join-Path $repositoryRoot 'data\logs\filemill.log'
}

if (-not (Test-Path -LiteralPath $LogPath -PathType Leaf)) {
    throw "Log file not found at $LogPath"
}

$mountainZone = [System.TimeZoneInfo]::FindSystemTimeZoneById('Mountain Standard Time')

# Example line:
#   mailgun 2026/09/10 05:55:14 webhook status=200 accepted: sender="a@b.com" recipient="c@d.cc" operation=workerlist
$timestampPattern = '(?<stamp>\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})'

$matchingLines = @(Select-String -LiteralPath $LogPath -SimpleMatch -Pattern 'sender=' |
    Select-Object -ExpandProperty Line)

if ($matchingLines.Count -eq 0) {
    Write-Warning "No lines containing 'sender=' were found in $LogPath"
    return
}

$recentLines = @($matchingLines | Select-Object -Last $Tail)

foreach ($line in $recentLines) {
    $stampMatch = [regex]::Match($line, $timestampPattern)

    if (-not $stampMatch.Success) {
        Write-Verbose "No timestamp found; passing line through unchanged: $line"
        if (-not $AsObject) { $line } else {
            [pscustomobject]@{
                LocalTime = $null
                Sender    = $null
                Recipient = $null
                Operation = $null
                Line      = $line
            }
        }
        continue
    }

    $utcText = $stampMatch.Groups['stamp'].Value

    # Parse as an unspecified-kind time, then tell the conversion it is UTC.
    $utc = [datetime]::ParseExact(
        $utcText,
        'yyyy/MM/dd HH:mm:ss',
        [System.Globalization.CultureInfo]::InvariantCulture,
        [System.Globalization.DateTimeStyles]::None)
    $utc = [datetime]::SpecifyKind($utc, [System.DateTimeKind]::Utc)

    $local = [System.TimeZoneInfo]::ConvertTimeFromUtc($utc, $mountainZone)

    # Derive the offset from the UTC delta rather than asking
    # IsDaylightSavingTime, which reports an ambiguous local time (the repeated
    # hour each fall) as standard time and would mislabel it.
    $offset = $local - $utc
    $abbreviation = if ($offset -eq $mountainZone.BaseUtcOffset) { 'MST' } else { 'MDT' }
    $localText = '{0:yyyy/MM/dd HH:mm:ss} {1}' -f $local, $abbreviation

    $rewritten = $line.Remove($stampMatch.Index, $stampMatch.Length).Insert($stampMatch.Index, $localText)

    if (-not $AsObject) {
        $rewritten
        continue
    }

    $senderMatch = [regex]::Match($line, 'sender="(?<value>[^"]*)"')
    $recipientMatch = [regex]::Match($line, 'recipient="(?<value>[^"]*)"')
    $operationMatch = [regex]::Match($line, 'operation=(?<value>\S+)')

    [pscustomobject]@{
        LocalTime = $localText
        Sender    = if ($senderMatch.Success) { $senderMatch.Groups['value'].Value } else { $null }
        Recipient = if ($recipientMatch.Success) { $recipientMatch.Groups['value'].Value } else { $null }
        Operation = if ($operationMatch.Success) { $operationMatch.Groups['value'].Value } else { $null }
        Line      = $rewritten
    }
}
