Describe 'install.ps1' {
    BeforeAll {
        function Get-InstallerPath {
            Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1'
        }

        # Runs $Body in a child process of the same shell, so the Windows
        # PowerShell 5.1 and pwsh CI steps each test their own host.
        #
        # The child's PATH is cut down to the shell's own directory. The
        # installer branches on `Get-Command scoop` before it does anything
        # else, and with Scoop resolvable a stable-channel run performs a real
        # `scoop bucket add` and `scoop install`. Scrubbing PATH pins every case
        # here to the direct-install branch whatever the host has installed; the
        # child reports the resolution so the scrub is asserted, not assumed.
        #
        # The child also shadows Invoke-RestMethod with a function that throws:
        # command resolution prefers functions over cmdlets, so the direct-install
        # branch's first network call fails with the sentinel and nothing is
        # downloaded.
        function Invoke-InstallerChild {
            param([string] $Body)
            $shell = (Get-Process -Id $PID).Path
            $preamble = @(
                "`$env:PATH = '$(Split-Path -Parent $shell)'"
                "'SCOOP:' + [bool](Get-Command scoop -ErrorAction SilentlyContinue)"
                "function Invoke-RestMethod { throw 'entire-test: network blocked' }"
            ) -join '; '
            $output = @(& $shell -NoProfile -NonInteractive -Command "$preamble; $Body" 2>&1 | ForEach-Object { "$_" })
            [pscustomobject]@{ Output = $output; ExitCode = $LASTEXITCODE }
        }

        function Assert-DirectInstallBranch {
            param([pscustomobject] $Run)
            $Run.Output | Should -Contain 'SCOOP:False'
            $Run.Output | Should -Contain '==> Installing Entire CLI...'
        }

        # The installer's own `exit 1` must not be reached when it runs inside
        # a live session, so the child continues past it and ends with its own
        # exit code: seeing 7 is the proof that the session survived.
        function Assert-ThrowsWithoutExiting {
            param([pscustomobject] $Run)
            Assert-DirectInstallBranch -Run $Run
            @($Run.Output | Where-Object { $_ -like 'CAUGHT:Error: *' }) | Should -HaveCount 1
            @($Run.Output | Where-Object { $_ -like 'Error: *' }) | Should -HaveCount 0
            if ($env:OS -eq 'Windows_NT') {
                $Run.Output | Should -Contain 'CAUGHT:Error: entire-test: network blocked'
            }
            $Run.Output | Should -Contain 'ALIVE'
            $Run.ExitCode | Should -Be 7
        }

        function Assert-NoExceptionFormatting {
            param([string[]] $Output)
            @($Output | Where-Object { $_ -like 'Error: *' }) | Should -HaveCount 1
            @($Output | Where-Object { $_ -match 'At line:|CategoryInfo|FullyQualifiedErrorId' }) | Should -HaveCount 0
        }
    }

    Context 'documented invocation shapes' {
        It 'runs as a scriptblock with -Help' {
            $installer = [scriptblock]::Create((Get-Content -Raw (Get-InstallerPath)))
            $output = @(& $installer -Help 6>&1 | ForEach-Object { "$_" })
            $output[0] | Should -BeLike 'Usage: install.ps1*'
        }

        It 'binds -Channel nightly through the scriptblock form' {
            $installer = [scriptblock]::Create((Get-Content -Raw (Get-InstallerPath)))
            $output = @(& $installer -Channel nightly -Help 6>&1 | ForEach-Object { "$_" })
            $output[0] | Should -BeLike 'Usage: install.ps1*'
        }

        It 'loads functions without installing when dot-sourced' {
            $run = Invoke-InstallerChild -Body ". '$(Get-InstallerPath)'; 'INSTALL-ENTIRE:' + [bool](Get-Command Install-Entire -CommandType Function -ErrorAction SilentlyContinue); 'ALIVE'; exit 7"
            $run.Output | Should -Contain 'INSTALL-ENTIRE:True'
            $run.Output | Should -Not -Contain '==> Installing Entire CLI...'
            $run.Output | Should -Contain 'ALIVE'
            $run.ExitCode | Should -Be 7
        }
    }

    Context 'error path' {
        It 'throws and leaves the session running when piped to Invoke-Expression' {
            $run = Invoke-InstallerChild -Body "try { Get-Content -Raw '$(Get-InstallerPath)' | Invoke-Expression } catch { 'CAUGHT:' + `$_.Exception.Message }; 'ALIVE'; exit 7"
            Assert-ThrowsWithoutExiting -Run $run
        }

        It 'throws and leaves the session running when run as a scriptblock with parameters' {
            $installDir = Join-Path $TestDrive 'bin'
            $run = Invoke-InstallerChild -Body "try { & ([scriptblock]::Create((Get-Content -Raw '$(Get-InstallerPath)'))) -InstallDir '$installDir' } catch { 'CAUGHT:' + `$_.Exception.Message }; 'ALIVE'; exit 7"
            Assert-ThrowsWithoutExiting -Run $run
        }

        It 'prints one Error: line and exits 1 when run from a file' {
            $installDir = Join-Path $TestDrive 'bin'
            $run = Invoke-InstallerChild -Body "& '$(Get-InstallerPath)' -InstallDir '$installDir'; exit `$LASTEXITCODE"
            Assert-DirectInstallBranch -Run $run
            Assert-NoExceptionFormatting -Output $run.Output
            if ($env:OS -eq 'Windows_NT') {
                $run.Output | Should -Contain 'Error: entire-test: network blocked'
            }
            $run.ExitCode | Should -Be 1
        }

        # The -File process is spawned from inside the scrubbed child so it
        # inherits the cut-down PATH, but it is a fresh process, so the
        # Invoke-RestMethod stub cannot reach it. A drive that does not exist
        # fails in Get-NormalizedPath, which runs before the registry read and
        # the first network call on every OS, so this case asserts the provider
        # message instead of the sentinel.
        It 'exits the process with 1 when run with -File' {
            $shell = (Get-Process -Id $PID).Path
            $run = Invoke-InstallerChild -Body "& '$shell' -NoProfile -NonInteractive -File '$(Get-InstallerPath)' -InstallDir 'entiretestnodrive:\bin'; exit `$LASTEXITCODE"
            Assert-DirectInstallBranch -Run $run
            Assert-NoExceptionFormatting -Output $run.Output
            @($run.Output | Where-Object { $_ -like 'Error: *Cannot find drive*entiretestnodrive*' }) | Should -HaveCount 1
            $run.ExitCode | Should -Be 1
        }
    }
}
