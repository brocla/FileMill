<#
.SYNOPSIS
    Exercises Deploy-FileMill.ps1 against a fake worker in a temporary
    directory, so the real deploy is not the first time it runs.

.DESCRIPTION
    Sets up a throwaway "repository": a bin\ holding a stub worker, a
    data\logs\ for it to write to, and a stub supervisor that relaunches the
    worker whenever it exits - the same shape as the real install, with none of
    its parts. The stub worker is a tiny Go program built three times with
    different stamped versions, one of which fails to start on purpose.

    Two cases are covered:

      1. A good build is deployed: the binary is swapped, the worker restarts
         into it, the new version reaches the log, and the previous binary is
         kept.
      2. A build that exits immediately is deployed: the deploy must notice
         that the new version never comes up, roll back, and exit non-zero,
         leaving the previous worker serving again.

    It needs no elevation (the stub worker runs in this session) and no modules.
    Exits 0 if every check passes, 1 otherwise.

    Run it under BOTH shells - powershell.exe (5.1, which is what an elevated
    window gives you, and what the deploy is run in) and pwsh.exe (7). They
    differ in ways that decide whether the deploy works at all: 5.1 reads a
    BOM-less file as ANSI, and it returns $null for .Count on a single
    CimInstance that PowerShell unwrapped out of an array. Both of those
    shipped in this script's first version and only 5.1 showed them.
#>
[CmdletBinding()]
param(
    # Where to build the throwaway repository. Removed afterwards unless -Keep.
    [string]$WorkRoot,
    [switch]$Keep
)

$ErrorActionPreference = 'Stop'

$script:failures = @()
function Check {
    # $Ok is deliberately untyped: a [bool] parameter throws when a helper
    # accidentally returns a collection, which hides the real failure behind a
    # binding error.
    param([string]$What, $Ok, [string]$Detail)
    if ($Ok) {
        Write-Host "  ok   $What" -ForegroundColor Green
    } else {
        Write-Host "  FAIL $What" -ForegroundColor Red
        if ($Detail) { Write-Host "       $Detail" }
        $script:failures += $What
    }
}

$script:polls = 0
$script:lastPollValue = '(never evaluated)'

function Wait-Until {
    param([scriptblock]$Condition, [int]$Seconds = 30)
    $script:polls = 0
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        $script:polls++
        # Kept as a value rather than tested inline, so a wait that gives up can
        # report what it was actually seeing instead of only that it failed.
        $value = & $Condition
        $script:lastPollValue = "[$value]"
        if ($value) { return $true }
        # 750ms, not 200: these conditions query Win32_Process, which starts
        # failing when it is asked several times a second.
        Start-Sleep -Milliseconds 750
    }
    return $false
}

$script:lastQueryError = $null

function Get-FakeWorkers {
    param([string]$Path)
    # Records what it saw on the way through: a wait that gives up has to be
    # able to say whether the query failed, matched nothing, or was handed the
    # wrong path.
    $script:lastQueryPath = $Path
    for ($attempt = 1; $attempt -le 4; $attempt++) {
        try {
            $all = @(Get-CimInstance Win32_Process -Filter "Name='filemill.exe'" -ErrorAction Stop)
            $found = @($all | Where-Object { $_.ExecutablePath -and ($_.ExecutablePath -ieq $Path) })
            $script:lastQueryError = $null
            $script:lastQueryCounts = "unfiltered=$($all.Count) filtered=$($found.Count)"
            return $found
        } catch {
            $script:lastQueryError = $_.Exception.Message
            Start-Sleep -Milliseconds 250
        }
    }
    return @()
}

# Show-WorkerState prints what the process list actually held when a wait gave
# up. A bare "never started" says nothing about whether the process was missing,
# somewhere else, or simply unreadable from this shell.
function Show-WorkerState {
    param([string]$Path)
    Write-Host "  expected worker path: [$Path]"
    Write-Host "  polls made: $script:polls, last condition value: $script:lastPollValue"
    Write-Host "  last query path: [$script:lastQueryPath]"
    Write-Host "  last query counts: $script:lastQueryCounts"
    if ($script:lastQueryError) { Write-Host "  last process-query error: $script:lastQueryError" }
    $all = @(Get-CimInstance Win32_Process -Filter "Name='filemill.exe'" -ErrorAction SilentlyContinue)
    Write-Host "  filemill.exe processes visible: $($all.Count)"
    foreach ($process in $all) {
        Write-Host ("    pid={0} path=[{1}]" -f $process.ProcessId, $process.ExecutablePath)
    }
}

$deployScript = Join-Path $PSScriptRoot 'Deploy-FileMill.ps1'
if (-not (Test-Path -LiteralPath $deployScript)) { throw "Deploy-FileMill.ps1 not found next to this script." }

# Test-Parses asks one PowerShell to parse a script and report syntax errors.
# The path travels in an environment variable so the command needs no quoting
# of its own.
function Test-Parses {
    param([string]$Shell, [string]$Path)
    $env:FILEMILL_PARSE_TARGET = $Path
    $code = '$e = $null; [void][System.Management.Automation.Language.Parser]::ParseFile($env:FILEMILL_PARSE_TARGET, [ref]$null, [ref]$e); if ($e) { $e | ForEach-Object { "line {0}: {1}" -f $_.Extent.StartLineNumber, $_.Message }; exit 1 }; exit 0'
    # Capture the child's output instead of letting it join this function's
    # return value, which would make the result an array rather than a boolean.
    $output = & $Shell -NoProfile -Command $code 2>&1
    $parsed = ($LASTEXITCODE -eq 0)
    if (-not $parsed) {
        $output | ForEach-Object { Write-Host "       $_" }
    }
    return $parsed
}

# These scripts are run by hand in an elevated Windows PowerShell, which is 5.1
# and parses more strictly than pwsh 7 - it rejects a double-quoted string
# inside $() inside another one, for instance. A harness that only ever ran
# under pwsh once let exactly that reach the operator, so both shells parse
# both scripts before anything else happens.
Write-Host 'Parsing the scripts in each installed PowerShell'
foreach ($shell in @('powershell.exe', 'pwsh.exe')) {
    if (-not (Get-Command $shell -ErrorAction SilentlyContinue)) {
        Write-Host "  skip $shell (not installed)"
        continue
    }
    foreach ($target in @($deployScript, $PSCommandPath)) {
        Check "$([System.IO.Path]::GetFileName($target)) parses in $shell" (Test-Parses $shell $target)
    }
}
# Windows PowerShell 5.1 reads a .ps1 with no byte-order mark as ANSI rather
# than UTF-8, so a character like an em dash arrives as three characters, one of
# which is a smart quote - and PowerShell accepts smart quotes as string
# delimiters. In a comment that is only mojibake. In a string it opens a string
# that never closes and the file stops parsing, which is exactly how a broken
# deploy script reached the operator once. Keeping these scripts ASCII-only
# sidesteps the encoding question rather than relying on remembering it.
Write-Host 'Checking the scripts are ASCII-only'
foreach ($script in (Get-ChildItem -LiteralPath $PSScriptRoot -Filter '*.ps1')) {
    $offenders = @()
    $number = 0
    foreach ($line in (Get-Content -LiteralPath $script.FullName -Encoding UTF8)) {
        $number++
        foreach ($char in $line.ToCharArray()) {
            if ([int]$char -gt 127) {
                $offenders += ('line {0}: U+{1:X4}' -f $number, [int]$char)
                break
            }
        }
    }
    Check "$($script.Name) is ASCII-only" ($offenders.Count -eq 0) ($offenders -join '; ')
}

if ($script:failures.Count -gt 0) {
    Write-Host ''
    Write-Host 'Fix the syntax or encoding problems above; not running the deploy cases.' -ForegroundColor Red
    exit 1
}

if (-not $WorkRoot) { $WorkRoot = Join-Path $env:TEMP ("filemill-deploy-test-" + [guid]::NewGuid().ToString('N').Substring(0, 8)) }
$binDir = Join-Path $WorkRoot 'bin'
$logDir = Join-Path $WorkRoot 'data\logs'
$srcDir = Join-Path $WorkRoot 'src'
$stage = Join-Path $WorkRoot 'stage'
$binary = Join-Path $binDir 'filemill.exe'
$logPath = Join-Path $logDir 'filemill.log'
foreach ($dir in @($binDir, $logDir, $srcDir, $stage)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }

# The stub worker: prints its version like the real one, appends the same
# startup line the Mailgun adapter logs, then idles until it is killed. Built
# with version "bad" it exits at once, which is the failure this has to catch.
$fakeSource = @'
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var version = "dev"

func logLine(text string) {
	path := filepath.Join("data", "logs", "filemill.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format("2006/01/02 15:04:05"), text)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("filemill " + version)
		return
	}
	if version == "bad" {
		// A build that dies at startup, as a bad config or a failed bind
		// would: it says why in the log, then exits.
		logLine("filemill: startup failed: stub worker refusing to run")
		os.Exit(3)
	}
	logLine(fmt.Sprintf("mailgun FileMill %s - webhook listening on :8080; delivery loop started", version))
	for {
		time.Sleep(time.Second)
	}
}
'@
Set-Content -Path (Join-Path $srcDir 'main.go') -Value $fakeSource -Encoding ascii
Set-Content -Path (Join-Path $srcDir 'go.mod') -Value "module fakemill`n`ngo 1.26.0`n" -Encoding ascii

Write-Host "Building stub workers in $WorkRoot"
Push-Location -LiteralPath $srcDir
try {
    foreach ($build in @(
            @{ Version = 'v-old'; Path = $binary },
            @{ Version = 'v-new'; Path = (Join-Path $stage 'filemill.new.exe') },
            @{ Version = 'bad'; Path = (Join-Path $stage 'filemill.bad.exe') })) {
        go build -ldflags "-X main.version=$($build.Version)" -o $build.Path .
        if ($LASTEXITCODE -ne 0) { throw "building the stub worker ($($build.Version)) failed" }
    }
} finally {
    Pop-Location
}

# The stub supervisor: relaunches the worker whenever it exits, like
# Supervise-FileMill.ps1, minus the backoff and logging.
$supervisorPath = Join-Path $WorkRoot 'stub-supervisor.ps1'
Set-Content -Path $supervisorPath -Encoding ascii -Value @'
param([string]$Executable, [string]$Root)
Set-Location -LiteralPath $Root
while ($true) {
    & $Executable run | Out-Null
    Start-Sleep -Milliseconds 300
}
'@

$supervisor = Start-Process -FilePath 'powershell.exe' `
    -ArgumentList '-NoProfile', '-ExecutionPolicy', 'Bypass', '-WindowStyle', 'Hidden', '-File', "`"$supervisorPath`"", '-Executable', "`"$binary`"", '-Root', "`"$WorkRoot`"" `
    -PassThru -WindowStyle Hidden

try {
    # @() around every call: PowerShell unwraps a one-element array on return,
    # and asking a lone CimInstance for .Count gets $null in Windows PowerShell
    # 5.1 (it looks for a CIM property by that name), so an unwrapped count
    # silently reads as "nothing running".
    if (-not (Wait-Until { @(Get-FakeWorkers $binary).Count -gt 0 })) { Show-WorkerState $binary; throw 'the stub worker never started' }
    if (-not (Wait-Until { (Test-Path $logPath) -and ((Get-Content $logPath -Raw) -match 'v-old') })) { Show-WorkerState $binary; throw 'the stub worker never logged' }
    $originalPid = @(Get-FakeWorkers $binary)[0].ProcessId

    Write-Host ''
    Write-Host 'Case 1: a good build deploys and the worker comes back on it'
    & $deployScript -RepositoryRoot $WorkRoot -NewBinary (Join-Path $stage 'filemill.new.exe') `
        -Yes -NoElevationCheck -TimeoutSeconds 30 | Out-Null
    $exit = $LASTEXITCODE
    Check 'deploy exits 0' ($exit -eq 0) "exit code $exit"
    Check 'bin\filemill.exe is now the new build' ((& $binary --version) -match 'v-new')
    Check 'the previous binary was kept' (Test-Path (Join-Path $binDir 'filemill.v-old.exe'))
    Check 'the worker was restarted' (Wait-Until { $running = @(Get-FakeWorkers $binary); ($running.Count -gt 0) -and ($running[0].ProcessId -ne $originalPid) })
    Check 'the new version reached the log' ((Get-Content $logPath -Raw) -match 'FileMill v-new')

    Write-Host ''
    Write-Host 'Case 2: a build that will not start is rolled back'
    $beforePid = @(Get-FakeWorkers $binary)[0].ProcessId
    # 6>&1 captures Write-Host output (the information stream), so the failure
    # report itself can be checked: a rolled-back deploy has to say why.
    $output = & $deployScript -RepositoryRoot $WorkRoot -NewBinary (Join-Path $stage 'filemill.bad.exe') `
        -Yes -NoElevationCheck -TimeoutSeconds 10 6>&1 | Out-String
    $exit = $LASTEXITCODE
    Check 'deploy exits non-zero' ($exit -ne 0) "exit code $exit"
    Check 'the failure report shows what the worker logged' ($output -match 'startup failed') $output
    Check 'bin\filemill.exe is back to the previous build' ((& $binary --version) -match 'v-new')
    Check 'the build that failed was kept for inspection' (Test-Path (Join-Path $binDir 'filemill.failed.exe'))
    Check 'a worker is serving again' (Wait-Until { $running = @(Get-FakeWorkers $binary); ($running.Count -gt 0) -and ($running[0].ProcessId -ne $beforePid) })
    Check 'it is the previous version' (Wait-Until { ((Get-Content $logPath -Raw) -split "`r?`n" | Where-Object { $_ -match 'FileMill v-new' }).Count -ge 2 })
} finally {
    Stop-Process -Id $supervisor.Id -Force -ErrorAction SilentlyContinue
    foreach ($worker in (Get-FakeWorkers $binary)) { Stop-Process -Id $worker.ProcessId -Force -ErrorAction SilentlyContinue }
    Start-Sleep -Milliseconds 500
    if (-not $Keep) {
        Remove-Item -LiteralPath $WorkRoot -Recurse -Force -ErrorAction SilentlyContinue
    } else {
        Write-Host "Kept $WorkRoot"
    }
}

Write-Host ''
if ($script:failures.Count -gt 0) {
    Write-Host "$($script:failures.Count) check(s) failed." -ForegroundColor Red
    exit 1
}
Write-Host 'All checks passed.' -ForegroundColor Green
exit 0
