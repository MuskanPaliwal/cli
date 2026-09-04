#Requires -Version 5.1

# Runs the PowerShell test suite under scripts/test. CI (both Windows
# PowerShell 5.1 and pwsh) and `mise run test:ps1` call this, so Pester
# configuration lives here once. On a fresh machine run scripts/test/init.ps1
# first; it installs Pester, PSScriptAnalyzer and, on Windows PowerShell 5.1,
# the NuGet provider they need.
#
# The module floors are enforced with Import-Module rather than `#Requires
# -Modules`: under `pwsh -File`, a `#Requires -Modules` line makes the script's
# `exit` code come back as 0, so a failing suite would pass CI. Import-Module
# also selects the highest satisfying version, which matters on Windows where
# the OS-bundled Pester 3.4.0 sits next to the modern one.
$ErrorActionPreference = 'Stop'
Import-Module Pester -MinimumVersion 5.2.0
Import-Module PSScriptAnalyzer -MinimumVersion 1.17.1

$pesterConfig = New-PesterConfiguration -Hashtable @{
    Run    = @{
        Path     = $PSScriptRoot
        PassThru = $true
    }
    Output = @{
        Verbosity = 'Detailed'
    }
}

$result = Invoke-Pester -Configuration $pesterConfig
exit $result.FailedCount
