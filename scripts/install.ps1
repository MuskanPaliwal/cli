[CmdletBinding()]
param(
    [ValidateSet("stable", "nightly")]
    [string] $Channel = "stable",

    [ValidateNotNullOrEmpty()]
    [string] $InstallDir = (Join-Path $HOME ".local\bin"),

    [switch] $NoPathUpdate,

    [Alias("h")]
    [switch] $Help
)

# Keep all installer functions and preference changes in a child scope. This
# matters when the script is piped to Invoke-Expression in an existing shell.
# Path is empty under irm | iex; pass it in so the child can tell file vs pipe
# without leaking a variable into the caller's session.
& {
    param(
        [string] $SelectedChannel,
        [string] $SelectedInstallDir,
        [bool] $SkipPathUpdate,
        [bool] $ShowHelp,
        [bool] $InvokedFromFile
    )

    Set-StrictMode -Version 2.0
    $ErrorActionPreference = "Stop"
    $ProgressPreference = "SilentlyContinue"

    $GitHubRepo = "entireio/cli"
    $ScoopBucketUrl = "https://github.com/entireio/scoop-bucket.git"
    $WebTimeoutSec = 60

    function Write-Info {
        param([string] $Message)
        Write-Host "==> $Message" -ForegroundColor Cyan
    }

    function Write-Success {
        param([string] $Message)
        Write-Host "==> $Message" -ForegroundColor Green
    }

    function Write-InstallerWarning {
        param([string] $Message)
        Write-Host "Warning: $Message" -ForegroundColor Yellow
    }

    function Write-Usage {
        Write-Host @"
Usage: install.ps1 [-Channel stable|nightly] [-InstallDir <path>] [-NoPathUpdate]

Options:
  -Channel       Release channel to install (default: stable)
  -InstallDir    Direct-install destination (default: `$HOME\.local\bin)
  -NoPathUpdate  Do not add a direct-install destination to the user PATH
  -Help, -h      Show this help message

Stable installs use Scoop when it is available. Nightly installs use the
verified release archive because the Scoop bucket only publishes stable builds.

Scoop chooses its own install location and manages its own PATH entry, so
-InstallDir and -NoPathUpdate apply only to release-archive installs.
"@
    }

    function Invoke-Scoop {
        param(
            [Parameter(Mandatory = $true, ValueFromRemainingArguments = $true)]
            [string[]] $ScoopArgs
        )

        # Native stderr redirected with 2>&1 becomes ErrorRecord. With
        # $ErrorActionPreference Stop that is terminating even when scoop
        # exits 0 (update notices, deprecation warnings).
        $previous = $ErrorActionPreference
        $ErrorActionPreference = "Continue"
        try {
            $output = & "scoop" @ScoopArgs 2>&1
            return @{
                ExitCode = [int] $LASTEXITCODE
                Output   = $output
            }
        }
        finally {
            $ErrorActionPreference = $previous
        }
    }

    function Test-ScoopAppInstalled {
        param([string] $AppName)

        # Only the exit code matters, so discard every stream. scoop's abort /
        # warn / info all use Write-Host, i.e. the information stream, which
        # 2>&1 neither captures nor suppresses -- a failed probe would
        # otherwise print "Could not find app path for '<app>'" straight to the
        # console and read as a failure during a successful first install.
        # Deliberately not folded into Invoke-Scoop: a blanket redirect there
        # would also hide scoop's install and update progress, which is useful.
        $previous = $ErrorActionPreference
        $ErrorActionPreference = "Continue"
        try {
            & "scoop" prefix $AppName *> $null
            return $LASTEXITCODE -eq 0
        }
        finally {
            $ErrorActionPreference = $previous
        }
    }

    function Install-EntireWithScoop {
        Write-Info "Scoop detected; installing Entire CLI with Scoop..."

        $listed = Invoke-Scoop bucket list
        if ($listed.ExitCode -ne 0) {
            $details = ($listed.Output | Out-String).Trim()
            throw "Failed to list Scoop buckets. $details"
        }

        $bucketList = $listed.Output | Out-String
        if ($bucketList -notmatch "(?im)^\s*entire(?:\s|$)") {
            Write-Info "Adding the Entire Scoop bucket..."
            $added = Invoke-Scoop bucket add entire $ScoopBucketUrl
            if ($added.ExitCode -ne 0) {
                throw "Failed to add the Entire Scoop bucket."
            }
        }

        $isInstalled = Test-ScoopAppInstalled -AppName "entire"
        if ($isInstalled) {
            Write-Info "Updating Entire CLI with Scoop..."
            $updated = Invoke-Scoop update entire/entire
            if ($updated.ExitCode -ne 0) {
                throw "Scoop failed to install Entire CLI."
            }
        }
        else {
            Write-Info "Installing Entire CLI with Scoop..."
            $installed = Invoke-Scoop install entire/entire
            if ($installed.ExitCode -ne 0) {
                throw "Scoop failed to install Entire CLI."
            }
        }

        Write-Success "Entire CLI installed with Scoop"
    }

    function Get-PlatformArchitecture {
        # The machine-level value is the native OS architecture. The
        # $env:PROCESSOR_ARCHITECTURE seen by the script is the process's, so
        # x64 PowerShell on ARM64 Windows reports AMD64. RuntimeInformation is
        # not an option either: it needs .NET 4.7.1+, so it is missing on
        # stock Windows PowerShell 5.1 installs.
        $environmentKey = "HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment"
        $architecture = (Get-ItemProperty -LiteralPath $environmentKey -Name "PROCESSOR_ARCHITECTURE").PROCESSOR_ARCHITECTURE

        if ([string]::IsNullOrWhiteSpace($architecture)) {
            throw "Cannot determine the Windows architecture."
        }

        switch ($architecture.ToUpperInvariant()) {
            "AMD64" { return "amd64" }
            "ARM64" { return "arm64" }
            default { throw "Unsupported architecture: $architecture" }
        }
    }

    function Invoke-GitHubApi {
        param([string] $Uri)

        $headers = @{
            "Accept"     = "application/vnd.github+json"
            "User-Agent" = "entire-install.ps1"
        }
        if (-not [string]::IsNullOrWhiteSpace($env:GITHUB_TOKEN)) {
            $headers["Authorization"] = "Bearer $($env:GITHUB_TOKEN)"
        }

        # -UseBasicParsing is required for Windows PowerShell 5.1 (skips the IE
        # DOM parser). In PowerShell 7+ it is accepted and silently ignored.
        Invoke-RestMethod -Uri $Uri -Headers $headers -TimeoutSec $WebTimeoutSec -UseBasicParsing
    }

    function Get-ReleaseVersion {
        param([string] $ReleaseChannel)

        if ($ReleaseChannel -eq "nightly") {
            $uri = "https://api.github.com/repos/$GitHubRepo/releases?per_page=20"
            $releases = Invoke-GitHubApi -Uri $uri
            $release = $null

            # GitHub returns created_at descending. The first *nightly* tag is
            # the latest nightly. Windows PowerShell 5.1 returns a JSON array
            # from Invoke-RestMethod as one pipeline object, so enumerate it
            # explicitly instead of piping to Where-Object.
            foreach ($candidate in $releases) {
                if ($candidate.tag_name -like "*nightly*") {
                    $release = $candidate
                    break
                }
            }
        }
        else {
            $uri = "https://api.github.com/repos/$GitHubRepo/releases/latest"
            $release = Invoke-GitHubApi -Uri $uri
        }

        if ($null -eq $release -or [string]::IsNullOrWhiteSpace([string] $release.tag_name)) {
            throw "Failed to fetch the latest $ReleaseChannel version from GitHub. Check your internet connection."
        }

        $version = ([string] $release.tag_name) -replace "^v", ""
        if ($version -notmatch "^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$") {
            throw "GitHub returned an invalid release version: $version"
        }

        return $version
    }

    function Save-RemoteFile {
        param(
            [string] $Uri,
            [string] $Destination
        )

        # -UseBasicParsing is required for Windows PowerShell 5.1 (skips the IE
        # DOM parser). In PowerShell 7+ it is accepted and silently ignored.
        Invoke-WebRequest -Uri $Uri -OutFile $Destination -UseBasicParsing -TimeoutSec $WebTimeoutSec
    }

    function Assert-Checksum {
        param(
            [string] $ArchivePath,
            [string] $ArchiveName,
            [string] $ChecksumsPath
        )

        $escapedArchiveName = [regex]::Escape($ArchiveName)
        $checksumLine = Get-Content -LiteralPath $ChecksumsPath |
            Where-Object { $_ -match "(?i)^[0-9a-f]{64}\s+\*?$escapedArchiveName\s*$" } |
            Select-Object -First 1

        if ([string]::IsNullOrWhiteSpace([string] $checksumLine)) {
            throw "Checksum for $ArchiveName was not found in checksums.txt."
        }

        $expectedChecksum = ([string] $checksumLine -split "\s+")[0]
        $actualChecksum = (Get-FileHash -LiteralPath $ArchivePath -Algorithm SHA256).Hash
        if (-not [string]::Equals($actualChecksum, $expectedChecksum, [StringComparison]::OrdinalIgnoreCase)) {
            throw "Checksum verification failed. Expected: $expectedChecksum, actual: $actualChecksum"
        }
    }

    function Get-NormalizedPath {
        param([string] $Path)

        # Resolve against $PWD and expand ~. [IO.Path]::GetFullPath alone uses
        # [Environment]::CurrentDirectory, which Windows PowerShell 5.1 does
        # not keep in sync with Set-Location, and does not expand ~.
        $fullPath = [IO.Path]::GetFullPath(
            $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Path)
        )
        $root = [IO.Path]::GetPathRoot($fullPath)
        if ([string]::Equals($fullPath, $root, [StringComparison]::OrdinalIgnoreCase)) {
            return $root
        }

        [char[]] $separators = @(
            [IO.Path]::DirectorySeparatorChar,
            [IO.Path]::AltDirectorySeparatorChar
        )
        return $fullPath.TrimEnd($separators)
    }

    function Test-SamePath {
        param(
            [string] $Left,
            [string] $Right
        )

        if ([string]::IsNullOrWhiteSpace($Left) -or [string]::IsNullOrWhiteSpace($Right)) {
            return $false
        }

        try {
            return [string]::Equals(
                (Get-NormalizedPath -Path $Left),
                (Get-NormalizedPath -Path $Right),
                [StringComparison]::OrdinalIgnoreCase
            )
        }
        catch {
            return $false
        }
    }

    function Test-PathContains {
        param(
            [AllowNull()]
            [string] $PathValue,
            [string] $Directory
        )

        if ([string]::IsNullOrWhiteSpace($PathValue)) {
            return $false
        }

        foreach ($entry in ($PathValue -split ";")) {
            if (Test-SamePath -Left $entry.Trim() -Right $Directory) {
                return $true
            }
        }
        return $false
    }

    function Test-PathIsFirst {
        param(
            [AllowNull()]
            [string] $PathValue,
            [string] $Directory
        )

        if ([string]::IsNullOrWhiteSpace($PathValue)) {
            return $false
        }

        foreach ($entry in ($PathValue -split ";")) {
            $trimmed = $entry.Trim()
            if ([string]::IsNullOrWhiteSpace($trimmed)) {
                continue
            }
            return (Test-SamePath -Left $trimmed -Right $Directory)
        }
        return $false
    }

    function Get-PathWithDirectoryFirst {
        param(
            [AllowNull()]
            [string] $PathValue,
            [string] $Directory
        )

        $parts = New-Object System.Collections.Generic.List[string]
        if (-not [string]::IsNullOrWhiteSpace($PathValue)) {
            foreach ($entry in ($PathValue -split ";")) {
                $trimmed = $entry.Trim()
                if ([string]::IsNullOrWhiteSpace($trimmed)) {
                    continue
                }
                if (-not (Test-SamePath -Left $trimmed -Right $Directory)) {
                    $parts.Add($trimmed)
                }
            }
        }
        if ($parts.Count -eq 0) {
            return $Directory
        }
        return "$Directory;" + ($parts -join ";")
    }

    function Add-ToUserPath {
        param([string] $Directory)

        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        $userPathChanged = $false
        if (-not (Test-PathIsFirst -PathValue $userPath -Directory $Directory)) {
            $newUserPath = Get-PathWithDirectoryFirst -PathValue $userPath -Directory $Directory
            [Environment]::SetEnvironmentVariable("Path", $newUserPath, "User")
            $userPathChanged = $true
        }

        # Make the command available immediately when install.ps1 is evaluated
        # in the caller's current PowerShell process. Do this before Get-Command
        # "entire", which caches the first Application hit for the session.
        if (-not (Test-PathIsFirst -PathValue $env:Path -Directory $Directory)) {
            $env:Path = Get-PathWithDirectoryFirst -PathValue $env:Path -Directory $Directory
        }

        return $userPathChanged
    }

    function Get-EntireOnPath {
        # -All is required: without it Get-Command returns only the first
        # Application, so a later Scoop shim is never seen.
        # Write-Output with the unary comma prevents PowerShell from unrolling
        # a single-element array, which would lose .Count under StrictMode 2.0.
        $results = @(Get-Command "entire" -CommandType Application -All -ErrorAction SilentlyContinue)
        Write-Output -NoEnumerate $results
    }

    function Install-Entire {
        if ($ShowHelp) {
            Write-Usage
            return
        }

        Write-Info "Installing Entire CLI..."

        $scoopCommand = Get-Command "scoop" -ErrorAction SilentlyContinue
        if ($SelectedChannel -eq "stable" -and $null -ne $scoopCommand) {
            # Scoop owns the install location and the shims directory it puts on
            # PATH, so -InstallDir and -NoPathUpdate do not apply on this branch.
            # Write-Usage says so; keep the two in step if this changes.
            Install-EntireWithScoop

            $prefixResult = Invoke-Scoop prefix entire
            if ($prefixResult.ExitCode -ne 0) {
                throw "Scoop installed Entire CLI, but its installation path could not be resolved."
            }
            $scoopPrefix = ($prefixResult.Output | Select-Object -First 1).ToString().Trim()
            $scoopRoot = Split-Path -Parent (Split-Path -Parent (Split-Path -Parent $scoopPrefix))
            $scoopShim = Join-Path $scoopRoot "shims\entire.exe"

            $pathCommands = Get-EntireOnPath
            $first = $pathCommands | Select-Object -First 1
            if ($null -eq $first -or -not (Test-SamePath -Left $first.Source -Right $scoopShim)) {
                Write-Host ""
                Write-Host "! WARNING: PATH conflict detected" -ForegroundColor Yellow
                Write-Host "!"
                Write-Host "! Scoop shim: $scoopShim"
                if ($null -eq $first) {
                    Write-Host "! 'entire' does not resolve to an executable on PATH."
                }
                else {
                    Write-Host "! 'entire' currently resolves to: $($first.Source)"
                }
                Write-Host "! Remove the old installation or adjust PATH to prioritize:"
                Write-Host "!   $(Split-Path -Parent $scoopShim)"
                Write-Host ""
                throw "Scoop installed Entire CLI, but its shim does not take priority on PATH."
            }

            $conflicting = @($pathCommands | Where-Object { -not (Test-SamePath -Left $_.Source -Right $scoopShim) })
            if ($conflicting.Count -gt 0) {
                Write-Host ""
                Write-Host "! WARNING: Other Entire CLI installations remain on PATH" -ForegroundColor Yellow
                foreach ($cmd in $conflicting) {
                    Write-Host "! Also found: $($cmd.Source)"
                }
                Write-Host "! The Scoop shim takes priority, but consider removing the other installation."
                Write-Host ""
            }
            return
        }
        if ($SelectedChannel -eq "nightly" -and $null -ne $scoopCommand) {
            Write-InstallerWarning "Scoop only publishes stable releases; installing nightly from the verified release archive."
        }

        $resolvedInstallDir = Get-NormalizedPath -Path $SelectedInstallDir
        $installPath = Join-Path $resolvedInstallDir "entire.exe"

        # GitHub requires TLS 1.2. Use the numeric value so this remains valid
        # on .NET versions whose SecurityProtocolType enum omits the name.
        [Net.ServicePointManager]::SecurityProtocol =
            [Net.ServicePointManager]::SecurityProtocol -bor 3072

        $architecture = Get-PlatformArchitecture
        Write-Info "Detected platform: windows/$architecture"

        Write-Info "Fetching latest $SelectedChannel version..."
        $version = Get-ReleaseVersion -ReleaseChannel $SelectedChannel
        Write-Info "Installing version: $version"

        $archiveName = "entire_windows_$architecture.zip"
        $releaseBaseUrl = "https://github.com/$GitHubRepo/releases/download/v$version"
        $downloadUrl = "$releaseBaseUrl/$archiveName"
        $checksumsUrl = "$releaseBaseUrl/checksums.txt"

        $tempDir = Join-Path ([IO.Path]::GetTempPath()) ("entire-install-" + [guid]::NewGuid().ToString("N"))
        New-Item -ItemType Directory -Path $tempDir | Out-Null

        try {
            $archivePath = Join-Path $tempDir $archiveName
            $checksumsPath = Join-Path $tempDir "checksums.txt"
            $extractDir = Join-Path $tempDir "extracted"

            Write-Info "Downloading $archiveName..."
            Save-RemoteFile -Uri $downloadUrl -Destination $archivePath

            Write-Info "Downloading checksums..."
            Save-RemoteFile -Uri $checksumsUrl -Destination $checksumsPath

            Write-Info "Verifying checksum..."
            Assert-Checksum -ArchivePath $archivePath -ArchiveName $archiveName -ChecksumsPath $checksumsPath
            Write-Success "Checksum verified"

            Write-Info "Extracting..."
            Expand-Archive -LiteralPath $archivePath -DestinationPath $extractDir -Force

            $sourceBinary = Join-Path $extractDir "entire.exe"
            $sourceHelper = Join-Path $extractDir "git-remote-entire.exe"
            if (-not (Test-Path -LiteralPath $sourceBinary -PathType Leaf)) {
                throw "entire.exe was not found in $archiveName."
            }

            Write-Info "Installing to $resolvedInstallDir..."
            New-Item -ItemType Directory -Path $resolvedInstallDir -Force | Out-Null

            Copy-Item -LiteralPath $sourceBinary -Destination $installPath -Force

            if (Test-Path -LiteralPath $sourceHelper -PathType Leaf) {
                Copy-Item -LiteralPath $sourceHelper -Destination (Join-Path $resolvedInstallDir "git-remote-entire.exe") -Force
            }
            else {
                Write-InstallerWarning "git-remote-entire.exe was not found in the archive; entire:// clones will not work until the next release includes it."
            }

            & $installPath version *> $null
            if ($LASTEXITCODE -ne 0) {
                throw "Installation completed, but entire.exe failed to execute."
            }
            Write-Success "Entire CLI installed to $installPath"

            # Prepend PATH before Get-Command "entire". Checking first throws on
            # the documented nightly path (Scoop stable already installed) and
            # never updates PATH, so a rerun fails the same way.
            $userPathChanged = $false
            if (-not $SkipPathUpdate) {
                $userPathChanged = Add-ToUserPath -Directory $resolvedInstallDir
            }

            $pathCommands = Get-EntireOnPath
            $conflicting = @($pathCommands | Where-Object { -not (Test-SamePath -Left $_.Source -Right $installPath) })
            if ($conflicting.Count -gt 0) {
                $first = $pathCommands | Select-Object -First 1
                $firstIsOurs = Test-SamePath -Left $first.Source -Right $installPath
                Write-Host ""
                Write-Host "! WARNING: PATH conflict detected" -ForegroundColor Yellow
                Write-Host "!"
                Write-Host "! Installed to: $installPath"
                foreach ($cmd in $conflicting) {
                    Write-Host "! Also found:   $($cmd.Source)"
                }

                # Our PATH write only ever lands in the user half, and Windows
                # composes a new session as machine-then-user. So a conflict on
                # the machine PATH outranks this install in every new terminal,
                # whatever the current session resolves to -- say so, because
                # neither the verdict below nor reordering the user PATH can fix
                # it.
                $machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
                foreach ($cmd in $conflicting) {
                    if (Test-PathContains -PathValue $machinePath -Directory (Split-Path -Parent $cmd.Source)) {
                        Write-Host "!"
                        Write-Host "! $($cmd.Source) is on the machine-wide PATH, which Windows places"
                        Write-Host "! ahead of your user PATH. It will win in a new terminal even though"
                        Write-Host "! this session now resolves to the new install. Remove it, or run"
                        Write-Host "! entire from $resolvedInstallDir explicitly."
                    }
                }

                if (-not $firstIsOurs) {
                    Write-Host "!"
                    Write-Host "! 'entire' currently resolves to: $($first.Source)"
                    Write-Host "! Remove the old installation or adjust PATH to prioritize:"
                    Write-Host "!   $resolvedInstallDir"
                    Write-Host ""
                    if ($SkipPathUpdate) {
                        throw "Installation completed, but PATH was not updated (-NoPathUpdate)."
                    }
                    throw "Installation completed, but PATH needs adjustment."
                }
                Write-Host "!"
                Write-Host "! The installed version takes priority, but consider removing"
                Write-Host "! the other installation to avoid confusion."
                Write-Host ""
            }

            if ($SkipPathUpdate) {
                if (-not (Test-PathContains -PathValue $env:Path -Directory $resolvedInstallDir)) {
                    Write-InstallerWarning "$resolvedInstallDir is not on PATH. Add it before running entire."
                }
            }
            elseif ($userPathChanged) {
                Write-Success "Added $resolvedInstallDir to your user PATH"
                Write-Host "Restart your terminal, then run entire to get started."
            }
        }
        finally {
            Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }

    try {
        Install-Entire
    }
    catch {
        $message = "Error: $($_.Exception.Message)"
        if ($InvokedFromFile) {
            Write-Host $message -ForegroundColor Red
            exit 1
        }
        # irm | iex: throw without Write-Host so the message appears once, and
        # do not exit 1 — that would close the user's interactive shell.
        throw $message
    }
} $Channel $InstallDir $NoPathUpdate.IsPresent $Help.IsPresent (-not [string]::IsNullOrEmpty($MyInvocation.MyCommand.Path))
