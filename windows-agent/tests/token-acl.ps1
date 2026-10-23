# Windows-only acceptance fixture for shared-token provisioning/verification.
#
# This script changes only a temporary directory and, with -RunAsAdministrator,
# one uniquely named temporary SYSTEM scheduled task that is removed in a
# finally block. It never touches the production token path, service, or task
# scheduler entries.
[CmdletBinding()]
param(
    [switch]$RunAsAdministrator,
    [string]$InteractiveUser
)
$ErrorActionPreference = 'Stop'

if ([System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT) {
    throw 'token-acl.ps1 requires Windows PowerShell on Windows.'
}
if ([string]::IsNullOrWhiteSpace($InteractiveUser)) {
    $InteractiveUser = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
}

function New-Sid([string]$Value) {
    return New-Object -TypeName System.Security.Principal.SecurityIdentifier -ArgumentList $Value
}

function Assert-CallerIdentity([string]$User) {
    $caller = [System.Security.Principal.WindowsIdentity]::GetCurrent()
    $target = (New-Object -TypeName System.Security.Principal.NTAccount -ArgumentList $User).Translate(
        [System.Security.Principal.SecurityIdentifier])
    if ($caller.User.Value -ne $target.Value) {
        throw "InteractiveUser '$User' must match the calling identity $($caller.Name); refusing a false identity claim."
    }
    return $caller.User
}

function Set-TestRootAcl([string]$Path, $CallerSid) {
    $acl = New-Object -TypeName System.Security.AccessControl.DirectorySecurity
    $acl.SetAccessRuleProtection($true, $false)
    $inherit = [System.Security.AccessControl.InheritanceFlags]::ContainerInherit -bor [System.Security.AccessControl.InheritanceFlags]::ObjectInherit
    foreach ($sid in @((New-Sid 'S-1-5-18'), (New-Sid 'S-1-5-32-544'), $CallerSid)) {
        $acl.AddAccessRule((New-Object -TypeName System.Security.AccessControl.FileSystemAccessRule -ArgumentList @(
            $sid, [System.Security.AccessControl.FileSystemRights]::FullControl,
            $inherit, [System.Security.AccessControl.PropagationFlags]::None,
            [System.Security.AccessControl.AccessControlType]::Allow)))
    }
    Set-Acl -LiteralPath $Path -AclObject $acl
}

function Set-InvalidFileAcl([string]$Path, $CallerSid, [switch]$BroadUsers, [switch]$DenyUsers) {
    $users = New-Sid 'S-1-5-32-545'
    $acl = New-Object -TypeName System.Security.AccessControl.FileSecurity
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($sid in @((New-Sid 'S-1-5-18'), (New-Sid 'S-1-5-32-544'))) {
        $acl.AddAccessRule((New-Object -TypeName System.Security.AccessControl.FileSystemAccessRule -ArgumentList @(
            $sid, [System.Security.AccessControl.FileSystemRights]::FullControl,
            [System.Security.AccessControl.AccessControlType]::Allow)))
    }
    $acl.AddAccessRule((New-Object -TypeName System.Security.AccessControl.FileSystemAccessRule -ArgumentList @(
        $CallerSid, [System.Security.AccessControl.FileSystemRights]::Read,
        [System.Security.AccessControl.AccessControlType]::Allow)))
    if ($BroadUsers) {
        $acl.AddAccessRule((New-Object -TypeName System.Security.AccessControl.FileSystemAccessRule -ArgumentList @(
            $users, [System.Security.AccessControl.FileSystemRights]::Read,
            [System.Security.AccessControl.AccessControlType]::Allow)))
    }
    if ($DenyUsers) {
        # Keep the fixture readable so this exercises our explicit DENY-ACE
        # rejection, not an earlier kernel denial while loading token bytes.
        $acl.AddAccessRule((New-Object -TypeName System.Security.AccessControl.FileSystemAccessRule -ArgumentList @(
            $users, [System.Security.AccessControl.FileSystemRights]::WriteData,
            [System.Security.AccessControl.AccessControlType]::Deny)))
    }
    Set-Acl -LiteralPath $Path -AclObject $acl
}

function New-TestToken {
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        $bytes = New-Object byte[] 32
        $rng.GetBytes($bytes)
        return ,([System.Text.Encoding]::UTF8.GetBytes([Convert]::ToBase64String($bytes)))
    } finally {
        $rng.Dispose()
    }
}

function Get-Sha256Hex([byte[]]$Bytes) {
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $hash = $sha.ComputeHash($Bytes)
    } finally {
        $sha.Dispose()
    }
    return [System.BitConverter]::ToString($hash).Replace('-', '').ToLowerInvariant()
}

function Assert-VerifyRejects([string]$TokenPath, [string]$PatchPattern, [string]$InstallScript) {
    try {
        & $InstallScript -AuthTokenFile $TokenPath -InteractiveUser $InteractiveUser -VerifyOnly
    } catch {
        if ($_.Exception.Message -notmatch $PatchPattern) {
            throw "VerifyOnly rejected for the wrong reason: $($_.Exception.Message)"
        }
        Write-Host "PASS: VerifyOnly rejected $TokenPath ($($_.Exception.Message))"
        return
    }
    throw "VerifyOnly accepted an ACL it must reject: $TokenPath"
}

function Invoke-SystemRead([string]$TokenPath, [string]$TaskDir, [string]$ExpectedHash) {
    $shell = Join-Path $PSHOME 'pwsh.exe'
    if (-not (Test-Path -LiteralPath $shell)) {
        $shell = Join-Path $PSHOME 'powershell.exe'
    }
    if (-not (Test-Path -LiteralPath $shell)) {
        throw "Cannot find the current PowerShell host under $PSHOME for the SYSTEM task."
    }
    $probe = Join-Path $TaskDir 'read-token.ps1'
    $output = Join-Path $TaskDir 'system-read.txt'
    $probeContent = @'
param([string]$TokenPath, [string]$OutPath)
$bytes = [System.IO.File]::ReadAllBytes($TokenPath)
$sha = [System.Security.Cryptography.SHA256]::Create()
try { $hash = $sha.ComputeHash($bytes) } finally { $sha.Dispose() }
[System.IO.File]::WriteAllText($OutPath + '.tmp', [System.BitConverter]::ToString($hash).Replace('-', '').ToLowerInvariant(), [System.Text.UTF8Encoding]::new($false))
[System.IO.File]::Move($OutPath + '.tmp', $OutPath)
'@
    [System.IO.File]::WriteAllText($probe, $probeContent, [System.Text.UTF8Encoding]::new($false))
    $taskName = 'rdp-launcher-acl-test-' + [guid]::NewGuid().ToString('N')
    $argument = '-NoProfile -NonInteractive -ExecutionPolicy Bypass -File "{0}" "{1}" "{2}"' -f $probe, $TokenPath, $output
    $action = New-ScheduledTaskAction -Execute $shell -Argument $argument
    $principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
    try {
        Register-ScheduledTask -TaskName $taskName -Action $action -Principal $principal -Force | Out-Null
        Start-ScheduledTask -TaskName $taskName
        $deadline = (Get-Date).AddSeconds(60)
        while (-not (Test-Path -LiteralPath $output)) {
            if ((Get-Date) -gt $deadline) {
                $info = Get-ScheduledTaskInfo -TaskName $taskName
                throw "SYSTEM task produced no output (LastTaskResult=$($info.LastTaskResult))."
            }
            Start-Sleep -Milliseconds 250
        }
        $actual = ([System.IO.File]::ReadAllText($output)).Trim().ToLowerInvariant()
        if ($actual -ne $ExpectedHash) {
            throw 'SYSTEM read hash did not match the provisioned token.'
        }
        Write-Host 'PASS: temporary SYSTEM task read the provisioned token'
    } finally {
        Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
    }
}

$fixture = Join-Path ([System.IO.Path]::GetTempPath()) ('rdp-launcher-acl-test-' + [guid]::NewGuid().ToString('N'))
$created = $false
try {
    $callerSid = Assert-CallerIdentity $InteractiveUser
    $installScript = (Resolve-Path (Join-Path $PSScriptRoot '..\install.ps1')).Path

    New-Item -ItemType Directory -Path $fixture | Out-Null
    $created = $true
    Set-TestRootAcl $fixture $callerSid

    # Harmless existing executable: copied as install.ps1's SourceExe and never run.
    $sourceExe = Join-Path $fixture 'source.exe'
    Copy-Item -LiteralPath $env:ComSpec -Destination $sourceExe

    $tokenDir = Join-Path $fixture 'token'
    $tokenSource = Join-Path $fixture 'token-source'
    $tokenBytes = New-TestToken
    [System.IO.File]::WriteAllBytes($tokenSource, $tokenBytes)
    $authTokenFile = Join-Path $tokenDir 'agent-token'
    $installDir = Join-Path $fixture 'bin'

    & $installScript -InstallDir $installDir -SourceExe $sourceExe `
        -AuthTokenFile $authTokenFile -InteractiveUser $InteractiveUser -TokenSource $tokenSource
    if (-not (Test-Path -LiteralPath $authTokenFile -PathType Leaf)) {
        throw "install.ps1 did not create $authTokenFile"
    }

    # Actual read as the calling interactive user (the caller identity was
    # checked above, so this is not an assumed-identity result).
    $readBack = [System.IO.File]::ReadAllBytes($authTokenFile)
    if ([Convert]::ToBase64String($readBack) -ne [Convert]::ToBase64String($tokenBytes)) {
        throw 'Calling user could not read the exact provisioned token.'
    }
    Write-Host "PASS: calling user $InteractiveUser read the provisioned token"

    & $installScript -AuthTokenFile $authTokenFile -InteractiveUser $InteractiveUser -VerifyOnly
    Write-Host 'PASS: VerifyOnly accepted the provisioned token ACL'

    $broadDir = Join-Path $fixture 'broad'
    New-Item -ItemType Directory -Path $broadDir | Out-Null
    $broadFile = Join-Path $broadDir 'agent-token'
    Copy-Item -LiteralPath $authTokenFile -Destination $broadFile
    Set-InvalidFileAcl $broadFile $callerSid -BroadUsers
    Assert-VerifyRejects $broadFile 'unexpected trustee|broad-readable' $installScript

    $denyDir = Join-Path $fixture 'deny'
    New-Item -ItemType Directory -Path $denyDir | Out-Null
    $denyFile = Join-Path $denyDir 'agent-token'
    Copy-Item -LiteralPath $authTokenFile -Destination $denyFile
    Set-InvalidFileAcl $denyFile $callerSid -DenyUsers
    Assert-VerifyRejects $denyFile 'unsupported deny ACE' $installScript

    if ($RunAsAdministrator) {
        $principal = New-Object -TypeName System.Security.Principal.WindowsPrincipal -ArgumentList (
            [System.Security.Principal.WindowsIdentity]::GetCurrent())
        if (-not $principal.IsInRole([System.Security.Principal.WindowsBuiltInRole]::Administrator)) {
            throw '-RunAsAdministrator requires an elevated Windows PowerShell.'
        }
        $taskDir = Join-Path $fixture 'system-task'
        New-Item -ItemType Directory -Path $taskDir | Out-Null
        Invoke-SystemRead $authTokenFile $taskDir (Get-Sha256Hex $tokenBytes)
    } else {
        Write-Host 'SKIP: SYSTEM scheduled-task read (rerun elevated with -RunAsAdministrator).'
    }

    Write-Host 'PASS: token ACL acceptance fixture completed'
} finally {
    if ($created -and (Test-Path -LiteralPath $fixture)) {
        try {
            Remove-Item -LiteralPath $fixture -Recurse -Force -ErrorAction Stop
        } catch {
            & icacls.exe $fixture /reset /T /C /Q 2>$null | Out-Null
            Remove-Item -LiteralPath $fixture -Recurse -Force -ErrorAction SilentlyContinue
        }
        if (Test-Path -LiteralPath $fixture) {
            Write-Warning "Could not completely remove temporary fixture: $fixture"
        }
    }
}
