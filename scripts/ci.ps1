<#
.SYNOPSIS
    Runs the same gates as the "Windows CI" workflow.

.DESCRIPTION
    Single source of truth for the `test` and `race` stages of
    .github/workflows/windows.yml.  The default `test` stage is what CI runs on
    a push; run `-Stage race` too for the full gate.

        ./scripts/ci.ps1              # test stage (what CI runs on push)
        ./scripts/ci.ps1 -Quick       # same, minus the slower end-to-end CLI test
        ./scripts/ci.ps1 -Stage race  # the -race subset (needs a C compiler)

    Local Go/Node versions can still differ from the runner's, so this reduces
    surprises rather than guaranteeing a green run.  Keep the workflow's job
    bodies delegating here; a gate that lives in only one of the two places
    will eventually stop running in the other.
#>
param(
    [ValidateSet('test', 'race')]
    [string]$Stage = 'test',

    # Local iteration only: skip the end-to-end CLI test.
    [switch]$Quick
)

$ErrorActionPreference = 'Stop'

$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

# Concurrency-sensitive tests that the race stage compiles with -race.
$RaceTests = 'TestSeededQueryOracleMatrix|TestSecondOracleSeededSubsample|TestEngineOverlayMutationStateMachineMatchesFreshOracle|TestEngineConcurrentChurnQueryStress|TestEngineOverlayMergeFillsLimitAndRanksCreates|TestEngineOverlayCountOnlySearchIncludesCreatesAndExcludesTombstones|TestEngineReadViewCachesUseSnapshotGeneration|TestEnginePersistVolumeCompactsOverlayToMappedBase|TestServiceVolumeIndexReplaysBinaryWAL|TestRewriteWALWithNoFramesResetsTheWAL|TestReplayLoopRetiresWhenGenChanges|TestReplayGenGuardDropsStaleBatch|TestPersistOverlayCarryKeepsPostSnapshotChanges|TestBackgroundNameTrigramBuildPublishesAtomicIndex|TestBackgroundNameOrderBuildPublishesResidentView|TestLockVolumeSearchCancelsWhileWaiting|TestPostingPrefetchHonorsBoundAndCancellation|TestServiceWatchDeltaCreatesModifiesDeletes|TestStandaloneServicePipeSearchEndToEnd|TestBroadPathScanCancellationPropagates'

function Write-Step {
    param([string]$Name)
    Write-Host ''
    Write-Host "==> $Name" -ForegroundColor Cyan
}

function Assert-LastExit {
    param([string]$Name)
    if ($LASTEXITCODE -ne 0) {
        throw "CI step '$Name' failed with exit code $LASTEXITCODE"
    }
}

function Invoke-Native {
    param([string]$Name, [scriptblock]$Body)
    Write-Step $Name
    & $Body
    Assert-LastExit $Name
}

if ($Stage -eq 'race') {
    # Matches the race job: the same seeded query matrix CI pins.
    $env:SEEKFS_QUERY_MATRIX_SEED = '1'
    $env:CGO_ENABLED = '1'
    Invoke-Native 'go test -race (concurrency subset)' {
        go test -race ./cmd/seekfs -run $RaceTests
    }
    Write-Host ''
    Write-Host "race stage passed." -ForegroundColor Green
    return
}

Write-Step 'toolchain'
Write-Host "  $(go version)"
Write-Host "  node $(node --version)"

# The Windows CI runner checks out with core.autocrlf=true; .gitattributes pins
# Go sources to LF so the gofmt gate is stable.  A stale clone without that
# filter would fail gofmt in CI, so catch it here first.
Write-Step 'Go sources are LF'
$eol = @(git ls-files --eol -- '*.go')
if ($LASTEXITCODE -ne 0) {
    throw 'git ls-files --eol failed'
}
# Each line is "i/<eol>  w/<eol>  attr/<...>" then a TAB then the path, so only
# the columns are inspected: a path could itself contain "w/crlf".  "mixed"
# means the file blended both endings and gofmt will still reject it.
$bad = @($eol | Where-Object { $_.Split("`t")[0] -match 'w/(crlf|mixed)' })
if ($bad.Count -gt 0) {
    throw "$($bad.Count) tracked Go file(s) are checked out as CRLF or mixed; gofmt will fail in CI. Re-checkout them with: git checkout -- '*.go'"
}

Invoke-Native 'gofmt' {
    $out = gofmt -l ./cmd/seekfs
    if ($out) {
        throw "gofmt needed on:`n$($out -join "`n")"
    }
}

Invoke-Native 'go vet' {
    go vet ./...
}

Write-Step 'staticcheck'
if (-not (Get-Command staticcheck -ErrorAction SilentlyContinue)) {
    throw 'staticcheck not found; install it with: go install honnef.co/go/tools/cmd/staticcheck@latest'
}
# U1000 (unused symbols) is excluded: without a tag-aware run, code that only
# compiles under the seekfs_ui tag reads as dead.
$previousPreference = $ErrorActionPreference
$ErrorActionPreference = 'Continue'
try {
    $staticcheckOut = (& staticcheck '-checks=all,-U1000' ./... 2>&1 | Out-String)
    $staticcheckCode = $LASTEXITCODE
} finally {
    $ErrorActionPreference = $previousPreference
}
if ($staticcheckOut -match 'internal error in importing|unsupported version') {
    throw 'staticcheck is too old for this Go toolchain; upgrade it with: go install honnef.co/go/tools/cmd/staticcheck@latest'
}
if ($staticcheckCode -ne 0) {
    Write-Host $staticcheckOut
    throw "staticcheck reported findings (exit $staticcheckCode)"
}

Invoke-Native 'go test ./... (with coverage)' {
    # Quoted: PowerShell splits an unquoted -flag=value at the dot (go sees ".out").
    go test '-coverprofile=coverage.out' '-covermode=atomic' ./...
}

Write-Step 'coverage total'
Write-Host "  $((go tool cover '-func=coverage.out' | Select-Object -Last 1))"

Invoke-Native 'go test (UI build tags)' {
    go test -tags 'seekfs_ui production' ./cmd/seekfs
}

Invoke-Native 'frontend unit tests' {
    node --test scripts/ui_query.test.cjs scripts/ui_util.test.cjs
}

Invoke-Native 'go build' {
    go build -o seekfs.exe ./cmd/seekfs
}

if (-not $Quick) {
    Invoke-Native 'CLI integration test' {
        powershell -ExecutionPolicy Bypass -File .\scripts\test_seekfs_cli.ps1 -Exe .\seekfs.exe
    }
}

Write-Host ''
Write-Host "test stage passed." -ForegroundColor Green
