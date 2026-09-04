#Requires -Version 5.1

# Runs scripts/install.ps1 for real against GitHub Releases and checks what it
# leaves behind. It rewrites the user PATH and installs binaries under the
# profile, so it refuses to run anywhere but GitHub Actions; the raw user PATH
# and its value kind are restored at the end regardless.
#
# Phases, each asserting before the next:
#   1. stable install through the documented `irm | iex` shape
#   2. reinstall while entire.exe is running, then again after it exits
#   3. nightly install into a second directory, alongside the first

# Phase 1 runs the installer through the documented `irm | iex` shape, which
# is the point of that phase.
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSAvoidUsingInvokeExpression', '', Justification = 'Phase 1 exercises the documented irm | iex shape')]
param()

$ErrorActionPreference = 'Stop'

if ($env:CI -ne 'true' -or $env:GITHUB_ACTIONS -ne 'true') {
    throw 'scripts/test/e2e.ps1 installs into the user profile and rewrites the user PATH; it runs only in GitHub Actions CI.'
}

$installerPath = Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1'
# Dot-sourcing loads the functions without installing.
. $installerPath

function Assert-Equal {
    param([string] $Phase, [string] $What, $Expected, $Actual)
    if (-not [object]::Equals($Expected, $Actual)) {
        throw "[$Phase] $What`n  expected: $Expected`n  actual:   $Actual"
    }
}

function Assert-True {
    param([string] $Phase, [string] $What, [bool] $Condition)
    if (-not $Condition) {
        throw "[$Phase] $What"
    }
}

function Get-PathEntry {
    param([AllowNull()] [string] $Value)
    if ([string]::IsNullOrEmpty($Value)) {
        return @()
    }
    return @($Value -split ';')
}

function Get-UserPathKind {
    $key = (Get-Item -LiteralPath 'HKCU:').OpenSubKey('Environment')
    if ($null -eq $key -or $null -eq $key.GetValue('Path', $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)) {
        return $null
    }
    return $key.GetValueKind('Path')
}

# The kind Write-UserEnvironmentValue promises: ExpandString when the value
# contains %, otherwise whatever the value had before, String if it did not
# exist.
function Get-ExpectedKind {
    param([string] $NewValue, $PreviousKind)
    if ($NewValue.Contains('%')) {
        return [Microsoft.Win32.RegistryValueKind]::ExpandString
    }
    if ($null -ne $PreviousKind) {
        return $PreviousKind
    }
    return [Microsoft.Win32.RegistryValueKind]::String
}

function Invoke-Installer {
    param([string[]] $Arguments)
    $installer = [scriptblock]::Create((Get-Content -Raw -LiteralPath $installerPath))
    # Write-Host is the information stream; merge it so the report can be asserted.
    & $installer @Arguments 6>&1 | ForEach-Object { "$_" }
}

function Get-EntireVersion {
    param([string] $Phase, [string] $Directory)
    $output = & (Join-Path $Directory 'entire.exe') version 2>&1 | Out-String
    Assert-Equal -Phase $Phase -What 'entire.exe version exit code' -Expected 0 -Actual $LASTEXITCODE
    return $output.Trim()
}

$snapshot = Get-UserEnvironmentValue -Name 'Path'
$snapshotKind = Get-UserPathKind
$snapshotEntries = Get-PathEntry $snapshot
$stableDir = Join-Path $env:USERPROFILE '.local\bin'
$nightlyDir = Join-Path $env:USERPROFILE 'entire-nightly'
$running = $null

try {
    # ---- Phase 1: stable through irm | iex --------------------------------
    $phase = 'phase 1: stable via iex'
    Write-Host "==> $phase"
    Get-Content -Raw -LiteralPath $installerPath | Invoke-Expression

    Assert-True -Phase $phase -What 'entire.exe installed' -Condition (Test-Path -LiteralPath (Join-Path $stableDir 'entire.exe') -PathType Leaf)
    Assert-True -Phase $phase -What 'git-remote-entire.exe installed' -Condition (Test-Path -LiteralPath (Join-Path $stableDir 'git-remote-entire.exe') -PathType Leaf)

    $afterStable = Get-UserEnvironmentValue -Name 'Path'
    $entries = Get-PathEntry $afterStable
    Assert-Equal -Phase $phase -What 'first raw PATH entry' -Expected $stableDir -Actual $entries[0]
    Assert-Equal -Phase $phase -What 'remaining raw PATH entries' -Expected ($snapshotEntries -join ';') -Actual (($entries | Select-Object -Skip 1) -join ';')
    Assert-Equal -Phase $phase -What 'raw PATH value kind' -Expected (Get-ExpectedKind -NewValue $afterStable -PreviousKind $snapshotKind) -Actual (Get-UserPathKind)
    Assert-Equal -Phase $phase -What 'first in-process $env:Path entry' -Expected $stableDir -Actual ((Get-PathEntry $env:Path)[0])
    $stableVersion = Get-EntireVersion -Phase $phase -Directory $stableDir
    Write-Host "    stable: $stableVersion"

    # ---- Phase 2: reinstall while entire.exe is running -------------------
    $phase = 'phase 2: reinstall while running'
    Write-Host "==> $phase"
    $startInfo = New-Object System.Diagnostics.ProcessStartInfo
    $startInfo.FileName = Join-Path $stableDir 'entire.exe'
    $startInfo.Arguments = 'mcp'
    $startInfo.UseShellExecute = $false
    $startInfo.RedirectStandardInput = $true
    $startInfo.RedirectStandardOutput = $true
    # stdin is never written to or closed, so the MCP server waits on it.
    $running = [System.Diagnostics.Process]::Start($startInfo)
    Start-Sleep -Seconds 2
    Assert-True -Phase $phase -What 'entire mcp is still running' -Condition (-not $running.HasExited)

    # A mapped executable image refuses to be opened for writing. Prove that
    # without writing anything: if the open succeeds, the file is untouched
    # and the precondition, not a later phase, is what fails.
    $opened = $false
    try {
        $handle = [IO.File]::Open((Join-Path $stableDir 'entire.exe'), [IO.FileMode]::Open, [IO.FileAccess]::Write)
        $handle.Dispose()
        $opened = $true
    }
    catch {
        $opened = $false
    }
    Assert-True -Phase $phase -What 'running entire.exe refuses a write handle (precondition)' -Condition (-not $opened)

    Invoke-Installer | Out-Null
    Assert-True -Phase $phase -What 'entire.exe.old left behind while the old image runs' -Condition (Test-Path -LiteralPath (Join-Path $stableDir 'entire.exe.old') -PathType Leaf)
    Get-EntireVersion -Phase $phase -Directory $stableDir | Out-Null
    Assert-Equal -Phase $phase -What 'raw PATH after reinstall' -Expected $afterStable -Actual (Get-UserEnvironmentValue -Name 'Path')

    $running.Kill()
    $running.WaitForExit()
    $running = $null
    Invoke-Installer | Out-Null
    $stale = @(Get-ChildItem -LiteralPath $stableDir -Filter 'entire.exe*.old' -File)
    Assert-Equal -Phase $phase -What 'stale .old files after the old image exited' -Expected 0 -Actual $stale.Count
    Assert-Equal -Phase $phase -What 'raw PATH after the third install' -Expected $afterStable -Actual (Get-UserEnvironmentValue -Name 'Path')

    # ---- Phase 3: nightly into a second directory -------------------------
    $phase = 'phase 3: nightly to a custom dir'
    Write-Host "==> $phase"
    $report = Invoke-Installer -Arguments @('-Channel', 'nightly', '-InstallDir', $nightlyDir) | Out-String

    Assert-True -Phase $phase -What 'conflict warning printed' -Condition ($report -match '! WARNING: PATH conflict detected')
    Assert-True -Phase $phase -What 'stable install named as the other copy' -Condition ($report.Contains("! Also found:   $(Join-Path $stableDir 'entire.exe')"))
    Assert-True -Phase $phase -What 'nightly install reported as taking priority' -Condition ($report -match 'The installed version takes priority')

    $afterNightly = Get-UserEnvironmentValue -Name 'Path'
    $entries = Get-PathEntry $afterNightly
    Assert-Equal -Phase $phase -What 'first raw PATH entry' -Expected $nightlyDir -Actual $entries[0]
    Assert-Equal -Phase $phase -What 'second raw PATH entry (stable, moved down, not removed)' -Expected $stableDir -Actual $entries[1]
    Assert-Equal -Phase $phase -What 'remaining raw PATH entries' -Expected ($snapshotEntries -join ';') -Actual (($entries | Select-Object -Skip 2) -join ';')
    Assert-Equal -Phase $phase -What 'raw PATH value kind' -Expected (Get-ExpectedKind -NewValue $afterNightly -PreviousKind $snapshotKind) -Actual (Get-UserPathKind)
    $nightlyVersion = Get-EntireVersion -Phase $phase -Directory $nightlyDir
    Write-Host "    nightly: $nightlyVersion"
    Assert-True -Phase $phase -What 'nightly binary reports a nightly version' -Condition ($nightlyVersion -match 'nightly')
    Assert-True -Phase $phase -What 'nightly and stable versions differ' -Condition ($nightlyVersion -ne $stableVersion)

    Write-Host '==> all phases passed'
}
finally {
    if ($null -ne $running -and -not $running.HasExited) {
        $running.Kill()
        $running.WaitForExit()
    }
    # The runner is ephemeral; restoring anyway records that this script
    # knows it mutated shared state.
    $key = (Get-Item -LiteralPath 'HKCU:').CreateSubKey('Environment')
    if ($null -eq $snapshot) {
        $key.DeleteValue('Path', $false)
    }
    else {
        $key.SetValue('Path', $snapshot, $snapshotKind)
    }
}
