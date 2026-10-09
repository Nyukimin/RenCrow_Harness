param(
    [string]$WorkingDirectory = ".",
    [string[]]$Step = @(),
    [string]$ExecutionPlan = "",
    [switch]$KeepRuntime,
    [switch]$SelfTest,
    [Parameter(Position = 0)]
    [string]$FilePath,
    [Parameter(Position = 1, ValueFromRemainingArguments = $true)]
    [string[]]$ArgumentList = @()
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$runtimeRoot = Join-Path $repoRoot ([IO.Path]::Combine("Tmp", "test-runtime"))
$runsRoot = Join-Path $runtimeRoot "_runs"
$cacheRoot = Join-Path $runtimeRoot "_cache"
$runName = "{0}-{1}" -f $PID, ([Guid]::NewGuid().ToString("N").Substring(0, 8))
$runRoot = Join-Path $runsRoot $runName
$sourceCandidateBootstrapDirectory = Join-Path $runRoot "source-candidate-bootstrap"
$script:sourceCandidateBootstrapLogPath = $null
$script:sourceCandidateBootstrapFileReady = $false
$sourceCandidateLogDirectory = Join-Path $runRoot "source-candidate-logs"
$script:sourceCandidateLogsReady = $false
$script:preserveSourceCandidateLogs = $false

$comparison = if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
    [StringComparison]::OrdinalIgnoreCase
} else {
    [StringComparison]::Ordinal
}

function Get-DirectoryPrefix([string]$Path) {
    return [IO.Path]::GetFullPath($Path).TrimEnd(
        [IO.Path]::DirectorySeparatorChar,
        [IO.Path]::AltDirectorySeparatorChar
    ) + [IO.Path]::DirectorySeparatorChar
}

$repoPrefix = Get-DirectoryPrefix $repoRoot
$localTempPrefix = Get-DirectoryPrefix (Join-Path $repoRoot "Tmp")
$planPath = Join-Path $PSScriptRoot "test-local.plan.json"

$candidateEnvironmentNames = @(
    "RENCROW_SOURCE_CANDIDATE_WORKSPACE",
    "RENCROW_SOURCE_CANDIDATE_ROOT",
    "RENCROW_SOURCE_CANDIDATE_MANIFEST",
    "RENCROW_SOURCE_CANDIDATE_RUNS",
    "RENCROW_SOURCE_CANDIDATE_ID"
)

function Assert-PrivateWindowsBootstrapObject([string]$Path, [bool]$IsDirectory, [string]$ExpectedOwnerSID) {
    $item = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    $isReparsePoint = ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0
    if ($isReparsePoint -or $item.PSIsContainer -ne $IsDirectory) {
        throw "SOURCE_CANDIDATE_BOOTSTRAP_PRIVATE_PATH_INVALID"
    }
    $acl = Get-Acl -LiteralPath $Path -ErrorAction Stop
    $ownerSID = $acl.GetOwner([Security.Principal.SecurityIdentifier]).Value
    if ($ownerSID -ne $ExpectedOwnerSID -or -not $acl.AreAccessRulesProtected) {
        throw "SOURCE_CANDIDATE_BOOTSTRAP_PRIVATE_ACL_INVALID"
    }
    $hasOwnerFullControl = $false
    $requiredInheritance = [Security.AccessControl.InheritanceFlags]::ContainerInherit -bor [Security.AccessControl.InheritanceFlags]::ObjectInherit
    foreach ($rule in $acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])) {
        if ($rule.AccessControlType -ne [Security.AccessControl.AccessControlType]::Allow -or
            [int]$rule.FileSystemRights -eq 0) {
            continue
        }
        if ($rule.IdentityReference.Value -ne $ExpectedOwnerSID) {
            throw "SOURCE_CANDIDATE_BOOTSTRAP_PRIVATE_ACL_INVALID"
        }
        if ($IsDirectory -and
            (($rule.InheritanceFlags -band $requiredInheritance) -eq $requiredInheritance) -and
            (($rule.FileSystemRights -band [Security.AccessControl.FileSystemRights]::FullControl) -eq [Security.AccessControl.FileSystemRights]::FullControl)) {
            $hasOwnerFullControl = $true
        }
        if (-not $IsDirectory -and
            (($rule.FileSystemRights -band [Security.AccessControl.FileSystemRights]::FullControl) -eq [Security.AccessControl.FileSystemRights]::FullControl)) {
            $hasOwnerFullControl = $true
        }
    }
    if (-not $hasOwnerFullControl) {
        throw "SOURCE_CANDIDATE_BOOTSTRAP_PRIVATE_ACL_INVALID"
    }
}

function Initialize-PrivateSourceCandidateBootstrapType {
    if ($null -ne ("RenCrowSourceCandidateBootstrapNative" -as [type])) {
        return
    }
    Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;

public static class RenCrowSourceCandidateBootstrapNative {
    [StructLayout(LayoutKind.Sequential)]
    private struct SecurityAttributes {
        public int Length;
        public IntPtr SecurityDescriptor;
        [MarshalAs(UnmanagedType.Bool)] public bool InheritHandle;
    }

    [DllImport("advapi32.dll", EntryPoint = "ConvertStringSecurityDescriptorToSecurityDescriptorW", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern bool ConvertStringSecurityDescriptorToSecurityDescriptor(string descriptor, uint revision, out IntPtr securityDescriptor, out uint size);
    [DllImport("kernel32.dll", EntryPoint = "CreateDirectoryW", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern bool CreateDirectoryWithSecurity(string path, ref SecurityAttributes attributes);
    [DllImport("kernel32.dll", EntryPoint = "CreateFileW", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern IntPtr CreateFileWithSecurity(string path, uint access, uint share, ref SecurityAttributes attributes, uint creation, uint flags, IntPtr template);
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool CloseHandle(IntPtr handle);
    [DllImport("kernel32.dll")]
    private static extern IntPtr LocalFree(IntPtr memory);

    private static bool BuildAttributes(string sddl, out IntPtr descriptor, out SecurityAttributes attributes, out int error) {
        uint size;
        if (!ConvertStringSecurityDescriptorToSecurityDescriptor(sddl, 1, out descriptor, out size)) {
            error = Marshal.GetLastWin32Error();
            attributes = default(SecurityAttributes);
            return false;
        }
        attributes = new SecurityAttributes();
        attributes.Length = Marshal.SizeOf(typeof(SecurityAttributes));
        attributes.SecurityDescriptor = descriptor;
        attributes.InheritHandle = false;
        error = 0;
        return true;
    }

    public static int CreatePrivateDirectory(string path, string ownerSid) {
        IntPtr descriptor;
        SecurityAttributes attributes;
        int error;
        string sddl = "O:" + ownerSid + "D:P(A;OICI;FA;;;" + ownerSid + ")";
        if (!BuildAttributes(sddl, out descriptor, out attributes, out error)) return error;
        try {
            if (CreateDirectoryWithSecurity(path, ref attributes)) return 0;
            return Marshal.GetLastWin32Error();
        } finally {
            LocalFree(descriptor);
        }
    }

    public static int CreatePrivateFile(string path, string ownerSid) {
        IntPtr descriptor;
        SecurityAttributes attributes;
        int error;
        string sddl = "O:" + ownerSid + "D:P(A;;FA;;;" + ownerSid + ")";
        if (!BuildAttributes(sddl, out descriptor, out attributes, out error)) return error;
        try {
            IntPtr handle = CreateFileWithSecurity(path, 0xC0000000, 0x00000007, ref attributes, 1, 0x00000080, IntPtr.Zero);
            if (handle == new IntPtr(-1)) return Marshal.GetLastWin32Error();
            return CloseHandle(handle) ? 0 : Marshal.GetLastWin32Error();
        } finally {
            LocalFree(descriptor);
        }
    }
}
'@ -ErrorAction Stop | Out-Null
}

function New-PrivateSourceCandidateBootstrapDirectory([string]$Path) {
    if (Test-Path -LiteralPath $Path) {
        throw "SOURCE_CANDIDATE_BOOTSTRAP_PRIVATE_PATH_NOT_FRESH"
    }
    if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
        Initialize-PrivateSourceCandidateBootstrapType
        $ownerSID = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
        if ([string]::IsNullOrWhiteSpace($ownerSID) -or
            [RenCrowSourceCandidateBootstrapNative]::CreatePrivateDirectory($Path, $ownerSID) -ne 0) {
            throw "SOURCE_CANDIDATE_BOOTSTRAP_PRIVATE_DIRECTORY_CREATE_FAILED"
        }
        Assert-PrivateWindowsBootstrapObject $Path $true $ownerSID
        return
    }
    $ownerOnlyMode = [IO.UnixFileMode]::UserRead -bor [IO.UnixFileMode]::UserWrite -bor [IO.UnixFileMode]::UserExecute
    [void][IO.Directory]::CreateDirectory($Path, $ownerOnlyMode)
    $item = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or
        [IO.File]::GetUnixFileMode($Path) -ne $ownerOnlyMode) {
        throw "SOURCE_CANDIDATE_BOOTSTRAP_PRIVATE_DIRECTORY_INVALID"
    }
}

function New-PrivateSourceCandidateBootstrapFile([string]$Path) {
    if (Test-Path -LiteralPath $Path) {
        throw "SOURCE_CANDIDATE_BOOTSTRAP_PRIVATE_PATH_NOT_FRESH"
    }
    if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
        $ownerSID = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
        if ([string]::IsNullOrWhiteSpace($ownerSID) -or
            [RenCrowSourceCandidateBootstrapNative]::CreatePrivateFile($Path, $ownerSID) -ne 0) {
            throw "SOURCE_CANDIDATE_BOOTSTRAP_PRIVATE_FILE_CREATE_FAILED"
        }
        Assert-PrivateWindowsBootstrapObject $Path $false $ownerSID
        return
    }
    $options = [IO.FileStreamOptions]::new()
    $options.Mode = [IO.FileMode]::CreateNew
    $options.Access = [IO.FileAccess]::ReadWrite
    $options.Share = [IO.FileShare]::ReadWrite -bor [IO.FileShare]::Delete
    $options.UnixCreateMode = [IO.UnixFileMode]::UserRead -bor [IO.UnixFileMode]::UserWrite
    $file = [IO.File]::Open($Path, $options)
    try {
        $file.Dispose()
    } finally {
        $file = $null
    }
    $item = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or
        [IO.File]::GetUnixFileMode($Path) -ne ([IO.UnixFileMode]::UserRead -bor [IO.UnixFileMode]::UserWrite)) {
        throw "SOURCE_CANDIDATE_BOOTSTRAP_PRIVATE_FILE_INVALID"
    }
}

function Initialize-SourceCandidateLogDirectory {
    if ($script:sourceCandidateLogsReady) {
        return
    }
    try {
        New-PrivateSourceCandidateBootstrapDirectory $sourceCandidateBootstrapDirectory
        $script:sourceCandidateBootstrapLogPath = Join-Path $sourceCandidateBootstrapDirectory "prepare-logs.log"
        New-PrivateSourceCandidateBootstrapFile $script:sourceCandidateBootstrapLogPath
        $script:sourceCandidateBootstrapFileReady = $true
        $exitCode = $null
        $locationPushed = $false
        try {
            Push-Location $repoRoot
            $locationPushed = $true
            $global:LASTEXITCODE = 0
            $null = @(& go run ./tools/source_candidate prepare-logs --owner-runs $runsRoot --directory $sourceCandidateLogDirectory 2> $script:sourceCandidateBootstrapLogPath)
            $exitCode = $LASTEXITCODE
        } finally {
            if ($locationPushed) {
                Pop-Location
            }
        }
        if ($exitCode -ne 0) {
            Add-Content -LiteralPath $script:sourceCandidateBootstrapLogPath -Value "status=failed action=prepare-logs evidence=bootstrap-prepare-logs" -Encoding utf8
            $script:preserveSourceCandidateLogs = $true
            throw "SOURCE_CANDIDATE_LOG_SETUP_FAILED: evidence=bootstrap-prepare-logs"
        }
        Add-Content -LiteralPath $script:sourceCandidateBootstrapLogPath -Value "status=ready action=prepare-logs evidence=bootstrap-prepare-logs" -Encoding utf8
        $script:sourceCandidateLogsReady = $true
    } catch {
        $script:preserveSourceCandidateLogs = $true
        if ($script:sourceCandidateBootstrapFileReady) {
            try {
                $lastLine = Get-Content -LiteralPath $script:sourceCandidateBootstrapLogPath -Tail 1 -ErrorAction Stop
                if ($lastLine -notmatch '^status=(failed|ready) action=prepare-logs evidence=bootstrap-prepare-logs$') {
                    Add-Content -LiteralPath $script:sourceCandidateBootstrapLogPath -Value "status=failed action=prepare-logs evidence=bootstrap-prepare-logs" -Encoding utf8
                }
            } catch {
            }
        }
        throw "SOURCE_CANDIDATE_LOG_SETUP_FAILED: evidence=bootstrap-prepare-logs"
    }
}

function Invoke-SourceCandidateCommand([string]$Action, [string]$Workspace, [string]$Evidence) {
    if (($Action -ne "capture" -and $Action -ne "verify") -or $Evidence -notmatch '^[a-z0-9-]+$') {
        throw "SOURCE_CANDIDATE_FAILED: invalid command evidence label"
    }
    Initialize-SourceCandidateLogDirectory
    $logPath = Join-Path $sourceCandidateLogDirectory ("{0}-{1}-{2}.log" -f $runName, $Evidence, $Action)
    $exitCode = $null
    $status = "failed"
    try {
        $locationPushed = $false
        try {
            Push-Location $repoRoot
            $locationPushed = $true
            $global:LASTEXITCODE = 0
            if ($Action -eq "capture") {
                $output = @(& go run ./tools/source_candidate capture --source $repoRoot --owner-runs $runsRoot --workspace $Workspace 2> $logPath)
            } else {
                $output = @(& go run ./tools/source_candidate verify --owner-runs $runsRoot --workspace $Workspace 2> $logPath)
            }
            $exitCode = $LASTEXITCODE
        } finally {
            if ($locationPushed) {
                Pop-Location
            }
        }
        if ($exitCode -ne 0) {
            $status = "failed-exit-$exitCode"
            throw "source candidate command failed"
        }
        $result = ($output -join "`n").Trim()
        if ($result -notmatch '^status=candidate candidateId=([0-9a-f]{64}) entries=\d+$') {
            $status = "invalid-result"
            throw "source candidate result was invalid"
        }
        $candidateID = $Matches[1]
        $status = "candidate"
        Add-Content -LiteralPath $logPath -Value "status=$status action=$Action evidence=$Evidence" -Encoding utf8
        return $candidateID
    } catch {
        if ($status -eq "failed") {
            $status = if ($null -eq $exitCode) { "failed-launch" } else { "failed-exit-$exitCode" }
        }
        try {
            Add-Content -LiteralPath $logPath -Value "status=$status action=$Action evidence=$Evidence" -Encoding utf8
        } catch {
        }
        $script:preserveSourceCandidateLogs = $true
        throw "SOURCE_CANDIDATE_FAILED: $Action (evidence=$Evidence)"
    }
}

function Set-SourceCandidateEnvironment([string]$Workspace, [string]$CandidateID) {
    [Environment]::SetEnvironmentVariable("RENCROW_SOURCE_CANDIDATE_WORKSPACE", $Workspace, "Process")
    [Environment]::SetEnvironmentVariable("RENCROW_SOURCE_CANDIDATE_ROOT", (Join-Path $Workspace "candidate"), "Process")
    [Environment]::SetEnvironmentVariable("RENCROW_SOURCE_CANDIDATE_MANIFEST", (Join-Path $Workspace "manifest.json"), "Process")
    [Environment]::SetEnvironmentVariable("RENCROW_SOURCE_CANDIDATE_RUNS", $runsRoot, "Process")
    [Environment]::SetEnvironmentVariable("RENCROW_SOURCE_CANDIDATE_ID", $CandidateID, "Process")
}

function Assert-SourceCandidateEnvironment([string]$ExpectedCandidateID, [string]$Evidence) {
    $workspace = [Environment]::GetEnvironmentVariable("RENCROW_SOURCE_CANDIDATE_WORKSPACE", "Process")
    $candidate = [Environment]::GetEnvironmentVariable("RENCROW_SOURCE_CANDIDATE_ROOT", "Process")
    $manifest = [Environment]::GetEnvironmentVariable("RENCROW_SOURCE_CANDIDATE_MANIFEST", "Process")
    $ownerRuns = [Environment]::GetEnvironmentVariable("RENCROW_SOURCE_CANDIDATE_RUNS", "Process")
    $candidateID = [Environment]::GetEnvironmentVariable("RENCROW_SOURCE_CANDIDATE_ID", "Process")
    if ([string]::IsNullOrWhiteSpace($workspace) -or
        [string]::IsNullOrWhiteSpace($candidate) -or
        [string]::IsNullOrWhiteSpace($manifest) -or
        [string]::IsNullOrWhiteSpace($ownerRuns) -or
        [string]::IsNullOrWhiteSpace($candidateID) -or
        -not [string]::Equals([IO.Path]::GetFullPath($candidate), [IO.Path]::GetFullPath((Join-Path $workspace "candidate")), $comparison) -or
        -not [string]::Equals([IO.Path]::GetFullPath($manifest), [IO.Path]::GetFullPath((Join-Path $workspace "manifest.json")), $comparison) -or
        -not [string]::Equals([IO.Path]::GetFullPath($ownerRuns), [IO.Path]::GetFullPath($runsRoot), $comparison) -or
        $candidateID -ne $ExpectedCandidateID) {
        throw "SOURCE_CANDIDATE_FAILED: environment does not bind the expected candidate"
    }
    $verifiedID = Invoke-SourceCandidateCommand "verify" $workspace $Evidence
    if ($verifiedID -ne $ExpectedCandidateID) {
        throw "SOURCE_CANDIDATE_FAILED: manifest identity changed"
    }
    return $verifiedID
}

function Invoke-WithRestoredStepEnvironment(
    [hashtable]$Environment,
    [scriptblock]$Action,
    [scriptblock]$PostAction,
    [string]$PrependPath
) {
    $previousEnvironment = @{}
    foreach ($name in $Environment.Keys) {
        $previousEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, "Process")
    }
    $previousPath = [Environment]::GetEnvironmentVariable("PATH", "Process")
    try {
        foreach ($name in $Environment.Keys) {
            [Environment]::SetEnvironmentVariable($name, $Environment[$name], "Process")
        }
        if (-not [string]::IsNullOrWhiteSpace($PrependPath)) {
            $pathParts = @($previousPath -split [Regex]::Escape([IO.Path]::PathSeparator))
            if ($pathParts.Count -eq 0 -or -not $pathParts[0].Equals($PrependPath, $comparison)) {
                [Environment]::SetEnvironmentVariable(
                    "PATH",
                    $PrependPath + [IO.Path]::PathSeparator + $previousPath,
                    "Process"
                )
            }
        }
        try {
            & $Action
        } finally {
            if ($null -ne $PostAction) {
                & $PostAction
            }
        }
    } finally {
        foreach ($name in $previousEnvironment.Keys) {
            [Environment]::SetEnvironmentVariable($name, $previousEnvironment[$name], "Process")
        }
        [Environment]::SetEnvironmentVariable("PATH", $previousPath, "Process")
    }
}

$outerCandidateWorkspace = $null
$outerEnvironment = @{}
$outerCandidateEnvironment = @{}
$outerEnvironmentActive = $false
try {
    if (-not [string]::IsNullOrWhiteSpace($ExecutionPlan)) {
        if ($Step.Count -gt 0 -or $SelfTest -or -not [string]::IsNullOrWhiteSpace($FilePath)) {
            throw "ExecutionPlan cannot be combined with Step, SelfTest, or FilePath."
        }
        if (Test-Path -LiteralPath $runRoot) {
            throw "Owner runtime run directory is not fresh."
        }
        $outerPaths = @{
            TEMP = $runRoot
            TMP = $runRoot
            TMPDIR = $runRoot
            GOTMPDIR = (Join-Path $runRoot "_go-build")
            GOCACHE = (Join-Path $cacheRoot "go-build")
            GOMODCACHE = (Join-Path $cacheRoot "go-mod")
        }
        $outerEnvironmentActive = $true
        foreach ($name in $outerPaths.Keys) {
            $outerEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, "Process")
            New-Item -ItemType Directory -Force -Path $outerPaths[$name] | Out-Null
            [Environment]::SetEnvironmentVariable($name, $outerPaths[$name], "Process")
        }
        foreach ($name in $outerPaths.Keys) {
            $resolved = [IO.Path]::GetFullPath($outerPaths[$name])
            if (-not $resolved.StartsWith($localTempPrefix, $comparison)) {
                throw "$name escaped the repository-local Tmp directory."
            }
        }
        $outerCandidateWorkspace = Join-Path $runRoot "source-work"
        $candidateID = Invoke-SourceCandidateCommand "capture" $outerCandidateWorkspace "execution-plan-capture"
        foreach ($name in $candidateEnvironmentNames) {
            $outerCandidateEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, "Process")
        }
        Set-SourceCandidateEnvironment $outerCandidateWorkspace $candidateID
        Write-Host "[test-local] source candidate: status=candidate candidateId=$candidateID"
    }

# BEGIN GENERATED test-impact:impact-entry
# Impact selection is owned by RenCrow_Tools; this owner runner retains command
# definitions, complete test registration checks, and isolated child environment.
if (-not [string]::IsNullOrWhiteSpace($ExecutionPlan)) {
    if ($Step.Count -gt 0 -or $SelfTest -or -not [string]::IsNullOrWhiteSpace($FilePath)) {
        throw "ExecutionPlan cannot be combined with Step, SelfTest, or FilePath."
    }
    $impactCommand = if ($env:RENCROW_TEST_IMPACT_BIN) { $env:RENCROW_TEST_IMPACT_BIN } else { "rencrow-test-impact" }
    $hostExecutable = (Get-Process -Id $PID).Path
    & $impactCommand run --repo $repoRoot --plan $planPath --execution-plan $ExecutionPlan --pwsh $hostExecutable
    if ($LASTEXITCODE -ne 0) { throw "Test Impact execution failed with exit code $LASTEXITCODE" }
    return
}
$testPlatform = if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) { "windows" }
    elseif ($PSVersionTable.ContainsKey("OS") -and $PSVersionTable.OS -match "Darwin") { "darwin" }
    else { "linux" }
# END GENERATED test-impact:impact-entry
} finally {
    try {
        if ($null -ne $outerCandidateWorkspace -and -not $KeepRuntime -and -not $script:preserveSourceCandidateLogs -and (Test-Path -LiteralPath $runRoot)) {
            $resolvedRunRoot = [IO.Path]::GetFullPath($runRoot)
            $runsPrefix = Get-DirectoryPrefix $runsRoot
            if (-not $resolvedRunRoot.StartsWith($runsPrefix, $comparison)) {
                throw "Refusing to clean a path outside the repository test runtime."
            }
            Remove-Item -LiteralPath $runRoot -Recurse -Force
        }
    } finally {
        if ($outerEnvironmentActive) {
            foreach ($name in $outerEnvironment.Keys) {
                [Environment]::SetEnvironmentVariable($name, $outerEnvironment[$name], "Process")
            }
        }
        foreach ($name in $outerCandidateEnvironment.Keys) {
            [Environment]::SetEnvironmentVariable($name, $outerCandidateEnvironment[$name], "Process")
        }
    }
}


function Assert-TestRuntimeLayout {
    if (-not (Test-Path -LiteralPath $runtimeRoot -PathType Container)) {
        return
    }

    # Go ignores directory names beginning with '_' or '.', so only a .go
    # file below a normal-name runtime directory can become a package during
    # `go list ./...`. Never remove an unknown stale tree here: fail closed
    # and report the exact paths for an operator-owned cleanup decision.
    $legacyGeneratedFiles = [Collections.Generic.List[object]]::new()
    $pendingDirectories = [Collections.Generic.Stack[string]]::new()
    $pendingDirectories.Push($runtimeRoot)
    while ($pendingDirectories.Count -gt 0) {
        $directory = $pendingDirectories.Pop()
        $children = @(Get-ChildItem -LiteralPath $directory -Force -ErrorAction SilentlyContinue)
        foreach ($child in $children) {
            if ($child.PSIsContainer) {
                if ($child.Name.StartsWith("_", [StringComparison]::Ordinal) -or $child.Name.StartsWith(".", [StringComparison]::Ordinal)) {
                    continue
                }
                $isReparsePoint = (($child.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0)
                if (-not $isReparsePoint -and $child.PSObject.Properties.Name -contains "LinkType") {
                    $isReparsePoint = -not [string]::IsNullOrWhiteSpace([string]$child.LinkType)
                }
                if (-not $isReparsePoint) {
                    $pendingDirectories.Push($child.FullName)
                }
                continue
            }
            if ($child.Extension -ne ".go") {
                continue
            }
            $relative = $child.FullName.Substring($runtimeRoot.Length).TrimStart([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
            $segments = $relative.Replace("\", "/").Split("/", [StringSplitOptions]::RemoveEmptyEntries)
            if (-not ($segments | Where-Object { $_.StartsWith("_", [StringComparison]::Ordinal) -or $_.StartsWith(".", [StringComparison]::Ordinal) })) {
                [void]$legacyGeneratedFiles.Add($child)
            }
        }
    }
    if ($legacyGeneratedFiles.Count -gt 0) {
        $reported = @($legacyGeneratedFiles | Select-Object -First 20 | ForEach-Object {
            $_.FullName.Substring($repoRoot.Length).TrimStart([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
        })
        $suffix = if ($legacyGeneratedFiles.Count -gt $reported.Count) {
            " (and $($legacyGeneratedFiles.Count - $reported.Count) more)"
        } else {
            ""
        }
        throw "TEST_RUNTIME_LAYOUT_INVALID: generated .go exists below a traversable Tmp/test-runtime directory: $($reported -join ', ')$suffix"
    }
}

Assert-TestRuntimeLayout

function Resolve-WorkingPath([string]$Candidate) {
    $resolved = if ([IO.Path]::IsPathRooted($Candidate)) {
        [IO.Path]::GetFullPath($Candidate)
    } else {
        [IO.Path]::GetFullPath((Join-Path $repoRoot $Candidate))
    }
    if ($resolved -ne $repoRoot -and -not $resolved.StartsWith($repoPrefix, $comparison)) {
        throw "WorkingDirectory must stay inside the repository: $resolved"
    }
    if (-not (Test-Path -LiteralPath $resolved -PathType Container)) {
        throw "WorkingDirectory does not exist: $resolved"
    }
    return $resolved
}

function Resolve-TestExecutable([string]$Candidate) {
# BEGIN GENERATED test-impact:runtime-executable
    if ($Candidate -eq "pwsh") {
        return (Get-Process -Id $PID).Path
    }
    if ($Candidate -eq "python" -and $null -eq (Get-Command python -CommandType Application -ErrorAction SilentlyContinue)) {
        $python3 = Get-Command python3 -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($null -ne $python3) { return $python3.Source }
    }
# END GENERATED test-impact:runtime-executable
    if ($Candidate -eq "bash" -and [Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
        $bashCommand = Get-Command bash -ErrorAction SilentlyContinue
        if ($null -ne $bashCommand) {
            return $bashCommand.Source
        }
        $gitBash = Join-Path $env:ProgramFiles "Git\bin\bash.exe"
        if (Test-Path -LiteralPath $gitBash -PathType Leaf) {
            return $gitBash
        }
    }
    return $Candidate
}

function Test-IsTrackedTestFile([string]$RelativePath) {
    $normalizedPath = $RelativePath.Replace("\", "/")
    $fileName = [IO.Path]::GetFileName($RelativePath)
    $isHarnessDesignValidator = $normalizedPath -match "^tools/speccheck/validate_[^/]+\.py$"
    $isTestDirectoryCode = (
        $normalizedPath -match "(^|/)(test|tests|scripts/tests)/" -and
        $fileName -match "\.(go|py|[cm]?js|sh)$"
    )
    return (
        $isHarnessDesignValidator -or
        $isTestDirectoryCode -or
        $fileName -match "_test\.go$" -or
        $fileName -match "^test_.*\.py$" -or
        $fileName -match "_test\.py$" -or
        $fileName -match "^test_.*\.[cm]?js$" -or
        $fileName -match "(\.test|_test)\.[cm]?js$" -or
        $fileName -match "_test\.ps1$" -or
        $fileName -match "_test\.sh$" -or
        $fileName -match "^test[-_].*\.sh$"
    )
}

function Test-MatchesTestFilePattern([string]$RelativePath, [string]$Pattern) {
    $normalizedPattern = $Pattern.Replace("\", "/")
    $wildcardOptions = if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
        [Management.Automation.WildcardOptions]::IgnoreCase
    } else {
        [Management.Automation.WildcardOptions]::None
    }
    $wildcard = [Management.Automation.WildcardPattern]::new(
        $normalizedPattern,
        $wildcardOptions
    )
    return $wildcard.IsMatch($RelativePath.Replace("\", "/"))
}

if ($SelfTest -and -not [string]::IsNullOrWhiteSpace($FilePath)) {
    throw "SelfTest cannot run a command. Put -- between FilePath and command arguments."
}
if ($Step.Count -gt 0 -and -not [string]::IsNullOrWhiteSpace($FilePath)) {
    throw "Step cannot be combined with an explicit FilePath."
}

$commands = @()
$testFilePatterns = @()
$trackedTestFiles = @()
if ([string]::IsNullOrWhiteSpace($FilePath)) {
    if (-not (Test-Path -LiteralPath $planPath -PathType Leaf)) {
        throw "Canonical test plan does not exist: $planPath"
    }
    $plan = Get-Content -LiteralPath $planPath -Raw | ConvertFrom-Json
    if ($plan.version -notin @(1, 2)) {
        throw "Unsupported canonical test plan version: $($plan.version)"
    }
    $allStepNames = @()
    foreach ($planStep in @($plan.steps)) {
        $name = [string]$planStep.name
        if ([string]::IsNullOrWhiteSpace($name)) {
            throw "Every canonical test step must have a name."
        }
        if ($allStepNames -contains $name) {
            throw "Canonical test step names must be unique: $name"
        }
        $allStepNames += $name

        $expectedSourceConsumer = if ($name -eq "test-runtime-layout") { "owner-runtime" } else { "candidate" }
        $sourceInput = [string]$planStep.environment.RENCROW_SOURCE_CANDIDATE_INPUT
        $sourceConsumer = [string]$planStep.environment.RENCROW_SOURCE_CANDIDATE_CONSUMER
        $sourceFailureAction = [string]$planStep.environment.RENCROW_SOURCE_CANDIDATE_FAILURE_ACTION
        $sourceModePolicy = [string]$planStep.environment.RENCROW_SOURCE_CANDIDATE_MODE_POLICY
        $sourcePurpose = [string]$planStep.environment.RENCROW_SOURCE_CANDIDATE_PURPOSE
        if ($sourceInput -ne "public-source-root" -or
            $sourceConsumer -ne $expectedSourceConsumer -or
            $sourceFailureAction -ne "block" -or
            $sourceModePolicy -ne "tracked=git-index;unix-working-drift=recorded;windows-untracked=0644" -or
            $sourcePurpose -ne "share-one-immutable-source-across-checks") {
            throw "Canonical test step '$name' has an invalid source-preparation boundary contract."
        }

        if ($planStep.PSObject.Properties.Name -contains "testFiles") {
            foreach ($patternValue in @($planStep.testFiles)) {
                $pattern = ([string]$patternValue).Replace("\", "/")
                if ([string]::IsNullOrWhiteSpace($pattern)) {
                    throw "Canonical test step '$name' contains an empty testFiles pattern."
                }
                if ([IO.Path]::IsPathRooted($pattern) -or $pattern -match "(^|/)\.\.(/|$)") {
                    throw "Canonical test step '$name' contains an unsafe testFiles pattern: $pattern"
                }
                $testFilePatterns += $pattern
            }
        }

# BEGIN GENERATED test-impact:platform-filter
        if ($planStep.PSObject.Properties.Name -contains "platform" -and @($planStep.platform).Count -gt 0 -and @($planStep.platform) -notcontains $testPlatform) {
            if ($Step -contains $name) { throw "Canonical test step '$name' is unavailable on $testPlatform." }
            continue
        }
# END GENERATED test-impact:platform-filter

        if ($Step.Count -gt 0 -and $Step -notcontains $name) {
            continue
        }

        $environment = @{}
        if ($planStep.PSObject.Properties.Name -contains "environment") {
            foreach ($property in $planStep.environment.PSObject.Properties) {
                $environment[$property.Name] = [string]$property.Value
            }
        }
        $arguments = @()
        if ($planStep.PSObject.Properties.Name -contains "arguments") {
            $arguments = @($planStep.arguments | ForEach-Object { [string]$_ })
        }
        $stepWorkingDirectory = "."
        if ($planStep.PSObject.Properties.Name -contains "workingDirectory") {
            $stepWorkingDirectory = [string]$planStep.workingDirectory
        }
        $commands += [pscustomobject]@{
            Name             = $name
            WorkingDirectory = $stepWorkingDirectory
            FilePath         = [string]$planStep.filePath
            Arguments        = $arguments
            Environment      = $environment
            SourceConsumer   = $sourceConsumer
        }
    }
    foreach ($requestedStep in $Step) {
        if ($allStepNames -notcontains $requestedStep) {
            throw "Unknown canonical test step: $requestedStep"
        }
    }
    if ($commands.Count -eq 0) {
        throw "Canonical test plan contains no selected steps: $planPath"
    }

    $trackedFiles = @(git -C $repoRoot -c core.quotepath=false ls-files --cached --others --exclude-standard)
    if ($LASTEXITCODE -ne 0) {
        throw "git ls-files failed while checking canonical test coverage."
    }
    $trackedTestFiles = @($trackedFiles | ForEach-Object { $_.Replace("\", "/") } | Where-Object {
        (Test-Path -LiteralPath (Join-Path $repoRoot $_) -PathType Leaf) -and
        (Test-IsTrackedTestFile $_)
    })
    # A registered wildcard is not sufficient when node --test enumerates files.
    # Check the whole plan even when -Step selects another suite.
    foreach ($nodeStep in @($plan.steps)) {
        if ($nodeStep.filePath -ne "node" -or $nodeStep.arguments -notcontains "--test") { continue }
        if ($nodeStep.PSObject.Properties.Name -notcontains "testFiles") { continue }
        $nodeArgs = @($nodeStep.arguments | Where-Object { -not $_.StartsWith("-") })
        foreach ($candidate in $trackedTestFiles) {
            $claimed = @($nodeStep.testFiles | Where-Object { Test-MatchesTestFilePattern $candidate $_ }).Count -gt 0
            if (-not $claimed) { continue }
            $executed = @($nodeArgs | Where-Object { Test-MatchesTestFilePattern $candidate $_ }).Count -gt 0
            if (-not $executed) { throw "Canonical node step '$($nodeStep.name)' registers but does not execute: $candidate" }
        }
    }
    $uncoveredTestFiles = @($trackedTestFiles | Where-Object {
        $relativePath = $_
        -not ($testFilePatterns | Where-Object {
            Test-MatchesTestFilePattern $relativePath $_
        } | Select-Object -First 1)
    })
    if ($uncoveredTestFiles.Count -gt 0) {
        throw "Canonical test plan does not cover tracked test files: $($uncoveredTestFiles -join ', ')"
    }
    $unusedTestFilePatterns = @($testFilePatterns | Where-Object {
        $pattern = $_
        -not ($trackedTestFiles | Where-Object {
            Test-MatchesTestFilePattern $_ $pattern
        } | Select-Object -First 1)
    })
    if ($unusedTestFilePatterns.Count -gt 0) {
        throw "Canonical test plan contains testFiles patterns that match no tracked test: $($unusedTestFilePatterns -join ', ')"
    }
} else {
    $commands += [pscustomobject]@{
        Name             = "explicit"
        WorkingDirectory = $WorkingDirectory
        FilePath         = $FilePath
        Arguments        = @($ArgumentList)
        Environment      = @{}
        SourceConsumer   = "none"
    }
}

$paths = @{
    TEMP                  = $runRoot
    TMP                   = $runRoot
    TMPDIR                = $runRoot
    GOTMPDIR              = (Join-Path $runRoot "_go-build")
    GOCACHE               = (Join-Path $cacheRoot "go-build")
    GOMODCACHE            = (Join-Path $cacheRoot "go-mod")
    PYTHONPYCACHEPREFIX   = (Join-Path $cacheRoot "python-bytecode")
    PYTEST_DEBUG_TEMPROOT = (Join-Path $runRoot "pytest")
    UV_CACHE_DIR          = (Join-Path $cacheRoot "uv")
    PIP_CACHE_DIR         = (Join-Path $cacheRoot "pip")
    npm_config_cache      = (Join-Path $cacheRoot "npm")
    NODE_COMPILE_CACHE    = (Join-Path $cacheRoot "node-compile")
    PLAYWRIGHT_BROWSERS_PATH = (Join-Path $cacheRoot "playwright")
    XDG_CACHE_HOME        = (Join-Path $cacheRoot "xdg")
}

$protectedEnvironmentNames = @($paths.Keys + $candidateEnvironmentNames)
foreach ($command in $commands) {
    if ([string]::IsNullOrWhiteSpace($command.FilePath)) {
        throw "Canonical test step '$($command.Name)' must have a filePath."
    }
    [void](Resolve-WorkingPath $command.WorkingDirectory)
    foreach ($name in $command.Environment.Keys) {
        if ($protectedEnvironmentNames -contains $name) {
            throw "Canonical test step '$($command.Name)' cannot override protected environment variable $name."
        }
    }
}

$previous = @{}
$sourceCandidatePrevious = @{}
$sourceCandidateEnvironmentManaged = $false
$sourceCandidateWorkspace = $null
$sourceCandidateID = $null
foreach ($name in $paths.Keys) {
    $previous[$name] = [Environment]::GetEnvironmentVariable($name, "Process")
    New-Item -ItemType Directory -Force -Path $paths[$name] | Out-Null
    [Environment]::SetEnvironmentVariable($name, $paths[$name], "Process")
}
$previousTestPython = [Environment]::GetEnvironmentVariable("RENCROW_TEST_PYTHON", "Process")
$sentinelName = "RENCROW_TEST_ENV_MERGE_SENTINEL"
$previousSentinel = [Environment]::GetEnvironmentVariable($sentinelName, "Process")
$sentinelValue = "preserved-{0}" -f ([Guid]::NewGuid().ToString("N"))
[Environment]::SetEnvironmentVariable($sentinelName, $sentinelValue, "Process")
$pythonCommand = Get-Command python -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
if ($null -ne $pythonCommand) {
    $testPython = $pythonCommand.Source
    if ([IO.Path]::DirectorySeparatorChar -eq '\') {
        $testPython = $testPython.Replace('\', '/')
    }
    [Environment]::SetEnvironmentVariable("RENCROW_TEST_PYTHON", $testPython, "Process")
}

try {
    foreach ($name in $paths.Keys) {
        $resolved = [IO.Path]::GetFullPath($paths[$name])
        if (-not $resolved.StartsWith($localTempPrefix, $comparison)) {
            throw "$name escaped the repository-local Tmp directory: $resolved"
        }
    }

    Write-Host "[test-local] repository: $repoRoot"
    Write-Host "[test-local] runtime: $runRoot"
    Write-Host "[test-local] cache: $cacheRoot"

    if ($SelfTest) {
        if (-not (Split-Path -Leaf $runsRoot).StartsWith("_", [StringComparison]::Ordinal) -or
            -not (Split-Path -Leaf $cacheRoot).StartsWith("_", [StringComparison]::Ordinal) -or
            -not (Split-Path -Leaf $paths["GOTMPDIR"]).StartsWith("_", [StringComparison]::Ordinal)) {
            throw "Repository-local test runtime paths must use underscore-prefixed Go-traversal-safe directories."
        }
        $hostExecutable = (Get-Process -Id $PID).Path
        $probeNames = @($protectedEnvironmentNames + $sentinelName)
        $quotedNames = @($probeNames | ForEach-Object {
            '"{0}"' -f $_.Replace('"', '""')
        })
        $probeCode = '$names=@({0}); foreach ($name in $names) {{ [Console]::WriteLine([Environment]::GetEnvironmentVariable($name, "Process")) }}' -f ($quotedNames -join ",")
        $encodedProbe = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($probeCode))
        $childValues = @(& $hostExecutable -NoProfile -NonInteractive -EncodedCommand $encodedProbe)
        if ($LASTEXITCODE -ne 0 -or $childValues.Count -ne $probeNames.Count) {
            throw "Child-process environment probe failed."
        }
        for ($index = 0; $index -lt $protectedEnvironmentNames.Count; $index++) {
            $name = $protectedEnvironmentNames[$index]
            if ($paths.ContainsKey($name)) {
                if ([IO.Path]::GetFullPath($childValues[$index]) -ne [IO.Path]::GetFullPath($paths[$name])) {
                    throw "Child process did not inherit repository-local $name."
                }
            } elseif ([string]$childValues[$index] -ne [string][Environment]::GetEnvironmentVariable($name, "Process")) {
                throw "Child process did not inherit source-candidate identity $name."
            }
        }
        if ($childValues[$probeNames.Count - 1] -ne $sentinelValue) {
            throw "Child process environment was replaced instead of merged."
        }
        $bootstrapFlagName = "GOFLAGS"
        $bootstrapFlagPrevious = [Environment]::GetEnvironmentVariable($bootstrapFlagName, "Process")
        $bootstrapFlagEnvironment = @{}
        $bootstrapFlagEnvironment[$bootstrapFlagName] = "-gcflags=all=-definitely-not-a-go-compiler-flag"
        $bootstrapFailureAction = { Initialize-SourceCandidateLogDirectory }
        $expectedBootstrapFailure = $false
        try {
            Invoke-WithRestoredStepEnvironment $bootstrapFlagEnvironment $bootstrapFailureAction $null $null
        } catch {
            $expectedBootstrapFailure = $_.Exception.Message -eq "SOURCE_CANDIDATE_LOG_SETUP_FAILED: evidence=bootstrap-prepare-logs"
        }
        $bootstrapEnvironmentRestored = [string]::Equals(
            [string][Environment]::GetEnvironmentVariable($bootstrapFlagName, "Process"),
            [string]$bootstrapFlagPrevious,
            [StringComparison]::Ordinal
        )
        $bootstrapEvidencePath = $script:sourceCandidateBootstrapLogPath
        $bootstrapEvidenceText = ""
        try {
            if (-not [string]::IsNullOrWhiteSpace($bootstrapEvidencePath) -and
                (Test-Path -LiteralPath $bootstrapEvidencePath -PathType Leaf)) {
                $bootstrapEvidenceText = [IO.File]::ReadAllText($bootstrapEvidencePath)
            }
        } catch {
            $bootstrapEvidenceText = ""
        }
        $bootstrapEvidenceRetained = $bootstrapEvidenceText -match '(?i)definitely-not-a-go-compiler-flag' -and
            $bootstrapEvidenceText -match '(?m)^status=failed action=prepare-logs evidence=bootstrap-prepare-logs\r?$'
        if (-not $expectedBootstrapFailure -or -not $bootstrapEnvironmentRestored -or -not $bootstrapEvidenceRetained) {
            throw "Source-candidate bootstrap failure evidence or environment restoration failed."
        }
        Remove-Item -LiteralPath $sourceCandidateBootstrapDirectory -Recurse -Force
        $script:sourceCandidateBootstrapLogPath = $null
        $script:sourceCandidateBootstrapFileReady = $false
        $script:sourceCandidateLogsReady = $false
        $script:preserveSourceCandidateLogs = $false

        $expectedCandidateFailure = $false
        try {
            [void](Invoke-SourceCandidateCommand "verify" (Join-Path $runRoot "missing-source-work") "self-test-verify-failure")
        } catch {
            $expectedCandidateFailure = $_.Exception.Message -match "SOURCE_CANDIDATE_FAILED: verify \(evidence=self-test-verify-failure\)"
        }
        $evidencePath = Join-Path $sourceCandidateLogDirectory ("{0}-self-test-verify-failure-verify.log" -f $runName)
        if (-not $expectedCandidateFailure -or -not (Test-Path -LiteralPath $evidencePath -PathType Leaf) -or
            (Get-Item -LiteralPath $evidencePath).Length -eq 0 -or
            (Get-Content -LiteralPath $evidencePath -Tail 1) -ne "status=failed-exit-1 action=verify evidence=self-test-verify-failure") {
            throw "Source-candidate failure evidence was not retained with its relative status."
        }
        Remove-Item -LiteralPath $sourceCandidateLogDirectory -Recurse -Force
        Remove-Item -LiteralPath $sourceCandidateBootstrapDirectory -Recurse -Force
        $script:sourceCandidateBootstrapLogPath = $null
        $script:sourceCandidateBootstrapFileReady = $false
        $script:sourceCandidateLogsReady = $false
        $script:preserveSourceCandidateLogs = $false

        $restoreProbeName = "RENCROW_TEST_STEP_RESTORE_PROBE"
        $restoreProbePrevious = [Environment]::GetEnvironmentVariable($restoreProbeName, "Process")
        $restorePathBefore = [Environment]::GetEnvironmentVariable("PATH", "Process")
        $restoreProbeDirectory = Join-Path $runRoot "_restore-probe-bin"
        $restoreProbeState = [pscustomobject]@{ SawExpectedSetup = $false }
        $restoreProbeEnvironment = @{}
        $restoreProbeEnvironment[$restoreProbeName] = "temporary-step-value"
        $restoreProbeAction = {
            $expectedPath = $restoreProbeDirectory + [IO.Path]::PathSeparator + $restorePathBefore
            if ([Environment]::GetEnvironmentVariable($restoreProbeName, "Process") -ne "temporary-step-value" -or
                [Environment]::GetEnvironmentVariable("PATH", "Process") -ne $expectedPath) {
                throw "SOURCE_CANDIDATE_RESTORE_PROBE_SETUP_FAILED"
            }
            $restoreProbeState.SawExpectedSetup = $true
        }
        $restoreProbePostAction = { throw "SOURCE_CANDIDATE_POST_VERIFY_RESTORE_PROBE" }
        try {
            Invoke-WithRestoredStepEnvironment $restoreProbeEnvironment $restoreProbeAction $restoreProbePostAction $restoreProbeDirectory
            throw "SOURCE_CANDIDATE_RESTORE_PROBE_DID_NOT_FAIL"
        } catch {
            if ($_.Exception.Message -notmatch "SOURCE_CANDIDATE_POST_VERIFY_RESTORE_PROBE") {
                throw "Source-candidate post-verification restoration probe failed."
            }
        }
        $restoreEnvironmentMatches = [string][Environment]::GetEnvironmentVariable($restoreProbeName, "Process") -eq [string]$restoreProbePrevious
        $restorePathMatches = [Environment]::GetEnvironmentVariable("PATH", "Process") -eq $restorePathBefore
        if (-not $restoreProbeState.SawExpectedSetup -or -not $restoreEnvironmentMatches -or -not $restorePathMatches) {
            throw "Source-candidate restoration probe failed (setup=$($restoreProbeState.SawExpectedSetup) environment=$restoreEnvironmentMatches path=$restorePathMatches)."
        }
        Write-Host "[test-local] plan: $planPath"
        Write-Host "[test-local] steps: $($commands.Name -join ', ')"
        Write-Host "[test-local] tracked tests: $($trackedTestFiles.Count)"
        Write-Host "[OK] Repository-local test runtime and canonical plan contract passed"
        return
    }

    $candidateRequired = @($commands | Where-Object { $_.SourceConsumer -eq "candidate" }).Count -gt 0
    if ($candidateRequired) {
        foreach ($name in $candidateEnvironmentNames) {
            $sourceCandidatePrevious[$name] = [Environment]::GetEnvironmentVariable($name, "Process")
        }
        $sourceCandidateEnvironmentManaged = $true
        $candidateValues = @($candidateEnvironmentNames | ForEach-Object {
            [Environment]::GetEnvironmentVariable($_, "Process")
        })
        $configuredValues = @($candidateValues | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }).Count
        if ($configuredValues -eq 0) {
            $sourceCandidateWorkspace = Join-Path $runRoot "source-work"
            $sourceCandidateID = Invoke-SourceCandidateCommand "capture" $sourceCandidateWorkspace "step-runner-capture"
            Set-SourceCandidateEnvironment $sourceCandidateWorkspace $sourceCandidateID
        } elseif ($configuredValues -eq $candidateEnvironmentNames.Count) {
            $sourceCandidateWorkspace = [Environment]::GetEnvironmentVariable("RENCROW_SOURCE_CANDIDATE_WORKSPACE", "Process")
            $sourceCandidateID = [Environment]::GetEnvironmentVariable("RENCROW_SOURCE_CANDIDATE_ID", "Process")
        } else {
            throw "SOURCE_CANDIDATE_FAILED: incomplete inherited candidate identity"
        }
        Write-Host "[test-local] source candidate: status=candidate candidateId=$sourceCandidateID"
    }

    foreach ($command in $commands) {
        $workingPath = Resolve-WorkingPath $command.WorkingDirectory
        if ($command.SourceConsumer -eq "candidate") {
            [void](Assert-SourceCandidateEnvironment $sourceCandidateID ("pre-{0}" -f $command.Name))
            $sourceCandidateRoot = [Environment]::GetEnvironmentVariable("RENCROW_SOURCE_CANDIDATE_ROOT", "Process")
            $workingPath = [IO.Path]::GetFullPath((Join-Path $sourceCandidateRoot $command.WorkingDirectory))
            $candidateRootFull = [IO.Path]::GetFullPath($sourceCandidateRoot)
            $candidatePrefix = Get-DirectoryPrefix $candidateRootFull
            if (-not $workingPath.StartsWith($candidatePrefix, $comparison) -and
                -not [string]::Equals($workingPath, $candidateRootFull, $comparison)) {
                throw "Canonical step working directory escaped the source candidate."
            }
        }
        $executable = Resolve-TestExecutable $command.FilePath
        $stepPrependPath = $null
        if ([IO.Path]::GetFileNameWithoutExtension($executable) -eq "bash") {
            $bashDirectory = Split-Path -Parent $executable
            $stepPathPrevious = [Environment]::GetEnvironmentVariable("PATH", "Process")
            $pathParts = @($stepPathPrevious -split [Regex]::Escape([IO.Path]::PathSeparator))
            if ($pathParts.Count -eq 0 -or -not $pathParts[0].Equals($bashDirectory, $comparison)) {
                $stepPrependPath = $bashDirectory
            }
        }

# BEGIN GENERATED test-impact:build-output
        # Link output belongs to this invocation, never to the source worktree.
        # The plan remains the command source; only the runtime output directory
        # is supplied by the existing owner isolation boundary.
        if ([IO.Path]::GetFileNameWithoutExtension($executable) -eq "go" -and $command.Arguments.Count -gt 0 -and $command.Arguments[0] -eq "build" -and $command.Arguments -notcontains "-o") {
            $buildOutput = Join-Path $runRoot "build-output"
            New-Item -ItemType Directory -Force -Path $buildOutput | Out-Null
            $remainingArgs = @($command.Arguments | Select-Object -Skip 1)
            $command.Arguments = @("build", "-o", ($buildOutput + [IO.Path]::DirectorySeparatorChar)) + $remainingArgs
        }
# END GENERATED test-impact:build-output

        Write-Host "[test-local] step: $($command.Name)"
        Write-Host "[test-local] command: $executable $($command.Arguments -join ' ')"
        if ($command.SourceConsumer -eq "candidate") {
            Write-Host "[test-local] source candidate: candidateId=$sourceCandidateID"
        }
        $stepAction = {
            Push-Location $workingPath
            try {
            $global:LASTEXITCODE = 0
            & $executable @($command.Arguments)
            if ($LASTEXITCODE -ne 0) {
                throw "Test step '$($command.Name)' failed with exit code ${LASTEXITCODE}: $executable $($command.Arguments -join ' ')"
            }
            } finally {
                Pop-Location
            }
        }
        $stepPostAction = $null
        if ($command.SourceConsumer -eq "candidate") {
            $stepPostAction = {
                [void](Assert-SourceCandidateEnvironment $sourceCandidateID ("post-{0}" -f $command.Name))
            }
        }
        Invoke-WithRestoredStepEnvironment $command.Environment $stepAction $stepPostAction $stepPrependPath
    }
} finally {
    [Environment]::SetEnvironmentVariable($sentinelName, $previousSentinel, "Process")
    [Environment]::SetEnvironmentVariable("RENCROW_TEST_PYTHON", $previousTestPython, "Process")
    if ($sourceCandidateEnvironmentManaged) {
        foreach ($name in $sourceCandidatePrevious.Keys) {
            [Environment]::SetEnvironmentVariable($name, $sourceCandidatePrevious[$name], "Process")
        }
    }
    foreach ($name in $paths.Keys) {
        [Environment]::SetEnvironmentVariable($name, $previous[$name], "Process")
    }

    if (-not $KeepRuntime -and -not $script:preserveSourceCandidateLogs -and (Test-Path -LiteralPath $runRoot)) {
        $resolvedRunRoot = [IO.Path]::GetFullPath($runRoot)
        $runsPrefix = Get-DirectoryPrefix $runsRoot
        if (-not $resolvedRunRoot.StartsWith($runsPrefix, $comparison)) {
            throw "Refusing to clean a path outside the repository test runtime: $resolvedRunRoot"
        }
        Remove-Item -LiteralPath $resolvedRunRoot -Recurse -Force
    }
}
