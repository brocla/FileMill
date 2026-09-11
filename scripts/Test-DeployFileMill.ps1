<#
.SYNOPSIS
    Exercises Deploy-FileMill.ps1 against a fake worker in a temporary
    directory, so the real deploy is not the first time it runs.

.DESCRIPTION
    Sets up a throwaway "repository": a bin\ holding a stub worker, a
    data\logs\ for it to write to, and a stub supervisor that relaunches the
    worker whenever it exits — the same shape as the real install, with none of
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
    param([string]$What, [bool]$Ok, [string]$Detail)
    if ($Ok) {
        Write-Host "  ok   $What" -ForegroundColor Green
    } else {
        Write-Host "  FAIL $What" -ForegroundColor Red
        if ($Detail) { Write-Host "       $Detail" }
        $script:failures += $What
    }
}

function Wait-Until {
    param([scriptblock]$Condition, [int]$Seconds = 30)
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        if (& $Condition) { return $true }
        Start-Sleep -Milliseconds 200
    }
    return $false
}

function Get-FakeWorkers {
    param([string]$Path)
    return @(Get-CimInstance Win32_Process -Filter "Name='filemill.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.ExecutablePath -and ($_.ExecutablePath -ieq $Path) })
}

$deployScript = Join-Path $PSScriptRoot 'Deploy-FileMill.ps1'
if (-not (Test-Path -LiteralPath $deployScript)) { throw "Deploy-FileMill.ps1 not found next to this script." }

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
    if (-not (Wait-Until { (Get-FakeWorkers $binary).Count -gt 0 })) { throw 'the stub worker never started' }
    if (-not (Wait-Until { (Test-Path $logPath) -and ((Get-Content $logPath -Raw) -match 'v-old') })) { throw 'the stub worker never logged' }
    $originalPid = (Get-FakeWorkers $binary)[0].ProcessId

    Write-Host ''
    Write-Host 'Case 1: a good build deploys and the worker comes back on it'
    & $deployScript -RepositoryRoot $WorkRoot -NewBinary (Join-Path $stage 'filemill.new.exe') `
        -Yes -NoElevationCheck -TimeoutSeconds 30 | Out-Null
    $exit = $LASTEXITCODE
    Check 'deploy exits 0' ($exit -eq 0) "exit code $exit"
    Check 'bin\filemill.exe is now the new build' ((& $binary --version) -match 'v-new')
    Check 'the previous binary was kept' (Test-Path (Join-Path $binDir 'filemill.v-old.exe'))
    Check 'the worker was restarted' (Wait-Until { $running = Get-FakeWorkers $binary; ($running.Count -gt 0) -and ($running[0].ProcessId -ne $originalPid) })
    Check 'the new version reached the log' ((Get-Content $logPath -Raw) -match 'FileMill v-new')

    Write-Host ''
    Write-Host 'Case 2: a build that will not start is rolled back'
    $beforePid = (Get-FakeWorkers $binary)[0].ProcessId
    # 6>&1 captures Write-Host output (the information stream), so the failure
    # report itself can be checked: a rolled-back deploy has to say why.
    $output = & $deployScript -RepositoryRoot $WorkRoot -NewBinary (Join-Path $stage 'filemill.bad.exe') `
        -Yes -NoElevationCheck -TimeoutSeconds 10 6>&1 | Out-String
    $exit = $LASTEXITCODE
    Check 'deploy exits non-zero' ($exit -ne 0) "exit code $exit"
    Check 'the failure report shows what the worker logged' ($output -match 'startup failed') $output
    Check 'bin\filemill.exe is back to the previous build' ((& $binary --version) -match 'v-new')
    Check 'the build that failed was kept for inspection' (Test-Path (Join-Path $binDir 'filemill.failed.exe'))
    Check 'a worker is serving again' (Wait-Until { $running = Get-FakeWorkers $binary; ($running.Count -gt 0) -and ($running[0].ProcessId -ne $beforePid) })
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
